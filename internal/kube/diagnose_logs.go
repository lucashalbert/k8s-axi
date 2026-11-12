package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// logSel names one container log to read for a diagnosis.
type logSel struct {
	pod, container string
	previous       bool
	prio           int // lower first: failing before merely not ready
}

// logCandidates picks the container logs worth reading: containers that
// crash or exit with an error (their previous instance when restarted) and
// containers that run without becoming ready. One pod per container name and
// instance, at most max in total.
func logCandidates(s *snapshot, max int) []logSel {
	var all []logSel
	for i := range s.Pods {
		p := &s.Pods[i]
		for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
			for _, c := range list(p.Object, "status", key) {
				if sel, ok := logSelFor(p.GetName(), asMap(c)); ok {
					all = append(all, sel)
				}
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.prio != b.prio {
			return a.prio < b.prio
		}
		if a.pod != b.pod {
			return a.pod < b.pod
		}
		return a.container < b.container
	})
	var out []logSel
	seen := map[string]bool{}
	for _, sel := range all {
		k := fmt.Sprintf("%s/%t", sel.container, sel.previous)
		if seen[k] || len(out) >= max {
			continue
		}
		seen[k] = true
		out = append(out, sel)
	}
	return out
}

func logSelFor(pod string, cs map[string]any) (logSel, bool) {
	name := str(cs, "name")
	cur, last := dict(cs, "state", "terminated"), dict(cs, "lastState", "terminated")
	running := dict(cs, "state", "running") != nil
	// A terminated state is the latest instance, read without --previous;
	// lastState is then an older one the kubelet has already removed.
	switch {
	case str(dict(cs, "state", "waiting"), "reason") == "CrashLoopBackOff", cur == nil && last != nil && num(last, "exitCode") != 0 && num(cs, "restartCount") > 0 && !running:
		return logSel{pod, name, true, 0}, true
	case cur != nil && num(cur, "exitCode") != 0:
		return logSel{pod, name, false, 0}, true
	case cur == nil && last != nil && num(last, "exitCode") != 0 && num(cs, "restartCount") > 0:
		return logSel{pod, name, true, 1}, true
	case running && cs["ready"] == false:
		return logSel{pod, name, false, 2}, true
	}
	return logSel{}, false
}

// readLogs reads the candidate logs concurrently through the same bounded,
// redacting reader the logs command uses. A log that cannot be read becomes
// a gap. The int result counts redactions.
func (g *globals) readLogs(ctx context.Context, c *Clients, s *snapshot, gp *gaps) ([]diagLog, int) {
	maxStreams, tail, budget := diagStreams, diagTail, diagBytes
	if g.full {
		maxStreams, tail, budget = diagStreamsFull, diagTailFull, diagBytesFull
	}
	cands := logCandidates(s, maxStreams)
	results := make([]*diagLog, len(cands))
	var mu sync.Mutex
	redacted := 0
	fns := make([]func(context.Context), len(cands))
	ns := s.Target.GetNamespace()
	for i, sel := range cands {
		fns[i] = func(ctx context.Context) {
			st, n, err := readStream(ctx, c, ns, sel.pod, sel.container, logOpts{tail: tail, previous: sel.previous}, budget)
			if err != nil {
				what := "logs " + sel.pod + "/" + sel.container
				if sel.previous {
					what += " (previous)"
				}
				gp.add(what, "get", "pods/log", err)
				return
			}
			if kubeletLogUnavailable(st) {
				what := "logs " + sel.pod + "/" + sel.container
				if sel.previous {
					what += " (previous)"
				}
				gp.raw(what + ": the kubelet could not return the log yet")
				return
			}
			mu.Lock()
			redacted += n
			mu.Unlock()
			results[i] = &diagLog{Pod: sel.pod, Container: sel.container, Previous: sel.previous, Lines: st.Lines, Truncated: st.Truncated, Log: st.Log}
		}
	}
	fanout(ctx, diagLogConcurrent, fns...)
	var out []diagLog
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, redacted
}

func kubeletLogUnavailable(st logStream) bool {
	return st.Lines == 1 && len(st.Log) == 1 && strings.HasPrefix(st.Log[0].Line, "unable to retrieve container logs")
}

// diagnoseNext suggests at most four follow-up commands.
func (g *globals) diagnoseNext(s *snapshot, fs []finding, logs []diagLog) []string {
	ns := s.Target.GetNamespace()
	var out []string
	add := func(cmd string) {
		if len(out) >= 4 {
			return
		}
		for _, o := range out {
			if o == cmd {
				return
			}
		}
		out = append(out, cmd)
	}
	if len(logs) > 0 {
		l := logs[0]
		parts := []string{"logs", l.Pod, "-c", l.Container}
		if l.Previous {
			parts = append(parts, "--previous")
		}
		add(g.cmdline(parts...) + nsFlag(ns))
	}
	if s.Resource.GVR.Resource != "pods" {
		if bad := s.unhealthyPods(); len(bad) > 0 {
			add(g.cmdline("inspect", "pods", bad[0]) + nsFlag(ns))
		}
	}
	for _, f := range fs {
		switch f.Rule {
		case "quota-exceeded":
			add(g.cmdline("get", "resourcequotas") + nsFlag(ns))
		case "service-no-pods", "service-selector-mismatch":
			add(g.cmdline("get", "pods") + nsFlag(ns))
		case "node-unhealthy":
			add(g.cmdline("inspect", "nodes", trimKind(f.Subject)))
		}
	}
	add(g.cmdline("events", s.Resource.String(), s.Target.GetName()) + nsFlag(ns))
	return out
}

// trimKind drops the "kind/" prefix of a subject and any merge suffix.
func trimKind(subject string) string {
	for i := 0; i < len(subject); i++ {
		if subject[i] == '/' {
			subject = subject[i+1:]
			break
		}
	}
	for i := 0; i < len(subject); i++ {
		if subject[i] == ',' || subject[i] == ' ' {
			return subject[:i]
		}
	}
	return subject
}
