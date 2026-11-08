package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
)

// Bounds on what one diagnose reads. Every list is one page.
const (
	diagPodLimit      = 100 // pods analysed
	diagReplicaSets   = 100
	diagNSPods        = 200 // pods read to explain a selector mismatch
	diagQuotas        = 50
	diagSlices        = 100
	diagNodes         = 3
	diagConcurrency   = 4
	diagLogConcurrent = 3
	diagPodRows       = 10 // pod rows shown in the compact view
	diagEvents        = 5  // warning events shown

	diagTail, diagBytes          = 20, 4 << 10 // lines and bytes per log stream
	diagTailFull, diagBytesFull  = 100, 16 << 10
	diagStreams, diagStreamsFull = 3, 5
	diagDeadlineFactor           = 3 // the whole command stops after this many --timeout
)

// diagnosable are the kinds diagnose accepts, as "group/resource".
var diagnosable = map[string]bool{
	"/pods": true, "/services": true, "apps/deployments": true, "apps/statefulsets": true, "apps/daemonsets": true, "batch/jobs": true,
}

const diagnosableList = "pods, deployments, statefulsets, daemonsets, jobs and services"

var (
	quotasResource = Resource{GVR: gvr("", "v1", "resourcequotas"), Kind: "ResourceQuota", Namespaced: true}
	slicesResource = Resource{GVR: gvr("discovery.k8s.io", "v1", "endpointslices"), Kind: "EndpointSlice", Namespaced: true}
	replicaSetsRes = Resource{GVR: gvr("apps", "v1", "replicasets"), Kind: "ReplicaSet", Namespaced: true}
)

type diagPods struct {
	Total     int    `json:"total"`
	Shown     int    `json:"shown"`
	Unhealthy int    `json:"unhealthy"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note,omitempty"`
	Items     []any  `json:"items"`
}

type diagLog struct {
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Previous  bool      `json:"previous"`
	Lines     int       `json:"lines"`
	Truncated bool      `json:"truncated"`
	Log       []logLine `json:"log"`
}

type diagnoseOut struct {
	Target     string         `json:"target"`
	Kind       string         `json:"kind"`
	Namespace  string         `json:"namespace,omitempty"`
	Context    string         `json:"context"`
	Status     string         `json:"status"`
	Conclusion string         `json:"conclusion"`
	Details    any            `json:"details"`
	Conditions []conditionRow `json:"conditions"`
	Owners     []ownerRow     `json:"owners"`
	Count      int            `json:"count"`
	Findings   []finding      `json:"findings"`
	Pods       *diagPods      `json:"pods,omitempty"`
	Events     relatedEvents  `json:"events"`
	Logs       []diagLog      `json:"logs"`
	Redacted   int            `json:"redacted,omitempty"`
	Gaps       []string       `json:"gaps"`
	Checked    []string       `json:"checked"`
	Object     map[string]any `json:"object,omitempty"`
	Next       []string       `json:"next,omitempty"`
}

func (g *globals) diagnoseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diagnose KIND NAME",
		Short: "Find out why a workload or service is unhealthy, with ranked, evidenced findings",
		Long: "Diagnose a pod, deployment, statefulset, daemonset, job or service.\n" +
			"It reads the object, its owners and pods, the newest warning events and a few bounded, redacted log tails,\n" +
			"applies fixed rules, and prints ranked findings with a short conclusion and next-step commands.\n" +
			"Each finding is 'observed' (read directly from the cluster) or 'inferred' (derived by a rule) and lists its evidence.\n" +
			"Checks that could not run, for example for lack of permission, are listed under 'gaps'.\n" +
			"The command only reads. It exits 0 whenever it produced a report, healthy or not.",
		Example: "  k8s-axi diagnose deploy/web -n prod\n  k8s-axi diagnose pod web-6d5f8\n  k8s-axi diagnose svc api --full",
		Args:    argRange(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := splitTarget(args)
			if err != nil {
				return err
			}
			if name == "" {
				return axierr.Usage("diagnose", "a target needs a name: KIND NAME or KIND/NAME", "write 'k8s-axi diagnose deploy web' or 'k8s-axi diagnose deploy/web'")
			}
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			c, err := g.clients()
			if err != nil {
				return err
			}
			r, err := c.Resolve(kind)
			if err != nil {
				return err
			}
			if !diagnosable[r.GVR.Group+"/"+r.GVR.Resource] {
				return axierr.Usage("diagnose", fmt.Sprintf("diagnose cannot analyse %s", r.String()),
					"diagnose supports "+diagnosableList+"; use 'k8s-axi inspect' for other kinds")
			}
			ctx := cmd.Context()
			if g.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, g.timeout*diagDeadlineFactor)
				defer cancel()
			}
			u, err := c.Get(ctx, r, c.Namespace, name)
			if err != nil {
				return apiError("diagnose", "context/"+c.Context+" "+r.String()+"/"+name+nsFlag(c.Namespace), err, "get", r.String())
			}
			return output.Render(cmd.OutOrStdout(), g.diagnose(ctx, c, r, u), f)
		},
	}
}

// gaps collects the checks that could not run.
type gaps struct {
	mu   sync.Mutex
	list []string
}

func (gp *gaps) add(what, verb, res string, err error) {
	reason := reasonOf(err)
	if reason == "forbidden" {
		reason += fmt.Sprintf(" (RBAC denied '%s' on %s)", verb, res)
	}
	gp.mu.Lock()
	gp.list = append(gp.list, what+": "+reason)
	gp.mu.Unlock()
}

func (gp *gaps) raw(msg string) {
	gp.mu.Lock()
	gp.list = append(gp.list, msg)
	gp.mu.Unlock()
}

func (gp *gaps) sorted() []string {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	out := append([]string{}, gp.list...)
	sort.Strings(out)
	return out
}

// fanout runs fns concurrently, at most limit at a time, and waits for all.
func fanout(ctx context.Context, limit int, fns ...func(context.Context)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, fn := range fns {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(ctx)
		}()
	}
	wg.Wait()
}

// bundle is the evidence one diagnose gathered, before the rules run.
type bundle struct {
	snap     *snapshot
	owners   []ownerRow
	allPods  int // pods the list returned
	podsMore bool
	events   int // warning events matched to the target
	logs     []diagLog
	redacted int
	slices   int
	nodes    int
	quotas   int
	gaps     *gaps
}

func (g *globals) diagnose(ctx context.Context, c *Clients, r Resource, u *unstructured.Unstructured) diagnoseOut {
	b := g.gather(ctx, c, r, u)
	s := b.snap
	fs := rankFindings(s.run())
	safe := s.Target.Object
	out := diagnoseOut{
		Target: strings.ToLower(r.Kind) + "/" + u.GetName(), Kind: r.Kind, Namespace: u.GetNamespace(), Context: c.Context,
		Status: statusOf(fs), Conclusion: conclusionOf(fs),
		Details: detailsFor(r, safe, u.Object), Conditions: conditions(safe, g.full), Owners: b.owners,
		Count: len(fs), Findings: nonNil(fs), Logs: nonNil(b.logs), Redacted: b.redacted, Gaps: nonNil(b.gaps.sorted()),
	}
	if r.GVR.Resource != "pods" {
		out.Pods = g.podSummary(s, b)
	}
	out.Events = g.eventSummary(s)
	out.Checked = b.checked()
	if g.full {
		out.Object = safe
	}
	out.Next = g.diagnoseNext(s, fs, b.logs)
	return out
}

// eventSummary shows the newest warning events about the target, its pods and
// replica sets.
func (g *globals) eventSummary(s *snapshot) relatedEvents {
	evs := s.Events
	out := relatedEvents{Total: len(evs)}
	if !g.full && len(evs) > diagEvents {
		evs, out.Truncated = evs[:diagEvents], true
	}
	out.Count = len(evs)
	out.Items = eventRows(evs, false, g.full)
	switch {
	case out.Truncated:
		out.Note = fmt.Sprintf("showing %d of %d warning events; --full shows all", out.Count, out.Total)
	case out.Total == 0:
		out.Note = "no warning events about the target or its pods"
	}
	return out
}

func (g *globals) podSummary(s *snapshot, b *bundle) *diagPods {
	bad := map[string]bool{}
	for _, n := range s.unhealthyPods() {
		bad[n] = true
	}
	pods := append([]unstructured.Unstructured(nil), s.Pods...)
	sort.SliceStable(pods, func(i, j int) bool {
		bi, bj := bad[pods[i].GetName()], bad[pods[j].GetName()]
		if bi != bj {
			return bi
		}
		return pods[i].GetName() < pods[j].GetName()
	})
	out := &diagPods{Total: b.allPods, Unhealthy: len(bad), Items: []any{}}
	shown := pods
	if !g.full && len(shown) > diagPodRows {
		shown = shown[:diagPodRows]
	}
	for i := range shown {
		out.Items = append(out.Items, rowFor(podsResource, &shown[i], false))
	}
	out.Shown = len(shown)
	out.Truncated = out.Shown < out.Total
	switch {
	case b.podsMore:
		out.Note = fmt.Sprintf("analysed the first %d pods only; the target has more", diagPodLimit)
	case out.Truncated:
		out.Note = fmt.Sprintf("showing %d of %d pods, unhealthy first; --full shows all", out.Shown, out.Total)
	case out.Total == 0:
		out.Note = "the target has no pods"
	}
	return out
}

func (b *bundle) checked() []string {
	out := []string{"object", "owners"}
	if b.snap.Resource.GVR.Resource != "pods" {
		out = append(out, fmt.Sprintf("pods (%d)", b.allPods))
	}
	out = append(out, fmt.Sprintf("warning events (%d)", b.events))
	if b.slices > 0 || b.snap.SlicesRead {
		out = append(out, fmt.Sprintf("endpoint slices (%d)", b.slices))
	}
	if b.nodes > 0 {
		out = append(out, fmt.Sprintf("nodes (%d)", b.nodes))
	}
	if b.quotas > 0 {
		out = append(out, fmt.Sprintf("resource quotas (%d)", b.quotas))
	}
	return append(out, fmt.Sprintf("logs (%d)", len(b.logs)))
}

// gather reads the evidence in three bounded steps: the objects around the
// target, then what the pods point at (nodes, logs, namespace pods).
// Failures become gaps and never stop the command.
func (g *globals) gather(ctx context.Context, c *Clients, r Resource, u *unstructured.Unstructured) *bundle {
	ns := u.GetNamespace()
	s := &snapshot{Resource: r, Target: &unstructured.Unstructured{Object: scrubObject(u.Object)}, Nodes: map[string]*unstructured.Unstructured{}, Full: g.full}
	b := &bundle{snap: s, gaps: &gaps{}}
	var allEvents []eventResult
	var mu sync.Mutex

	steps := []func(context.Context){
		func(ctx context.Context) { b.owners = c.ownerChain(ctx, u) },
		func(ctx context.Context) {
			res, err := c.Events(ctx, eventQuery{Namespace: ns, Type: "Warning"})
			if err != nil {
				b.gaps.add("events", "list", "events", err)
				return
			}
			mu.Lock()
			allEvents = append(allEvents, res)
			mu.Unlock()
			if res.Cut {
				b.gaps.raw(fmt.Sprintf("events: more than %d matched, so the newest may be missing", maxEventScan))
			}
		},
	}
	switch r.GVR.Resource {
	case "pods":
		s.Pods = []unstructured.Unstructured{{Object: s.Target.Object}}
		b.allPods = 1
	default:
		if sel := podSelector(u, r); sel != "" {
			steps = append(steps, func(ctx context.Context) {
				res, err := c.List(ctx, podsResource, ns, metav1.ListOptions{LabelSelector: sel, Limit: diagPodLimit})
				if err != nil {
					b.gaps.add("pods", "list", "pods", err)
					return
				}
				s.Pods, b.allPods, b.podsMore = scrubAll(res.Items), len(res.Items), res.More
			})
		}
	}
	if r.GVR.Resource == "deployments" {
		steps = append(steps, func(ctx context.Context) {
			res, err := c.List(ctx, replicaSetsRes, ns, metav1.ListOptions{LabelSelector: labelSelector(u.Object, "spec", "selector"), Limit: diagReplicaSets})
			if err != nil {
				b.gaps.add("replica sets", "list", "replicasets", err)
				return
			}
			for _, rs := range res.Items {
				if ownedBy(&rs, u) {
					s.ReplicaSets = append(s.ReplicaSets, unstructured.Unstructured{Object: scrubObject(rs.Object)})
				}
			}
		})
	}
	if r.GVR.Resource == "services" {
		steps = append(steps, func(ctx context.Context) {
			res, err := c.List(ctx, slicesResource, ns, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + u.GetName(), Limit: diagSlices})
			if err != nil {
				b.gaps.add("endpoint slices", "list", "endpointslices", err)
				return
			}
			s.Slices, s.SlicesRead, b.slices = scrubAll(res.Items), true, len(res.Items)
		})
	}
	if s.missingReplicas() {
		steps = append(steps, func(ctx context.Context) {
			res, err := c.List(ctx, quotasResource, ns, metav1.ListOptions{Limit: diagQuotas})
			if err != nil {
				b.gaps.add("resource quotas", "list", "resourcequotas", err)
				return
			}
			s.Quotas, b.quotas = scrubAll(res.Items), len(res.Items)
		})
	}
	fanout(ctx, diagConcurrency, steps...)

	for _, res := range allEvents {
		s.Events = matchEvents(res.Events, s)
	}
	b.events = len(s.Events)
	g.gatherLate(ctx, c, b)
	return b
}

// gatherLate reads what the first step points at: the nodes of unhealthy pods,
// namespace pods to explain an empty service, and log tails.
func (g *globals) gatherLate(ctx context.Context, c *Clients, b *bundle) {
	s := b.snap
	ns := s.Target.GetNamespace()
	var steps []func(context.Context)

	var mu sync.Mutex
	for _, node := range unhealthyNodes(s, diagNodes) {
		steps = append(steps, func(ctx context.Context) {
			n, err := c.Typed.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
			if err != nil {
				b.gaps.add("node "+node, "get", "nodes", err)
				return
			}
			u, err := toUnstructured(Resource{GVR: gvr("", "v1", "nodes"), Kind: "Node"}, n)
			if err != nil {
				return
			}
			mu.Lock()
			s.Nodes[node] = &unstructured.Unstructured{Object: scrubObject(u.Object)}
			b.nodes++
			mu.Unlock()
		})
	}
	if s.Resource.GVR.Resource == "services" && len(s.Pods) == 0 && len(dict(s.Target.Object, "spec", "selector")) > 0 {
		steps = append(steps, func(ctx context.Context) {
			res, err := c.List(ctx, podsResource, ns, metav1.ListOptions{Limit: diagNSPods})
			if err != nil {
				b.gaps.add("namespace pods", "list", "pods", err)
				return
			}
			s.NSPods = scrubAll(res.Items)
		})
	}
	fanout(ctx, diagConcurrency, steps...)

	b.logs, b.redacted = g.readLogs(ctx, c, s, b.gaps)
}

// unhealthyNodes are the nodes that unhealthy pods run on, busiest first,
// at most limit of them.
func unhealthyNodes(s *snapshot, limit int) []string {
	count := map[string]int{}
	for i := range s.Pods {
		if n := str(s.Pods[i].Object, "spec", "nodeName"); n != "" && len(s.podFindings(&s.Pods[i])) > 0 {
			count[n]++
		}
	}
	names := sortedKeys(count)
	sort.SliceStable(names, func(i, j int) bool { return count[names[i]] > count[names[j]] })
	if len(names) > limit {
		names = names[:limit]
	}
	return names
}

func scrubAll(items []unstructured.Unstructured) []unstructured.Unstructured {
	out := make([]unstructured.Unstructured, len(items))
	for i := range items {
		out[i] = unstructured.Unstructured{Object: scrubObject(items[i].Object)}
	}
	return out
}

func ownedBy(obj, owner *unstructured.Unstructured) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.GetUID() {
			return true
		}
	}
	return false
}

// podSelector is the label selector that finds the pods of u.
func podSelector(u *unstructured.Unstructured, r Resource) string {
	if r.GVR.Resource == "services" {
		if len(dict(u.Object, "spec", "selector")) == 0 {
			return ""
		}
		return mapSelector(u.Object)
	}
	if sel := labelSelector(u.Object, "spec", "selector"); sel != "" {
		return sel
	}
	if r.GVR.Resource == "jobs" {
		return "batch.kubernetes.io/job-name=" + u.GetName()
	}
	return ""
}

// matchEvents keeps the events about the target, its pods and replica sets:
// by UID when the event carries one, else by kind and name.
func matchEvents(evs []corev1.Event, s *snapshot) []corev1.Event {
	uids, names := map[string]bool{}, map[string]bool{}
	add := func(kind string, u *unstructured.Unstructured) {
		uids[string(u.GetUID())] = true
		names[kind+"/"+u.GetName()] = true
	}
	add(s.Resource.Kind, s.Target)
	for i := range s.Pods {
		add("Pod", &s.Pods[i])
	}
	for i := range s.ReplicaSets {
		add("ReplicaSet", &s.ReplicaSets[i])
	}
	var out []corev1.Event
	for _, e := range evs {
		if uid := string(e.InvolvedObject.UID); uid != "" {
			if uids[uid] {
				out = append(out, e)
			}
			continue
		}
		if names[e.InvolvedObject.Kind+"/"+e.InvolvedObject.Name] {
			out = append(out, e)
		}
	}
	return out
}
