package kube

import (
	"strings"
	"testing"
)

func deployFix(t *testing.T, spec, status string) *snapshot {
	d := objPtr(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: default, generation: 3, uid: uid-web}
spec:
`+spec+`
status:
`+status)
	return snapOf(t, "deployments", d)
}

func ranked(s *snapshot) []finding { return rankFindings(s.run()) }

func TestDeploymentStalledAndUnavailable(t *testing.T) {
	s := deployFix(t, "  replicas: 3", `
  observedGeneration: 3
  replicas: 3
  updatedReplicas: 3
  readyReplicas: 1
  availableReplicas: 1
  conditions:
  - {type: Available, status: "False", reason: MinimumReplicasUnavailable}
  - {type: Progressing, status: "False", reason: ProgressDeadlineExceeded, message: 'ReplicaSet "web-7d4" has timed out progressing.'}`)
	fs := ranked(s)
	st := find(fs, "rollout-stalled")
	if st == nil || st.Severity != sevCritical || st.Basis != basisObserved || !hasEvidence(st, "timed out progressing") {
		t.Fatalf("rollout-stalled = %+v", st)
	}
	un := find(fs, "replicas-unavailable")
	if un == nil || un.Severity != sevWarning || !hasEvidence(un, "desired 3, ready 1, available 1, updated 3") {
		t.Fatalf("replicas-unavailable = %+v", un)
	}
	if fs[0].Rule != "rollout-stalled" {
		t.Errorf("top = %s", fs[0].Rule)
	}
}

func TestDeploymentNothingAvailableIsCritical(t *testing.T) {
	s := deployFix(t, "  replicas: 2", "  observedGeneration: 3\n  replicas: 2\n  updatedReplicas: 2")
	if got := find(ranked(s), "replicas-unavailable"); got == nil || got.Severity != sevCritical {
		t.Errorf("replicas-unavailable = %+v", got)
	}
}

func TestDeploymentProgressingPausedAndZero(t *testing.T) {
	s := deployFix(t, "  replicas: 3\n  paused: true", "  observedGeneration: 2\n  replicas: 3\n  updatedReplicas: 1\n  readyReplicas: 3\n  availableReplicas: 3")
	fs := ranked(s)
	if p := find(fs, "paused"); p == nil || p.Severity != sevWarning {
		t.Errorf("paused = %+v", p)
	}
	rp := find(fs, "rollout-progressing")
	if rp == nil || rp.Severity != sevInfo || !hasEvidence(rp, "updated 1 of 3") || !hasEvidence(rp, "observedGeneration 2 is behind generation 3") {
		t.Errorf("rollout-progressing = %+v", rp)
	}
	z := deployFix(t, "  replicas: 0", "  observedGeneration: 3")
	zf := ranked(z)
	if len(zf) != 1 || zf[0].Rule != "scaled-to-zero" || zf[0].Severity != sevInfo {
		t.Errorf("scaled-to-zero = %v", zf)
	}
	if statusOf(zf) != "healthy" {
		t.Errorf("a scaled-to-zero workload is not unhealthy")
	}
}

func TestHealthyDeploymentHasNoFindings(t *testing.T) {
	s := deployFix(t, "  replicas: 2", "  observedGeneration: 3\n  replicas: 2\n  updatedReplicas: 2\n  readyReplicas: 2\n  availableReplicas: 2")
	if fs := ranked(s); len(fs) != 0 {
		t.Errorf("findings = %v", ruleSet(fs))
	}
}

func TestFailedCreateEventsAndQuota(t *testing.T) {
	s := deployFix(t, "  replicas: 3", `
  observedGeneration: 3
  replicas: 0
  conditions:
  - {type: ReplicaFailure, status: "True", reason: FailedCreate, message: 'pods "web-7d4-x" is forbidden: exceeded quota: compute'}`)
	s.Events = append(s.Events, warn("ReplicaSet", "web-7d4", "FailedCreate", `Error creating: pods "web-7d4-x" is forbidden: exceeded quota: compute, requested: requests.cpu=500m, used: requests.cpu=4, limited: requests.cpu=4`, 9))
	s.Quotas = append(s.Quotas, obj(t, `
kind: ResourceQuota
metadata: {name: compute, namespace: default}
status:
  hard: {requests.cpu: "4", pods: "10"}
  used: {requests.cpu: "4", pods: "3"}`))
	fs := ranked(s)
	q := find(fs, "quota-exceeded")
	if q == nil || q.Severity != sevCritical || q.Basis != basisObserved || !hasEvidence(q, "exceeded quota: compute") || !hasEvidence(q, "ResourceQuota compute: requests.cpu 4/4") {
		t.Fatalf("quota-exceeded = %+v", q)
	}
	if find(fs, "replica-failure") != nil {
		t.Errorf("a quota failure is not also reported as a generic replica failure: %v", ruleSet(fs))
	}
	if fs[0].Rule != "quota-exceeded" {
		t.Errorf("cause should rank first: %v", ruleSet(fs))
	}
}

func TestReplicaFailureWithoutQuota(t *testing.T) {
	s := deployFix(t, "  replicas: 2", `
  observedGeneration: 3
  conditions:
  - {type: ReplicaFailure, status: "True", reason: FailedCreate, message: 'pods "x" is forbidden: violates PodSecurity "restricted:latest"'}`)
	got := find(ranked(s), "replica-failure")
	if got == nil || got.Severity != sevCritical || !hasEvidence(got, "violates PodSecurity") {
		t.Fatalf("replica-failure = %+v", got)
	}
	s.Events = append(s.Events, warn("ReplicaSet", "web-1", "FailedCreate", "Error creating: admission webhook denied the request", 2))
	got = find(ranked(s), "replica-failure")
	if got == nil || len(got.Evidence) == 0 || !strings.Contains(got.Evidence[0], "admission webhook") || got.Count != 1 {
		t.Errorf("events replace the condition as evidence: %+v", got)
	}
}

func TestQuotaExhaustedWithoutEventStillReported(t *testing.T) {
	s := deployFix(t, "  replicas: 2", "  observedGeneration: 3\n  replicas: 2\n  readyReplicas: 0")
	s.Quotas = append(s.Quotas, obj(t, `
kind: ResourceQuota
metadata: {name: pods, namespace: default}
status:
  hard: {pods: "5"}
  used: {pods: "5"}`))
	q := find(ranked(s), "quota-exceeded")
	if q == nil || !hasEvidence(q, "ResourceQuota pods: pods 5/5") {
		t.Fatalf("quota-exceeded = %+v", q)
	}
	healthy := deployFix(t, "  replicas: 2", "  observedGeneration: 3\n  replicas: 2\n  updatedReplicas: 2\n  readyReplicas: 2\n  availableReplicas: 2")
	healthy.Quotas = s.Quotas
	if find(ranked(healthy), "quota-exceeded") != nil {
		t.Errorf("a full quota is not a problem for a workload with all replicas")
	}
}

func TestStatefulSetBlockedRollout(t *testing.T) {
	d := objPtr(t, `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: default, generation: 2}
spec: {replicas: 3, podManagementPolicy: OrderedReady}
status:
  observedGeneration: 2
  replicas: 3
  readyReplicas: 2
  currentRevision: db-aaa
  updateRevision: db-bbb
  updatedReplicas: 1`)
	s := snapOf(t, "statefulsets", d, podFix(t, "db-2", `
  phase: Running
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - {name: app, ready: false, state: {running: {startedAt: "2026-11-07T11:00:00Z"}}}`))
	fs := ranked(s)
	rb := find(fs, "rollout-blocked")
	if rb == nil || rb.Basis != basisInferred || !hasEvidence(rb, "db-aaa -> db-bbb") || !hasEvidence(rb, "db-2") {
		t.Fatalf("rollout-blocked = %+v", rb)
	}
	if un := find(fs, "replicas-unavailable"); un == nil || !hasEvidence(un, "desired 3, ready 2") {
		t.Errorf("replicas-unavailable = %+v", un)
	}
}

func TestDaemonSet(t *testing.T) {
	d := objPtr(t, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent, namespace: default}
spec: {}
status: {desiredNumberScheduled: 4, numberReady: 2, numberAvailable: 2, numberUnavailable: 2, numberMisscheduled: 1, updatedNumberScheduled: 4}`)
	got := find(ranked(snapOf(t, "daemonsets", d)), "daemonset-unavailable")
	if got == nil || got.Severity != sevWarning || !hasEvidence(got, "desired 4, ready 2, unavailable 2, misscheduled 1") {
		t.Fatalf("daemonset-unavailable = %+v", got)
	}
	zero := objPtr(t, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent, namespace: default}
status: {desiredNumberScheduled: 0}`)
	if z := find(ranked(snapOf(t, "daemonsets", zero)), "scaled-to-zero"); z == nil || z.Severity != sevInfo {
		t.Errorf("scaled-to-zero = %+v", z)
	}
}

func TestJob(t *testing.T) {
	failed := objPtr(t, `
apiVersion: batch/v1
kind: Job
metadata: {name: batch, namespace: default}
spec: {completions: 1, backoffLimit: 2}
status:
  failed: 3
  conditions:
  - {type: Failed, status: "True", reason: BackoffLimitExceeded, message: Job has reached the specified backoff limit}`)
	got := find(ranked(snapOf(t, "jobs", failed)), "job-failed")
	if got == nil || got.Severity != sevCritical || !hasEvidence(got, "BackoffLimitExceeded") || !hasEvidence(got, "3 failed pod(s)") {
		t.Fatalf("job-failed = %+v", got)
	}
	retry := objPtr(t, `
apiVersion: batch/v1
kind: Job
metadata: {name: batch, namespace: default}
spec: {completions: 1, backoffLimit: 6}
status: {failed: 2, active: 1}`)
	r := find(ranked(snapOf(t, "jobs", retry)), "job-retrying")
	if r == nil || r.Severity != sevWarning || !hasEvidence(r, "2 failed pod(s) so far, backoffLimit 6") {
		t.Fatalf("job-retrying = %+v", r)
	}
	susp := objPtr(t, `
apiVersion: batch/v1
kind: Job
metadata: {name: batch, namespace: default}
spec: {suspend: true}
status: {}`)
	if s := find(ranked(snapOf(t, "jobs", susp)), "job-suspended"); s == nil || s.Severity != sevInfo {
		t.Errorf("job-suspended = %+v", s)
	}
	done := objPtr(t, `
apiVersion: batch/v1
kind: Job
metadata: {name: batch, namespace: default}
spec: {completions: 1}
status: {succeeded: 1, conditions: [{type: Complete, status: "True"}]}`)
	if fs := ranked(snapOf(t, "jobs", done)); len(fs) != 0 {
		t.Errorf("a completed job has no findings: %v", ruleSet(fs))
	}
}
