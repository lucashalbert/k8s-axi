package kube

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func podFindingsOf(t *testing.T, p unstructured.Unstructured, events ...corev1.Event) []finding {
	t.Helper()
	s := snapOf(t, "pods", &p, p)
	s.Events = events
	return rankFindings(s.run())
}

func hasEvidence(f *finding, sub string) bool {
	for _, e := range f.Evidence {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

func TestPodImagePull(t *testing.T) {
	cases := []struct{ msg, cause string }{
		{`Back-off pulling image "registry.test/app:1"`, ""},
		{`failed to resolve reference "registry.test/app:1": registry.test/app:1: not found`, "does not exist"},
		{`pull access denied for app, repository does not exist or may require 'docker login'`, "does not exist, or the registry requires credentials"},
		{`unauthorized: authentication required`, "rejected the pull credentials"},
		{`dial tcp: lookup registry.test: no such host`, "cannot be reached"},
	}
	for _, c := range cases {
		p := podFix(t, "web", `
  phase: Pending
  containerStatuses:
  - name: app
    ready: false
    restartCount: 0
    state: {waiting: {reason: ImagePullBackOff, message: '`+strings.ReplaceAll(c.msg, "'", "''")+`'}}`)
		fs := podFindingsOf(t, p)
		got := find(fs, "image-pull")
		if got == nil || got.Basis != basisObserved || got.Severity != sevCritical || got.Subject != "pod/web" {
			t.Fatalf("%q: image-pull = %+v", c.msg, got)
		}
		if !hasEvidence(got, "registry.test/app:1") || !hasEvidence(got, "ImagePullBackOff") {
			t.Errorf("evidence = %v", got.Evidence)
		}
		cause := find(fs, "image-pull-cause")
		switch {
		case c.cause == "" && cause != nil:
			t.Errorf("%q: unexpected cause %+v", c.msg, cause)
		case c.cause != "" && (cause == nil || cause.Basis != basisInferred || !strings.Contains(cause.Summary, c.cause)):
			t.Errorf("%q: cause = %+v, want %q", c.msg, cause, c.cause)
		}
	}
}

func TestPodCrashLoopWithLastTermination(t *testing.T) {
	cases := map[string]string{
		"backoff":    `state: {waiting: {reason: CrashLoopBackOff, message: back-off 5m0s restarting failed container}}`,
		"restarting": `state: {waiting: {reason: ContainerCreating}}`,
	}
	for name, state := range cases {
		p := podFix(t, "web", `
  phase: Running
  containerStatuses:
  - name: app
    ready: false
    restartCount: 7
    `+state+`
    lastState: {terminated: {reason: Error, exitCode: 1, finishedAt: "2026-11-07T11:57:00Z"}}`)
		fs := podFindingsOf(t, p)
		got := find(fs, "crash-loop")
		if got == nil || got.Severity != sevCritical || got.Basis != basisObserved {
			t.Fatalf("%s: crash-loop = %+v", name, got)
		}
		for _, want := range []string{"restarts: 7", "last termination: Error, exit code 1, 3m ago"} {
			if !hasEvidence(got, want) {
				t.Errorf("%s: evidence %v lacks %q", name, got.Evidence, want)
			}
		}
		if find(fs, "oom-killed") != nil || find(fs, "restarts") != nil {
			t.Errorf("%s: unexpected extra findings: %v", name, ruleSet(fs))
		}
	}
}

func TestPodOOMKilledNamesTheLimit(t *testing.T) {
	p := podFix(t, "web", `
  phase: Running
  containerStatuses:
  - name: app
    ready: false
    restartCount: 3
    state: {waiting: {reason: CrashLoopBackOff}}
    lastState: {terminated: {reason: OOMKilled, exitCode: 137, finishedAt: "2026-11-07T11:59:00Z"}}`)
	fs := podFindingsOf(t, p)
	oom, cause := find(fs, "oom-killed"), find(fs, "oom-cause")
	if oom == nil || oom.Basis != basisObserved || !hasEvidence(oom, "memory limit: 64Mi") || !hasEvidence(oom, "exit code 137") {
		t.Fatalf("oom-killed = %+v", oom)
	}
	if cause == nil || cause.Basis != basisInferred || !strings.Contains(cause.Summary, "64Mi") {
		t.Fatalf("oom-cause = %+v", cause)
	}
	if find(fs, "crash-loop") == nil {
		t.Errorf("the crash loop is still reported: %v", ruleSet(fs))
	}
}

func TestPodRestartsWhileRunning(t *testing.T) {
	p := podFix(t, "web", `
  phase: Running
  conditions: [{type: Ready, status: "True"}]
  containerStatuses:
  - name: app
    ready: true
    restartCount: 2
    state: {running: {startedAt: "2026-11-07T11:00:00Z"}}
    lastState: {terminated: {reason: Error, exitCode: 2, finishedAt: "2026-11-07T10:00:00Z"}}`)
	fs := podFindingsOf(t, p)
	got := find(fs, "restarts")
	if got == nil || got.Severity != sevWarning || !hasEvidence(got, "exit code 2") {
		t.Fatalf("restarts = %+v (%v)", got, ruleSet(fs))
	}
}

func TestPodContainerConfigError(t *testing.T) {
	p := podFix(t, "web", `
  phase: Pending
  containerStatuses:
  - name: app
    ready: false
    state: {waiting: {reason: CreateContainerConfigError, message: 'configmap "cfg" not found'}}`)
	got := find(podFindingsOf(t, p), "container-error")
	if got == nil || got.Severity != sevCritical || !hasEvidence(got, `configmap "cfg" not found`) {
		t.Fatalf("container-error = %+v", got)
	}
}

func TestPodInitFailure(t *testing.T) {
	p := podFix(t, "web", `
  phase: Pending
  initContainerStatuses:
  - name: setup
    ready: false
    restartCount: 4
    state: {waiting: {reason: CrashLoopBackOff}}
    lastState: {terminated: {reason: Error, exitCode: 3, finishedAt: "2026-11-07T11:59:00Z"}}`)
	got := find(podFindingsOf(t, p), "init-failure")
	if got == nil || got.Severity != sevCritical || !hasEvidence(got, "init container setup") || !hasEvidence(got, "exit code 3") {
		t.Fatalf("init-failure = %+v", got)
	}
}

func TestPodContainerFailedAndEvicted(t *testing.T) {
	failed := podFix(t, "job-pod", `
  phase: Failed
  containerStatuses:
  - name: app
    ready: false
    state: {terminated: {reason: Error, exitCode: 9, finishedAt: "2026-11-07T11:50:00Z"}}`)
	if got := find(podFindingsOf(t, failed), "container-failed"); got == nil || !hasEvidence(got, "exit code 9") {
		t.Errorf("container-failed = %+v", got)
	}
	ev := podFix(t, "web", `
  phase: Failed
  reason: Evicted
  message: 'The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi, available: 100Mi.'`)
	fs := podFindingsOf(t, ev)
	if got := find(fs, "evicted"); got == nil || got.Severity != sevCritical || !hasEvidence(got, "ephemeral-storage") {
		t.Fatalf("evicted = %+v", got)
	}
	if got := find(fs, "evicted-cause"); got == nil || got.Basis != basisInferred || !strings.Contains(got.Summary, "disk") {
		t.Errorf("evicted-cause = %+v", got)
	}
}

func TestPodUnschedulableAndCauses(t *testing.T) {
	p := podFix(t, "web", `
  phase: Pending
  conditions:
  - type: PodScheduled
    status: "False"
    reason: Unschedulable
    message: "0/3 nodes are available: 1 Insufficient cpu, 2 node(s) had untolerated taint {gpu: true}. preemption: 0/3 nodes are available."`)
	fs := podFindingsOf(t, p)
	un := find(fs, "unschedulable")
	if un == nil || un.Basis != basisObserved || un.Severity != sevCritical || !hasEvidence(un, "0/3 nodes are available") {
		t.Fatalf("unschedulable = %+v", un)
	}
	var causes []string
	for _, f := range fs {
		if f.Rule == "unschedulable-cause" {
			if f.Basis != basisInferred {
				t.Errorf("cause must be inferred: %+v", f)
			}
			causes = append(causes, f.Summary)
		}
	}
	if len(causes) != 2 || !strings.Contains(strings.Join(causes, "|"), "cpu or memory") || !strings.Contains(strings.Join(causes, "|"), "tainted") {
		t.Errorf("causes = %v", causes)
	}
	for _, f := range fs {
		if f.Rule == "unschedulable-cause" && strings.Contains(f.Summary, "cpu or memory") && !hasEvidence(&f, "pod requests: cpu=500m memory=32Mi") {
			t.Errorf("evidence = %v", f.Evidence)
		}
	}
}

func TestPodUnschedulableCauseTable(t *testing.T) {
	cases := map[string]string{
		"0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.":          "nodeSelector or affinity",
		"0/2 nodes are available: pod has unbound immediate PersistentVolumeClaims.":             "volume",
		"0/2 nodes are available: 2 Too many pods.":                                              "pod limit",
		"0/2 nodes are available: 2 node(s) didn't have free ports for the requested pod ports.": "host port",
		"0/2 nodes are available: 2 node(s) didn't match pod anti-affinity rules.":               "anti-affinity",
		"0/2 nodes are available: something entirely unrecognised happened.":                     "",
	}
	for msg, want := range cases {
		p := podFix(t, "web", `
  phase: Pending
  conditions: [{type: PodScheduled, status: "False", reason: Unschedulable, message: "`+msg+`"}]`)
		fs := podFindingsOf(t, p)
		c := find(fs, "unschedulable-cause")
		if want == "" && c != nil {
			t.Errorf("%q: unexpected cause %+v", msg, c)
		}
		if want != "" && (c == nil || !strings.Contains(c.Summary, want)) {
			t.Errorf("%q: cause = %+v, want %q", msg, c, want)
		}
	}
}

func TestPodProbeEventsAndNotReady(t *testing.T) {
	p := podFix(t, "web", `
  phase: Running
  conditions: [{type: Ready, status: "False", reason: ContainersNotReady, message: "containers with unready status: [app]"}]
  containerStatuses:
  - name: app
    ready: false
    restartCount: 0
    state: {running: {startedAt: "2026-11-07T11:00:00Z"}}`)
	probe := warn("Pod", "web", "Unhealthy", "Readiness probe failed: HTTP probe failed with statuscode: 503", 12)
	fs := podFindingsOf(t, p, probe)
	pf := find(fs, "probe-failed")
	if pf == nil || pf.Severity != sevWarning || pf.Basis != basisObserved || !hasEvidence(pf, "statuscode: 503") || !hasEvidence(pf, "12 times") {
		t.Fatalf("probe-failed = %+v", pf)
	}
	if find(fs, "not-ready") != nil {
		t.Errorf("not-ready is only for pods nothing else explains: %v", ruleSet(fs))
	}
	nr := find(podFindingsOf(t, p), "not-ready")
	if nr == nil || !hasEvidence(nr, "ContainersNotReady") || !hasEvidence(nr, "app") {
		t.Errorf("not-ready = %+v", nr)
	}
	other := warn("Pod", "other", "Unhealthy", "Readiness probe failed: x", 1)
	if find(podFindingsOf(t, p, other), "probe-failed") != nil {
		t.Errorf("events of other pods must not match")
	}
}

func TestPodStartupEventsAndTerminating(t *testing.T) {
	p := podFix(t, "web", `
  phase: Pending
  containerStatuses:
  - name: app
    ready: false
    state: {waiting: {reason: ContainerCreating}}`)
	fs := podFindingsOf(t, p, warn("Pod", "web", "FailedMount", `MountVolume.SetUp failed for volume "cfg" : secret "creds" not found`, 3))
	got := find(fs, "startup-failed")
	if got == nil || got.Severity != sevCritical || !hasEvidence(got, `secret "creds" not found`) {
		t.Fatalf("startup-failed = %+v", got)
	}
	term := podFix(t, "gone", `
  phase: Running
  conditions: [{type: Ready, status: "True"}]`)
	ten := metav1.NewTime(diagNow.Add(-10 * time.Minute))
	term.SetDeletionTimestamp(&ten)
	term.SetFinalizers([]string{"example.com/hold"})
	ts := find(podFindingsOf(t, term), "terminating-stuck")
	if ts == nil || ts.Basis != basisInferred || !hasEvidence(ts, "example.com/hold") {
		t.Errorf("terminating-stuck = %+v", ts)
	}
}

func TestHealthyPodHasNoFindings(t *testing.T) {
	p := podFix(t, "web", `
  phase: Running
  conditions: [{type: Ready, status: "True"}]
  containerStatuses:
  - name: app
    ready: true
    restartCount: 0
    state: {running: {startedAt: "2026-11-07T11:00:00Z"}}`)
	if fs := podFindingsOf(t, p); len(fs) != 0 {
		t.Errorf("findings = %v", ruleSet(fs))
	}
}

func TestNodeUnhealthy(t *testing.T) {
	p := podFix(t, "web", `
  phase: Running
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - name: app
    ready: false
    state: {running: {startedAt: "2026-11-07T11:00:00Z"}}`)
	s := snapOf(t, "pods", &p, p)
	s.Nodes["node-1"] = objPtr(t, `
kind: Node
metadata: {name: node-1}
status:
  conditions:
  - {type: Ready, status: "False", reason: KubeletNotReady, message: container runtime is down}
  - {type: MemoryPressure, status: "True", reason: KubeletHasInsufficientMemory}
  - {type: DiskPressure, status: "False"}`)
	got := find(rankFindings(s.run()), "node-unhealthy")
	if got == nil || got.Subject != "node/node-1" || got.Severity != sevCritical || got.Basis != basisObserved ||
		!hasEvidence(got, "Ready=False") || !hasEvidence(got, "MemoryPressure=True") || !hasEvidence(got, "1 unhealthy pod") {
		t.Fatalf("node-unhealthy = %+v", got)
	}
}
