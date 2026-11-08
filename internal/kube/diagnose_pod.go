package kube

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const terminatingGrace = 5 * time.Minute

var imagePullReasons = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true, "ErrImageNeverPull": true, "RegistryUnavailable": true,
}

var containerErrorReasons = map[string]bool{
	"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true,
	"ContainerCannotRun": true, "PreCreateHookError": true, "PostStartHookError": true,
}

// startupEventReasons are warning events of a pod that cannot start.
var startupEventReasons = map[string]bool{
	"FailedMount": true, "FailedAttachVolume": true, "FailedMapVolume": true, "FailedCreatePodSandBox": true,
}

// cause maps phrases found in a free-text message to an inferred cause.
// Matching is case-insensitive and in table order.
type cause struct {
	needles []string
	summary string
}

var pullCauses = []cause{
	{[]string{"repository does not exist or may require"}, "the image does not exist, or the registry requires credentials; check the image name and imagePullSecrets"},
	{[]string{"not found", "manifest unknown", "name unknown", "no such image"}, "the image or tag does not exist in the registry"},
	{[]string{"unauthorized", "authentication required", "pull access denied", "insufficient_scope", "denied"}, "the registry rejected the pull credentials, or none were sent; check imagePullSecrets"},
	{[]string{"no such host", "i/o timeout", "connection refused", "no route to host", "dial tcp", "tls:", "timeout"}, "the registry cannot be reached from the node"},
}

var scheduleCauses = []cause{
	{[]string{"insufficient cpu", "insufficient memory", "insufficient ephemeral-storage"}, "no node has enough free cpu or memory for the pod's requests"},
	{[]string{"untolerated taint", "had taint"}, "every node is tainted in a way the pod does not tolerate"},
	{[]string{"node affinity", "node selector", "nodeselector"}, "no node matches the pod's nodeSelector or affinity"},
	{[]string{"persistentvolumeclaim", "unbound immediate", "volume node affinity", "volume binding"}, "a volume the pod needs is not bound or is not available on any node"},
	{[]string{"too many pods"}, "the nodes are at their pod limit"},
	{[]string{"free ports", "host port"}, "the requested host port is in use on every node"},
	{[]string{"anti-affinity", "pod affinity", "topology spread"}, "pod affinity, anti-affinity or topology spread rules cannot be satisfied"},
}

var evictionCauses = []cause{
	{[]string{"ephemeral-storage", "disk", "storage"}, "the node ran low on disk"},
	{[]string{"memory"}, "the node ran low on memory"},
	{[]string{"pid"}, "the node ran low on process ids"},
}

// matchCauses returns the causes whose needle occurs in msg, with the needle.
func matchCauses(table []cause, msg string, all bool) (out []struct{ Summary, Needle string }) {
	low := strings.ToLower(msg)
	for _, c := range table {
		for _, n := range c.needles {
			if strings.Contains(low, n) {
				out = append(out, struct{ Summary, Needle string }{c.summary, n})
				break
			}
		}
		if len(out) > 0 && !all {
			return out
		}
	}
	return out
}

func newFinding(rule, sev, basis, subject, group, summary string, evidence ...string) finding {
	return finding{Rule: rule, Severity: sev, Basis: basis, Subject: subject, Group: group, Summary: summary, Evidence: evidence}
}

// podRules applies the pod rules to every pod of the snapshot, then the rules
// that need all pods at once.
func (s *snapshot) podRules() []finding {
	var out []finding
	for i := range s.Pods {
		out = append(out, s.podFindings(&s.Pods[i])...)
	}
	return out
}

// unhealthyPods are the names of pods with any finding, sorted.
func (s *snapshot) unhealthyPods() []string {
	var names []string
	for i := range s.Pods {
		if len(s.podFindings(&s.Pods[i])) > 0 {
			names = append(names, s.Pods[i].GetName())
		}
	}
	sort.Strings(names)
	return names
}

func (s *snapshot) podFindings(p *unstructured.Unstructured) []finding {
	subject := "pod/" + p.GetName()
	var out []finding
	for _, cs := range list(p.Object, "status", "initContainerStatuses") {
		out = append(out, s.initFindings(p, subject, asMap(cs))...)
	}
	for _, cs := range list(p.Object, "status", "containerStatuses") {
		out = append(out, s.containerFindings(p, subject, asMap(cs))...)
	}
	out = append(out, s.scheduleFindings(p, subject)...)
	out = append(out, s.evictionFindings(p, subject)...)
	out = append(out, s.podEventFindings(p, subject)...)
	out = append(out, s.terminatingFindings(p, subject)...)
	if len(out) == 0 {
		out = append(out, s.notReadyFindings(p, subject)...)
	}
	return out
}

func (s *snapshot) initFindings(p *unstructured.Unstructured, subject string, cs map[string]any) []finding {
	name := str(cs, "name")
	wait, term := dict(cs, "state", "waiting"), dict(cs, "state", "terminated")
	last := dict(cs, "lastState", "terminated")
	var ev []string
	switch {
	case wait != nil && str(wait, "reason") != "" && str(wait, "reason") != "PodInitializing":
		ev = append(ev, s.text(fmt.Sprintf("init container %s is waiting: %s %s", name, str(wait, "reason"), str(wait, "message"))))
		if last != nil {
			ev = append(ev, "last termination: "+termText(last))
		}
	case term != nil && num(term, "exitCode") != 0:
		ev = append(ev, fmt.Sprintf("init container %s terminated: %s", name, termText(term)))
	default:
		return nil
	}
	return []finding{newFinding("init-failure", sevCritical, basisObserved, subject, name, "init container "+name+" is failing, so the pod cannot start", ev...)}
}

func (s *snapshot) containerFindings(p *unstructured.Unstructured, subject string, cs map[string]any) []finding {
	name := str(cs, "name")
	wait := dict(cs, "state", "waiting")
	cur, last := dict(cs, "state", "terminated"), dict(cs, "lastState", "terminated")
	running := dict(cs, "state", "running") != nil
	restarts := num(cs, "restartCount")
	reason := str(wait, "reason")
	image := specContainerField(p, name, "image")
	var out []finding

	switch {
	case imagePullReasons[reason]:
		msg := str(wait, "message")
		out = append(out, newFinding("image-pull", sevCritical, basisObserved, subject, name+"/"+reason,
			fmt.Sprintf("container %s cannot pull its image (%s)", name, reason),
			"image: "+s.text(image), s.text(fmt.Sprintf("waiting: %s: %s", reason, msg))))
		if reason == "InvalidImageName" {
			out = append(out, newFinding("image-pull-cause", sevCritical, basisInferred, subject, name+"/invalid",
				"the image reference is not valid", "waiting reason is InvalidImageName"))
		} else if m := matchCauses(pullCauses, msg, false); len(m) > 0 {
			out = append(out, newFinding("image-pull-cause", sevCritical, basisInferred, subject, name+"/"+m[0].Summary,
				m[0].Summary, fmt.Sprintf("matched %q in the waiting message", m[0].Needle)))
		}
	case containerErrorReasons[reason]:
		out = append(out, newFinding("container-error", sevCritical, basisObserved, subject, name+"/"+reason,
			fmt.Sprintf("container %s cannot be started (%s)", name, reason), s.text(fmt.Sprintf("waiting: %s: %s", reason, str(wait, "message")))))
	case reason == "CrashLoopBackOff", last != nil && num(last, "exitCode") != 0 && restarts > 0 && !running:
		ev := []string{fmt.Sprintf("restarts: %d", restarts)}
		if last != nil {
			ev = append(ev, "last termination: "+termText(last))
		}
		out = append(out, newFinding("crash-loop", sevCritical, basisObserved, subject, name,
			fmt.Sprintf("container %s is crash looping", name), ev...))
	case cur != nil && num(cur, "exitCode") != 0 && str(p.Object, "status", "phase") == "Failed":
		out = append(out, newFinding("container-failed", sevCritical, basisObserved, subject, name,
			fmt.Sprintf("container %s exited with an error", name), "termination: "+termText(cur)))
	case last != nil && restarts > 0 && running:
		out = append(out, newFinding("restarts", sevWarning, basisObserved, subject, name,
			fmt.Sprintf("container %s has restarted", name), fmt.Sprintf("restarts: %d", restarts), "last termination: "+termText(last)))
	}

	if oom := oomTermination(cur, last); oom != nil {
		out = append(out, s.oomFindings(p, subject, name, oom, running)...)
	}
	return out
}

// oomTermination is the OOMKilled termination among the current and the last
// state, if any.
func oomTermination(states ...map[string]any) map[string]any {
	for _, t := range states {
		if t != nil && str(t, "reason") == "OOMKilled" {
			return t
		}
	}
	return nil
}

func (s *snapshot) oomFindings(p *unstructured.Unstructured, subject, name string, term map[string]any, running bool) []finding {
	limit := specContainerResource(p, name, "limits", "memory")
	sev := sevCritical
	if running {
		sev = sevWarning
	}
	ev := []string{"termination: " + termText(term)}
	if limit != "" {
		ev = append(ev, "memory limit: "+limit)
	} else {
		ev = append(ev, "memory limit: none set")
	}
	out := []finding{newFinding("oom-killed", sev, basisObserved, subject, name,
		fmt.Sprintf("container %s was killed for running out of memory", name), ev...)}
	summary := "the container ran out of memory under node memory pressure; it has no memory limit"
	if limit != "" {
		summary = fmt.Sprintf("memory use reached the container limit %s; raise resources.limits.memory or reduce usage", limit)
	}
	return append(out, newFinding("oom-cause", sevWarning, basisInferred, subject, name+"/"+limit,
		summary, "termination reason is OOMKilled"))
}

func (s *snapshot) scheduleFindings(p *unstructured.Unstructured, subject string) []finding {
	for _, c := range list(p.Object, "status", "conditions") {
		cm := asMap(c)
		if str(cm, "type") != "PodScheduled" || str(cm, "status") != "False" || str(cm, "reason") != "Unschedulable" {
			continue
		}
		msg := str(cm, "message")
		out := []finding{newFinding("unschedulable", sevCritical, basisObserved, subject, "", "the pod cannot be scheduled onto any node",
			s.text("PodScheduled=False: "+msg))}
		for _, m := range matchCauses(scheduleCauses, msg, true) {
			ev := []string{fmt.Sprintf("matched %q in the scheduler message", m.Needle)}
			if strings.Contains(m.Summary, "cpu or memory") {
				ev = append(ev, "pod requests: "+requestsText(p))
			}
			out = append(out, newFinding("unschedulable-cause", sevCritical, basisInferred, subject, m.Summary, m.Summary, ev...))
		}
		return out
	}
	return nil
}

func (s *snapshot) evictionFindings(p *unstructured.Unstructured, subject string) []finding {
	if str(p.Object, "status", "reason") != "Evicted" {
		return nil
	}
	msg := str(p.Object, "status", "message")
	out := []finding{newFinding("evicted", sevCritical, basisObserved, subject, "", "the pod was evicted from its node", s.text("status.reason Evicted: "+msg))}
	if m := matchCauses(evictionCauses, msg, false); len(m) > 0 {
		out = append(out, newFinding("evicted-cause", sevWarning, basisInferred, subject, m[0].Summary, m[0].Summary,
			fmt.Sprintf("matched %q in the eviction message", m[0].Needle)))
	}
	return out
}

// podEventFindings reads the warning events about the pod: failed probes and
// failures to start (volume mounts, sandbox creation).
func (s *snapshot) podEventFindings(p *unstructured.Unstructured, subject string) []finding {
	var out []finding
	for _, e := range s.eventsAbout("Pod", p.GetName()) {
		switch {
		case e.Reason == "Unhealthy":
			probe := "readiness"
			low := strings.ToLower(e.Message)
			for _, k := range []string{"liveness", "startup"} {
				if strings.HasPrefix(low, k+" probe") {
					probe = k
				}
			}
			out = append(out, newFinding("probe-failed", sevWarning, basisObserved, subject, probe,
				probe+" probe is failing", s.text(e.Message), eventSeen(&e)))
		case startupEventReasons[e.Reason]:
			out = append(out, newFinding("startup-failed", sevCritical, basisObserved, subject, e.Reason,
				"the pod cannot start ("+e.Reason+")", s.text(e.Message), eventSeen(&e)))
		}
	}
	return out
}

func eventSeen(e *corev1.Event) string {
	return fmt.Sprintf("event seen %d times, last %s ago", eventCount(e), age(eventTime(e)))
}

func (s *snapshot) terminatingFindings(p *unstructured.Unstructured, subject string) []finding {
	ts := p.GetDeletionTimestamp()
	if ts == nil || now().Sub(ts.Time) < terminatingGrace {
		return nil
	}
	ev := []string{"deletion requested " + age(ts.Time) + " ago"}
	if f := p.GetFinalizers(); len(f) > 0 {
		ev = append(ev, "finalizers: "+strings.Join(f, ", "))
	}
	return []finding{newFinding("terminating-stuck", sevWarning, basisInferred, subject, "", "the pod has been terminating for longer than its grace period should allow", ev...)}
}

// notReadyFindings explains a running pod that is not Ready when no rule
// found a more specific reason.
func (s *snapshot) notReadyFindings(p *unstructured.Unstructured, subject string) []finding {
	if str(p.Object, "status", "phase") != "Running" {
		return nil
	}
	for _, c := range list(p.Object, "status", "conditions") {
		cm := asMap(c)
		if str(cm, "type") != "Ready" || str(cm, "status") != "False" {
			continue
		}
		ev := []string{s.text(fmt.Sprintf("Ready=False: %s %s", str(cm, "reason"), str(cm, "message")))}
		var names []string
		for _, cs := range list(p.Object, "status", "containerStatuses") {
			if b, _ := asMap(cs)["ready"].(bool); !b {
				names = append(names, str(asMap(cs), "name"))
			}
		}
		if len(names) > 0 {
			ev = append(ev, "containers not ready: "+strings.Join(names, ", "))
		}
		return []finding{newFinding("not-ready", sevWarning, basisObserved, subject, "", "the pod is running but not Ready", ev...)}
	}
	return nil
}

// nodeRules reports the nodes that unhealthy pods run on when the node itself
// is not Ready or is under pressure.
func (s *snapshot) nodeRules() []finding {
	onNode := map[string][]string{}
	for i := range s.Pods {
		if len(s.podFindings(&s.Pods[i])) > 0 {
			n := str(s.Pods[i].Object, "spec", "nodeName")
			onNode[n] = append(onNode[n], s.Pods[i].GetName())
		}
	}
	var out []finding
	for _, name := range sortedKeys(s.Nodes) {
		node := s.Nodes[name]
		var ev []string
		sev := sevWarning
		for _, c := range list(node.Object, "status", "conditions") {
			cm := asMap(c)
			t, st := str(cm, "type"), str(cm, "status")
			switch {
			case t == "Ready" && st != "True":
				sev = sevCritical
			case t != "Ready" && st != "True":
				continue
			case t == "Ready":
				continue
			}
			ev = append(ev, s.text(fmt.Sprintf("%s=%s (%s) %s", t, st, str(cm, "reason"), str(cm, "message"))))
		}
		if len(ev) == 0 {
			continue
		}
		pods := onNode[name]
		sort.Strings(pods)
		ev = append(ev, fmt.Sprintf("%d unhealthy pod(s) on this node: %s", len(pods), strings.Join(pods, ", ")))
		out = append(out, newFinding("node-unhealthy", sev, basisObserved, "node/"+name, "", "the node is not healthy", ev...))
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// termText renders a terminated state: "Error, exit code 1, 3m ago".
func termText(t map[string]any) string {
	var parts []string
	if r := str(t, "reason"); r != "" {
		parts = append(parts, r)
	}
	parts = append(parts, fmt.Sprintf("exit code %d", num(t, "exitCode")))
	if sig := num(t, "signal"); sig != 0 {
		parts = append(parts, fmt.Sprintf("signal %d", sig))
	}
	if at, err := time.Parse(time.RFC3339, str(t, "finishedAt")); err == nil {
		parts = append(parts, age(at)+" ago")
	}
	return strings.Join(parts, ", ")
}

func specContainer(p *unstructured.Unstructured, name string) map[string]any {
	for _, key := range []string{"containers", "initContainers"} {
		for _, c := range list(p.Object, "spec", key) {
			if cm := asMap(c); str(cm, "name") == name {
				return cm
			}
		}
	}
	return nil
}

func specContainerField(p *unstructured.Unstructured, name, field string) string {
	return str(specContainer(p, name), field)
}

func specContainerResource(p *unstructured.Unstructured, name, kind, res string) string {
	return str(specContainer(p, name), "resources", kind, res)
}

// requestsText sums the cpu and memory requests of the pod's containers.
func requestsText(p *unstructured.Unstructured) string {
	var cpu, mem resource.Quantity
	for _, c := range list(p.Object, "spec", "containers") {
		for res, total := range map[string]*resource.Quantity{"cpu": &cpu, "memory": &mem} {
			if v := str(asMap(c), "resources", "requests", res); v != "" {
				if q, err := resource.ParseQuantity(v); err == nil {
					total.Add(q)
				}
			}
		}
	}
	return fmt.Sprintf("cpu=%s memory=%s", orDefault(cpu.String(), "0"), orDefault(mem.String(), "0"))
}
