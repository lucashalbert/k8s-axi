package kube

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func typed[T any](t *testing.T, y string) *T {
	t.Helper()
	var v T
	if err := yaml.UnmarshalStrict([]byte(y), &v); err != nil {
		t.Fatalf("%v\n%s", err, y)
	}
	return &v
}

// brokenWeb is a Deployment whose two pods cannot pull their image, with a
// ReplicaSet, the pods' events and a warning that carries a token.
func brokenWeb(t *testing.T) []runtime.Object {
	dep := typed[appsv1.Deployment](t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: default, uid: uid-dep, generation: 1}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec: {containers: [{name: app, image: "registry.test/app:nope"}]}
status: {observedGeneration: 1, replicas: 2, updatedReplicas: 2, unavailableReplicas: 2}`)
	rs := typed[appsv1.ReplicaSet](t, `
apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: web-7d4
  namespace: default
  uid: uid-rs
  labels: {app: web}
  ownerReferences: [{apiVersion: apps/v1, kind: Deployment, name: web, uid: uid-dep, controller: true}]
spec: {replicas: 2, selector: {matchLabels: {app: web}}}`)
	objs := []runtime.Object{dep, rs}
	for _, n := range []string{"web-7d4-a", "web-7d4-b"} {
		objs = append(objs, typed[corev1.Pod](t, `
apiVersion: v1
kind: Pod
metadata:
  name: `+n+`
  namespace: default
  uid: uid-`+n+`
  labels: {app: web}
  ownerReferences: [{apiVersion: apps/v1, kind: ReplicaSet, name: web-7d4, uid: uid-rs, controller: true}]
spec:
  nodeName: n1
  containers:
  - name: app
    image: registry.test/app:nope
    env: [{name: DB_PASSWORD, value: hunter2-sentinel}]
status:
  phase: Pending
  containerStatuses:
  - name: app
    ready: false
    restartCount: 0
    state: {waiting: {reason: ImagePullBackOff, message: "Back-off pulling image \"registry.test/app:nope\": not found"}}`))
	}
	ev := event("default", "ev1", "Warning", "Failed", "Pod", "web-7d4-a", 2*time.Minute)
	ev.Message = "Failed to pull image: Authorization: Bearer abc.def.ghi-sentinel-token"
	return append(objs, ev, readyNode(t))
}

func readyNode(t *testing.T) *corev1.Node {
	return typed[corev1.Node](t, `
apiVersion: v1
kind: Node
metadata: {name: n1}
status: {conditions: [{type: Ready, status: "True"}]}`)
}

func TestDiagnoseDeploymentImagePull(t *testing.T) {
	freezeTime(t)
	c, _ := readFixture(t, brokenWeb(t))
	code, out, errOut := run(t, c, "diagnose", "deploy/web", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, leak := range []string{"hunter2-sentinel", "ghi-sentinel-token"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q", leak)
		}
	}
	m := runJSON(t, c, "diagnose", "deploy/web")
	if m["target"] != "deployment/web" || m["status"] != "unhealthy" || m["context"] != "kind-test" || m["namespace"] != "default" {
		t.Errorf("envelope = %v", m)
	}
	fs := objectsOf(m["findings"])
	if m["count"] != float64(len(fs)) || len(fs) < 3 {
		t.Fatalf("findings = %v", fs)
	}
	top := fs[0]
	if top["rank"] != float64(1) || top["rule"] != "image-pull" || top["basis"] != "observed" || top["count"] != float64(2) || top["subject"] != "pod/web-7d4-a, pod/web-7d4-b" {
		t.Errorf("top = %v", top)
	}
	if !strings.Contains(m["conclusion"].(string), "cannot pull its image") {
		t.Errorf("conclusion = %v", m["conclusion"])
	}
	var rules []string
	for _, f := range fs {
		rules = append(rules, f["rule"].(string))
	}
	if !strings.Contains(strings.Join(rules, ","), "image-pull-cause") || !strings.Contains(strings.Join(rules, ","), "replicas-unavailable") {
		t.Errorf("rules = %v", rules)
	}
	pods := m["pods"].(map[string]any)
	if pods["total"] != float64(2) || pods["unhealthy"] != float64(2) || pods["truncated"] != false || len(pods["items"].([]any)) != 2 {
		t.Errorf("pods = %v", pods)
	}
	if len(m["logs"].([]any)) != 0 {
		t.Errorf("a pod that cannot pull has no log to read: %v", m["logs"])
	}
	if len(m["owners"].([]any)) != 0 || m["checked"] == nil || m["gaps"] == nil {
		t.Errorf("owners/checked/gaps: %v %v %v", m["owners"], m["checked"], m["gaps"])
	}
	next := m["next"].([]any)
	if len(next) == 0 || len(next) > 4 || !strings.Contains(next[0].(string), "k8s-axi inspect pods web-7d4-a -n default") {
		t.Errorf("next = %v", next)
	}
	evs := m["events"].(map[string]any)
	if evs["count"] != float64(1) || evs["total"] != float64(1) || !strings.Contains(objectsOf(evs["items"])[0]["message"].(string), "Failed to pull image") {
		t.Errorf("events = %v", evs)
	}
	if m["details"].(map[string]any)["desired"] != float64(2) {
		t.Errorf("details = %v", m["details"])
	}
}

func TestDiagnoseFullAndTOON(t *testing.T) {
	freezeTime(t)
	c, _ := readFixture(t, brokenWeb(t))
	full := runJSON(t, c, "diagnose", "deploy", "web", "--full")
	obj, ok := full["object"].(map[string]any)
	if !ok || obj["kind"] != "Deployment" {
		t.Fatalf("--full adds the object: %v", full["object"])
	}
	_, out, _ := run(t, c, "diagnose", "deploy/web", "--full", "--json")
	if strings.Contains(out, "hunter2-sentinel") {
		t.Errorf("--full leaks an env value")
	}
	_, y, _ := run(t, c, "diagnose", "deploy/web", "--yaml")
	if !strings.Contains(y, "rule: image-pull") {
		t.Errorf("yaml = %s", y)
	}
	code, toon, _ := run(t, c, "diagnose", "deploy/web")
	if code != 0 || !strings.Contains(toon, "status: unhealthy") || !strings.Contains(toon, "findings[") || strings.HasPrefix(strings.TrimSpace(toon), "{") {
		t.Errorf("default output is TOON: %s", toon)
	}
}

func TestDiagnoseUsageAndMissingTarget(t *testing.T) {
	c, _ := readFixture(t, brokenWeb(t))
	for _, args := range [][]string{{"diagnose"}, {"diagnose", "deploy"}, {"diagnose", "secrets", "x"}, {"diagnose", "nodes", "n1"}} {
		if code, _, errOut := run(t, c, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", args, code, errOut)
		}
	}
	code, _, errOut := run(t, c, "diagnose", "secrets", "x")
	if code != 2 || !strings.Contains(errOut, "pods, deployments, statefulsets, daemonsets, jobs and services") {
		t.Errorf("unsupported kind should list the supported ones: %s", errOut)
	}
	if code, _, errOut := run(t, c, "diagnose", "deploy/nope"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("missing target: exit %d: %s", code, errOut)
	}
}

func TestDiagnoseForbiddenEventsIsAGap(t *testing.T) {
	freezeTime(t)
	c, cs := readFixture(t, brokenWeb(t))
	cs.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("no"))
	})
	m := runJSON(t, c, "diagnose", "deploy/web")
	if m["status"] != "unhealthy" {
		t.Errorf("the rest still runs: %v", m["status"])
	}
	gaps := m["gaps"].([]any)
	if len(gaps) != 1 || gaps[0] != "events: forbidden (RBAC denied 'list' on events)" {
		t.Errorf("gaps = %v", gaps)
	}
}

func crashLoopPod(t *testing.T, name, container string) *corev1.Pod {
	return typed[corev1.Pod](t, `
apiVersion: v1
kind: Pod
metadata: {name: `+name+`, namespace: default, uid: uid-`+name+`, labels: {app: crash}}
spec:
  nodeName: n1
  containers: [{name: `+container+`, image: "app:1", resources: {limits: {memory: 64Mi}}}]
status:
  phase: Running
  containerStatuses:
  - name: `+container+`
    ready: false
    restartCount: 5
    state: {waiting: {reason: CrashLoopBackOff}}
    lastState: {terminated: {reason: Error, exitCode: 1, finishedAt: "2026-11-07T11:57:00Z"}}`)
}

func stubDiagLogs(t *testing.T, fn func(ctx context.Context, pod, container string, previous bool) (string, error)) {
	t.Helper()
	old := podLog
	podLog = func(ctx context.Context, _ *Clients, _, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
		s, err := fn(ctx, pod, o.Container, o.Previous)
		return io.NopCloser(strings.NewReader(s)), err
	}
	t.Cleanup(func() { podLog = old })
}

func TestDiagnosePodReadsPreviousLogsRedactedAndBounded(t *testing.T) {
	freezeTime(t)
	var calls atomic.Int32
	stubDiagLogs(t, func(_ context.Context, pod, container string, previous bool) (string, error) {
		calls.Add(1)
		if !previous {
			t.Errorf("a crash looping container's previous instance is the one with the error")
		}
		return "starting\nfatal: cannot connect, Authorization: Bearer abc.def.ghi-sentinel-token\n", nil
	})
	c, _ := readFixture(t, []runtime.Object{crashLoopPod(t, "crash-1", "app"), readyNode(t)})
	m := runJSON(t, c, "diagnose", "pod", "crash-1")
	if m["status"] != "unhealthy" || m["target"] != "pod/crash-1" {
		t.Fatalf("envelope = %v", m)
	}
	if _, ok := m["pods"]; ok {
		t.Errorf("a pod target has no pods block: %v", m["pods"])
	}
	logs := m["logs"].([]any)
	if len(logs) != 1 {
		t.Fatalf("logs = %v", logs)
	}
	l := logs[0].(map[string]any)
	if l["pod"] != "crash-1" || l["container"] != "app" || l["previous"] != true || l["lines"] != float64(2) {
		t.Errorf("log = %v", l)
	}
	raw, _ := yaml.Marshal(m)
	if strings.Contains(string(raw), "ghi-sentinel-token") || m["redacted"] != float64(1) {
		t.Errorf("redacted = %v; leak = %v", m["redacted"], strings.Contains(string(raw), "ghi-sentinel-token"))
	}
	if next := m["next"].([]any); next[0] != "k8s-axi logs crash-1 -c app --previous -n default" {
		t.Errorf("next = %v", next)
	}
}

func TestDiagnoseLogSelectionUsesPreviousForRestartedFailure(t *testing.T) {
	cases := []struct {
		name         string
		status       map[string]any
		wantPrevious bool
	}{
		{
			name: "restarted current termination",
			status: map[string]any{
				"name":         "app",
				"restartCount": int64(2),
				"state":        map[string]any{"terminated": map[string]any{"exitCode": int64(3)}},
				"lastState":    map[string]any{"terminated": map[string]any{"exitCode": int64(3)}},
			},
			wantPrevious: true,
		},
		{
			name: "terminal one shot failure",
			status: map[string]any{
				"name":         "app",
				"restartCount": int64(0),
				"state":        map[string]any{"terminated": map[string]any{"exitCode": int64(3)}},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			sel, ok := logSelFor("crasher", tt.status)
			if !ok {
				t.Fatal("no log selected")
			}
			if sel.previous != tt.wantPrevious {
				t.Fatalf("previous = %v, want %v", sel.previous, tt.wantPrevious)
			}
		})
	}
}

func TestDiagnoseCapsLogStreams(t *testing.T) {
	freezeTime(t)
	var objs []runtime.Object
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		objs = append(objs, crashLoopPod(t, "p-"+n, "c-"+n)) // distinct containers
	}
	var calls atomic.Int32
	stubDiagLogs(t, func(context.Context, string, string, bool) (string, error) { calls.Add(1); return "x\n", nil })
	svcLike := typed[appsv1.DaemonSet](t, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent, namespace: default, uid: uid-ds}
spec: {selector: {matchLabels: {app: crash}}}
status: {desiredNumberScheduled: 6, numberReady: 0}`)
	c, _ := readFixture(t, append(objs, svcLike))
	m := runJSON(t, c, "diagnose", "daemonset", "agent")
	if n := len(m["logs"].([]any)); n != 3 || calls.Load() != 3 {
		t.Errorf("streams = %d, reads = %d, want 3", n, calls.Load())
	}
	full := runJSON(t, c, "diagnose", "daemonset", "agent", "--full")
	if n := len(full["logs"].([]any)); n != 5 {
		t.Errorf("--full streams = %d, want 5", n)
	}
	if pods := m["pods"].(map[string]any); pods["total"] != float64(6) || pods["shown"] != float64(6) {
		t.Errorf("pods = %v", pods)
	}
}

func TestDiagnoseDeadlineKeepsPartialResults(t *testing.T) {
	freezeTime(t)
	stubDiagLogs(t, func(ctx context.Context, _, _ string, _ bool) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	c, _ := readFixture(t, []runtime.Object{crashLoopPod(t, "crash-1", "app"), readyNode(t)})
	start := time.Now()
	m := runJSON(t, c, "diagnose", "pod", "crash-1", "--timeout", "40ms")
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	gaps := m["gaps"].([]any)
	if len(gaps) != 1 || !strings.Contains(gaps[0].(string), "logs crash-1/app (previous): timed out") {
		t.Errorf("gaps = %v", gaps)
	}
	if m["status"] != "unhealthy" {
		t.Errorf("findings survive a timed out log read: %v", m["status"])
	}
}

func TestDiagnoseHealthyAndNodeAndService(t *testing.T) {
	freezeTime(t)
	okPod := typed[corev1.Pod](t, `
apiVersion: v1
kind: Pod
metadata: {name: web-1, namespace: default, labels: {app: web}}
spec: {nodeName: n1, containers: [{name: app, image: "app:1"}]}
status:
  phase: Running
  conditions: [{type: Ready, status: "True"}]
  containerStatuses: [{name: app, ready: true, restartCount: 0, state: {running: {startedAt: "2026-11-07T11:00:00Z"}}}]`)
	svc := typed[corev1.Service](t, `
apiVersion: v1
kind: Service
metadata: {name: web, namespace: default}
spec: {selector: {app: web}, ports: [{port: 80}]}`)
	slice := typed[discoveryv1.EndpointSlice](t, `
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: web-x, namespace: default, labels: {kubernetes.io/service-name: web}}
addressType: IPv4
endpoints: [{addresses: [10.0.0.1], conditions: {ready: true}}]`)
	c, _ := readFixture(t, []runtime.Object{okPod, svc, slice})
	m := runJSON(t, c, "diagnose", "svc/web")
	if m["status"] != "healthy" || m["count"] != float64(0) || !strings.Contains(m["conclusion"].(string), "no problems found") {
		t.Errorf("healthy service = %v", m)
	}
	if len(m["findings"].([]any)) != 0 || !strings.Contains(strings.Join(anyStrings(m["checked"]), "|"), "endpoint slices (1)") {
		t.Errorf("an empty result states what was checked: %v", m["checked"])
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestFanoutHonoursTheConcurrencyCap(t *testing.T) {
	var running, peak atomic.Int32
	fns := make([]func(context.Context), 12)
	for i := range fns {
		fns[i] = func(context.Context) {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
		}
	}
	fanout(context.Background(), 4, fns...)
	if peak.Load() > 4 || peak.Load() < 2 {
		t.Errorf("peak concurrency = %d", peak.Load())
	}
}

func objectsOf(v any) []map[string]any {
	var out []map[string]any
	for _, x := range v.([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}
