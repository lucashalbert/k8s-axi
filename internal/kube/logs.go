package kube

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
	"github.com/lucashalbert/axi-sdk-go/redact"
)

const (
	defaultTail     = 100
	maxTailLines    = 10000
	defaultMaxSize  = 64 * 1024 // total log bytes in one response
	minStreamBytes  = 1024
	maxLogPods      = 5
	logPodListLimit = 500
	// maxReadBytes caps what is read from one container, so a container with
	// very long lines cannot make the command read without bound.
	maxReadBytes = 4 << 20
)

// podsResource is core/v1 pods, which logs reads without discovery.
var podsResource = Resource{GVR: gvr("", "v1", "pods"), Kind: "Pod", Namespaced: true}

// podLog opens one container's log stream. Tests replace it.
var podLog = func(ctx context.Context, c *Clients, ns, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
	return c.Typed.CoreV1().Pods(ns).GetLogs(pod, o).Stream(ctx)
}

type logLine struct {
	Line string `json:"line"`
}

type logStream struct {
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Lines     int       `json:"lines"`
	Truncated bool      `json:"truncated"`
	Error     string    `json:"error,omitempty"`
	Log       []logLine `json:"log"`
}

type logsOut struct {
	Target    string      `json:"target"`
	Namespace string      `json:"namespace"`
	Pods      int         `json:"pods"`
	Truncated bool        `json:"truncated"`
	Redacted  int         `json:"redacted"`
	Note      string      `json:"note,omitempty"`
	Streams   []logStream `json:"streams"`
	Next      []string    `json:"next,omitempty"`
}

// logOpts are the flags of the logs command.
type logOpts struct {
	container, selector string
	previous            bool
	tail                int
	since               time.Duration
	maxBytes            int
}

func (g *globals) logsCmd() *cobra.Command {
	var o logOpts
	cmd := &cobra.Command{
		Use:   "logs [TARGET]",
		Short: "Read bounded, redacted logs from a pod or a workload's pods",
		Long: "Read container logs, bounded in lines and bytes, with credential-looking text redacted.\n" +
			"TARGET is a pod name, or KIND/NAME for a pod, deployment, statefulset, daemonset, replicaset or job (its pods are found through its selector).\n" +
			"With no TARGET, -l selects pods directly. Without -c every regular container is read.\n" +
			"The newest --tail lines are returned, cut to --max-bytes in total; at most " + fmt.Sprint(maxLogPods) + " pods are read and the output says when anything was left out.\n" +
			"--tail 0 reads up to " + fmt.Sprint(maxTailLines) + " lines, still within the byte limit.",
		Example: "  k8s-axi logs web-6d5f8 -c app --previous\n  k8s-axi logs deploy/web --since 15m --tail 50\n  k8s-axi logs -l app=web -n prod",
		Args:    argRange(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.validate(len(args)); err != nil {
				return err
			}
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			c, err := g.clients()
			if err != nil {
				return err
			}
			out, err := g.logs(cmd.Context(), c, args, o)
			if err != nil {
				return err
			}
			return output.Render(cmd.OutOrStdout(), out, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&o.container, "container", "c", "", "container to read (default: every regular container)")
	fl.StringVarP(&o.selector, "selector", "l", "", "label selector choosing pods when no TARGET is given")
	fl.BoolVarP(&o.previous, "previous", "p", false, "read the previous, terminated instance of the container")
	fl.IntVar(&o.tail, "tail", defaultTail, "newest lines to return per container; 0 means up to "+fmt.Sprint(maxTailLines))
	fl.DurationVar(&o.since, "since", 0, "only lines newer than this duration, for example 10m")
	fl.IntVar(&o.maxBytes, "max-bytes", defaultMaxSize, "limit on log text in the whole response, shared by all containers")
	return cmd
}

func (o logOpts) validate(args int) error {
	switch {
	case args == 0 && o.selector == "":
		return axierr.Usage("logs", "no target: name a pod or workload, or select pods with -l", "write 'k8s-axi logs POD', 'k8s-axi logs deploy/NAME' or 'k8s-axi logs -l app=web'")
	case args > 0 && o.selector != "":
		return axierr.Usage("logs", "a TARGET cannot be combined with -l", "use one or the other")
	case o.tail < 0 || o.tail > maxTailLines:
		return axierr.Usage("logs", fmt.Sprintf("--tail must be between 0 and %d", maxTailLines), "narrow with --since for older output")
	case o.maxBytes < minStreamBytes:
		return axierr.Usage("logs", fmt.Sprintf("--max-bytes must be at least %d", minStreamBytes), "")
	case o.since < 0:
		return axierr.Usage("logs", "--since must be a positive duration such as 10m", "")
	}
	return nil
}

// logTarget is the set of pods a logs command reads.
type logTarget struct {
	label   string
	pods    []unstructured.Unstructured
	matched int
}

func (g *globals) logs(ctx context.Context, c *Clients, args []string, o logOpts) (logsOut, error) {
	ns := c.Namespace
	tgt, err := c.logPods(ctx, ns, args, o.selector)
	if err != nil {
		return logsOut{}, err
	}
	out := logsOut{Target: tgt.label, Namespace: ns, Pods: tgt.matched, Streams: []logStream{}}
	if len(tgt.pods) == 0 {
		out.Note = "no pods found for " + tgt.label + " in namespace " + ns
		return out, nil
	}
	pods := tgt.pods
	if len(pods) > maxLogPods {
		pods = pods[:maxLogPods]
	}

	type job struct{ pod, container string }
	var jobs []job
	for i := range pods {
		names, err := containerNames(&pods[i], o.container)
		if err != nil {
			if tgt.matched == 1 {
				return out, err
			}
			continue // a selector may match pods without that container
		}
		for _, n := range names {
			jobs = append(jobs, job{pods[i].GetName(), n})
		}
	}
	if len(jobs) == 0 {
		return out, axierr.Usage("logs", fmt.Sprintf("no pod of %s has a container named %q", tgt.label, o.container), "check the names with 'k8s-axi inspect pod NAME'")
	}

	budget := o.maxBytes / len(jobs)
	var failed []string
	for _, j := range jobs {
		s, redacted, err := readStream(ctx, c, ns, j.pod, j.container, o, budget)
		if err != nil {
			failed = append(failed, err.Error())
			s = logStream{Pod: j.pod, Container: j.container, Error: truncate(redactedError(err).Error(), 200), Log: []logLine{}}
		}
		out.Redacted += redacted
		out.Truncated = out.Truncated || s.Truncated
		out.Streams = append(out.Streams, s)
	}
	if len(failed) == len(out.Streams) {
		hint := "check the pod's state with 'k8s-axi inspect pod " + out.Streams[0].Pod + "'"
		if o.previous {
			hint = "the container has not restarted, so there is no previous log; drop --previous"
		}
		return out, axierr.Runtime("logs", "context/"+c.Context+" "+tgt.label, fmt.Errorf("%s", failed[0]), hint)
	}
	out.Note = logsNote(out, tgt.matched, len(pods), o)
	out.Next = g.logsNext(out)
	return out, nil
}

// logPods resolves the pods a logs command reads.
func (c *Clients) logPods(ctx context.Context, ns string, args []string, selector string) (logTarget, error) {
	if selector != "" {
		pods, matched, err := c.listLogPods(ctx, ns, selector)
		if err != nil {
			return logTarget{}, apiError("logs", "context/"+c.Context+" pods"+nsFlag(ns), err, "list", "pods")
		}
		return logTarget{label: "selector " + selector, pods: pods, matched: matched}, nil
	}
	kind, name := "pod", args[0]
	if len(args) == 2 {
		kind, name = args[0], args[1]
	} else if k, n, ok := strings.Cut(args[0], "/"); ok {
		kind, name = k, n
	}
	if name == "" || strings.Contains(name, "/") {
		return logTarget{}, axierr.Usage("logs", fmt.Sprintf("cannot read %q as POD or KIND/NAME", strings.Join(args, " ")), "write 'k8s-axi logs web-1' or 'k8s-axi logs deploy/web'")
	}
	r, err := c.Resolve(kind)
	if err != nil {
		return logTarget{}, err
	}
	if !logsSupported(r) {
		return logTarget{}, axierr.Usage("logs", fmt.Sprintf("logs cannot read %s", r.String()),
			"use a pod, deployment, statefulset, daemonset, replicaset or job, or select pods with -l")
	}
	label := strings.ToLower(r.Kind) + "/" + name
	obj, err := c.Get(ctx, r, ns, name)
	if err != nil {
		return logTarget{}, apiError("logs", "context/"+c.Context+" "+r.String()+"/"+name+nsFlag(ns), err, "get", r.String())
	}
	if r.GVR.Resource == "pods" {
		return logTarget{label: label, pods: []unstructured.Unstructured{*obj}, matched: 1}, nil
	}
	sel := labelSelector(obj.Object, "spec", "selector")
	if sel == "" && r.GVR.Resource == "jobs" {
		sel = "batch.kubernetes.io/job-name=" + name
	}
	if sel == "" {
		return logTarget{}, axierr.Usage("logs", label+" has no pod selector", "select its pods with -l instead")
	}
	pods, matched, err := c.listLogPods(ctx, ns, sel)
	if err != nil {
		return logTarget{}, apiError("logs", "context/"+c.Context+" pods"+nsFlag(ns), err, "list", "pods")
	}
	return logTarget{label: label, pods: pods, matched: matched}, nil
}

func (c *Clients) listLogPods(ctx context.Context, ns, selector string) ([]unstructured.Unstructured, int, error) {
	o := metav1.ListOptions{LabelSelector: selector, Limit: logPodListLimit}
	var pods []unstructured.Unstructured
	matched := 0
	for {
		res, err := c.List(ctx, podsResource, ns, o)
		if err != nil {
			return nil, 0, err
		}
		matched += len(res.Items)
		pods = append(pods, res.Items...)
		pods = sortedByName(pods)
		if len(pods) > maxLogPods {
			pods = pods[:maxLogPods]
		}
		if !res.More {
			return pods, matched, nil
		}
		o.Continue = res.Continue
	}
}

// logsSupported reports whether logs can read resource r.
func logsSupported(r Resource) bool {
	switch {
	case r.GVR.Group == "" && r.GVR.Resource == "pods":
		return true
	case r.GVR.Group == "apps":
		return r.GVR.Resource == "deployments" || r.GVR.Resource == "statefulsets" || r.GVR.Resource == "daemonsets" || r.GVR.Resource == "replicasets"
	case r.GVR.Group == "batch":
		return r.GVR.Resource == "jobs"
	}
	return false
}

func sortedByName(items []unstructured.Unstructured) []unstructured.Unstructured {
	sort.Slice(items, func(i, j int) bool { return items[i].GetName() < items[j].GetName() })
	return items
}

// containerNames returns the containers to read in pod: the named one (a
// regular or init container), else every regular container.
func containerNames(pod *unstructured.Unstructured, want string) ([]string, error) {
	var regular, init []string
	for _, c := range list(pod.Object, "spec", "containers") {
		regular = append(regular, str(asMap(c), "name"))
	}
	for _, c := range list(pod.Object, "spec", "initContainers") {
		init = append(init, str(asMap(c), "name"))
	}
	if want == "" {
		return regular, nil
	}
	for _, n := range append(append([]string{}, regular...), init...) {
		if n == want {
			return []string{want}, nil
		}
	}
	return nil, axierr.Usage("logs", fmt.Sprintf("pod %s has no container named %q; it has: %s", pod.GetName(), want, strings.Join(append(regular, init...), ", ")),
		"pass one of them with -c")
}

// readStream reads one container's log and shapes it: redacted, then cut to
// the newest o.tail lines and the byte budget. The int result counts redactions.
func readStream(ctx context.Context, c *Clients, ns, pod, container string, o logOpts, budget int) (logStream, int, error) {
	tail := o.tail
	if tail == 0 {
		tail = maxTailLines
	}
	opts := &corev1.PodLogOptions{Container: container, Previous: o.previous, TailLines: ptr.To(int64(tail) + 1), LimitBytes: ptr.To(int64(maxReadBytes))}
	if o.since > 0 {
		opts.SinceSeconds = ptr.To(int64(o.since / time.Second))
	}
	rc, err := podLog(ctx, c, ns, pod, opts)
	if err != nil {
		return logStream{}, 0, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return logStream{}, 0, err
	}
	lines, truncated, redacted := shapeLog(string(raw), tail, budget)
	if len(raw) >= maxReadBytes {
		truncated = true // the read limit cut the newest lines
	}
	s := logStream{Pod: pod, Container: container, Lines: len(lines), Truncated: truncated, Log: make([]logLine, len(lines))}
	for i, l := range lines {
		s.Log[i] = logLine{l}
	}
	return s, redacted, nil
}

// shapeLog redacts raw as a whole, so multi-line secrets such as private keys
// are caught, then keeps the newest tail lines that fit in budget bytes.
// truncated is true when older lines were left out. A single line longer than
// the budget is cut.
func shapeLog(raw string, tail, budget int) (lines []string, truncated bool, redacted int) {
	masked := redact.Mask(raw)
	redacted = strings.Count(masked, redact.Placeholder) - strings.Count(raw, redact.Placeholder)
	masked = strings.TrimRight(masked, "\r\n")
	if masked == "" {
		return nil, false, redacted
	}
	lines = strings.Split(strings.ReplaceAll(masked, "\r\n", "\n"), "\n")
	if len(lines) > tail {
		lines, truncated = lines[len(lines)-tail:], true
	}
	used, keep := 0, len(lines)
	for keep > 0 && used+len(lines[keep-1])+1 <= budget {
		keep--
		used += len(lines[keep]) + 1
	}
	if keep > 0 {
		truncated = true
		if keep == len(lines) { // even the newest line is over budget
			lines = []string{truncateBytes(lines[len(lines)-1], budget)}
			return lines, true, redacted
		}
		lines = lines[keep:]
	}
	return lines, truncated, redacted
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ellipsis = "…"
	if n < len(ellipsis) {
		return ""
	}
	if n == len(ellipsis) {
		return ellipsis
	}
	maxPrefix := n - len(ellipsis)
	limit := 0
	for i := range s {
		if i > maxPrefix {
			break
		}
		limit = i
	}
	return s[:limit] + ellipsis
}

func logsNote(out logsOut, matched, shown int, o logOpts) string {
	var notes []string
	if matched > shown {
		notes = append(notes, fmt.Sprintf("showing %d of %d pods (first by name); name a pod or narrow with -l to see others", shown, matched))
	}
	if logsWereCut(out) {
		notes = append(notes, fmt.Sprintf("older lines were left out (--tail %d, --max-bytes %d); raise them or narrow with --since", o.tail, o.maxBytes))
	}
	total := 0
	for _, s := range out.Streams {
		total += s.Lines
	}
	if total == 0 {
		notes = append(notes, "no log lines in the selected window; the container may not have written any, or --since excluded them")
	}
	for _, s := range out.Streams {
		if s.Error != "" {
			notes = append(notes, fmt.Sprintf("%s/%s could not be read: %s", s.Pod, s.Container, s.Error))
		}
	}
	if out.Redacted > 0 {
		notes = append(notes, fmt.Sprintf("%d credential-looking value(s) were redacted", out.Redacted))
	}
	return strings.Join(notes, "; ")
}

func logsWereCut(out logsOut) bool {
	for _, s := range out.Streams {
		if s.Truncated {
			return true
		}
	}
	return false
}

func (g *globals) logsNext(out logsOut) []string {
	if len(out.Streams) == 0 {
		return nil
	}
	s := out.Streams[0]
	return []string{g.cmdline("inspect", "pods", s.Pod) + nsFlag(out.Namespace)}
}
