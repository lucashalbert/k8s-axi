package kube

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func owned(kind, name, apiVersion string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, Controller: ptr.To(true)}}
}

// inspectFixture has the chain Pod web-rs-1 -> ReplicaSet web-rs -> Deployment web,
// a pod owned by something that no longer exists, events, a Secret and a CRD object.
func inspectFixture(t *testing.T) *Clients {
	t.Helper()
	freezeTime(t)
	pod := runningPod("default", "web-rs-1", nil)
	pod.OwnerReferences = owned("ReplicaSet", "web-rs", "apps/v1")
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: created(2 * time.Hour)}}
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "env-literal-pw"}, {Name: "MODE", Value: "fast"}}
	orphan := runningPod("default", "orphan", nil)
	orphan.OwnerReferences = owned("StatefulSet", "gone", "apps/v1")
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-rs", OwnerReferences: owned("Deployment", "web", "apps/v1")}}
	dep := webDeployment()
	dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable",
		Message: "Deployment does not have minimum availability.", LastTransitionTime: created(30 * time.Minute)}}
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	dep.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	dep.Status.AvailableReplicas, dep.Status.UpdatedReplicas = 2, 3

	objs := []runtime.Object{pod, orphan, rs, dep, dbSecret(), controlPlaneNode(),
		event("default", "e1", "Warning", "BackOff", "Pod", "web-rs-1", 2*time.Minute),
		event("default", "e2", "Normal", "Pulled", "Pod", "web-rs-1", 10*time.Minute),
		event("default", "e3", "Normal", "Scheduled", "Pod", "orphan", 5*time.Minute),
		event("default", "e4", "Normal", "NodeReady", "Node", "n1", 5*time.Minute),
	}
	c, _ := readFixture(t, objs, readyWidget("default", "w1"))
	return c
}

func TestInspectPod(t *testing.T) {
	c := inspectFixture(t)
	m := runJSON(t, c, "inspect", "pod", "web-rs-1")
	if m["kind"] != "Pod" || m["resource"] != "pods" || m["name"] != "web-rs-1" || m["namespace"] != "default" || m["age"] != "3h" || m["created"] != "2026-11-07T09:00:00Z" {
		t.Fatalf("identity: %v", m)
	}
	d := m["details"].(map[string]any)
	if d["status"] != "Running" || d["node"] != "n1" {
		t.Errorf("details: %v", d)
	}
	cont := d["containers"].([]any)[0].(map[string]any)
	if cont["name"] != "app" || cont["image"] != "app:1" || cont["ready"] != true || cont["state"] != "running" || cont["restarts"] != float64(0) {
		t.Errorf("container: %v", cont)
	}
	conds := m["conditions"].([]any)
	if len(conds) != 1 || conds[0].(map[string]any)["type"] != "Ready" || conds[0].(map[string]any)["status"] != "True" || conds[0].(map[string]any)["age"] != "2h" {
		t.Errorf("conditions: %v", conds)
	}
	for _, k := range []string{"labels", "annotations", "object"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s present by default", k)
		}
	}
}

func TestInspectOwnerChain(t *testing.T) {
	c := inspectFixture(t)
	owners := runJSON(t, c, "inspect", "pod/web-rs-1")["owners"].([]any)
	if len(owners) != 2 {
		t.Fatalf("owners: %v", owners)
	}
	first, second := owners[0].(map[string]any), owners[1].(map[string]any)
	if first["depth"] != float64(1) || first["kind"] != "ReplicaSet" || first["name"] != "web-rs" || first["controller"] != true {
		t.Errorf("first owner: %v", first)
	}
	if second["depth"] != float64(2) || second["kind"] != "Deployment" || second["name"] != "web" {
		t.Errorf("second owner: %v", second)
	}
}

func TestInspectUnreadableOwnerIsANoteNotAFailure(t *testing.T) {
	c := inspectFixture(t)
	owners := runJSON(t, c, "inspect", "pod", "orphan")["owners"].([]any)
	row := owners[0].(map[string]any)
	if len(owners) != 1 || row["kind"] != "StatefulSet" || row["name"] != "gone" || !strings.Contains(row["note"].(string), "not found") {
		t.Errorf("owners: %v", owners)
	}
}

func TestInspectStatesNoOwnersAndNoConditions(t *testing.T) {
	c := inspectFixture(t)
	m := runJSON(t, c, "inspect", "node", "n1")
	if len(m["owners"].([]any)) != 0 {
		t.Errorf("owners: %v", m["owners"])
	}
	code, out, _ := run(t, c, "inspect", "node", "n1")
	if code != 0 || !strings.Contains(out, "owners: []") || !strings.Contains(out, "conditions[1]") {
		t.Errorf("TOON states counts:\n%s", out)
	}
}

func TestInspectRelatedEvents(t *testing.T) {
	c := inspectFixture(t)
	ev := runJSON(t, c, "inspect", "pod", "web-rs-1")["events"].(map[string]any)
	if ev["count"] != float64(2) || ev["total"] != float64(2) || ev["truncated"] != false {
		t.Fatalf("events: %v", ev)
	}
	first := ev["items"].([]any)[0].(map[string]any)
	if first["reason"] != "BackOff" || first["type"] != "Warning" {
		t.Errorf("newest first: %v", ev["items"])
	}
	// Events of other objects are not mixed in.
	if o := runJSON(t, c, "inspect", "pod", "orphan")["events"].(map[string]any); o["count"] != float64(1) {
		t.Errorf("orphan events: %v", o)
	}
}

func TestInspectRelatedEventsRedactMessages(t *testing.T) {
	leak := "sentinelTok123abc"
	p := runningPod("default", "sensitive", nil)
	ev := event("default", "secret", "Warning", "Secret", "Pod", "sensitive", time.Minute)
	ev.Message = "token=" + leak
	c, _ := readFixture(t, []runtime.Object{p, ev})
	items := runJSON(t, c, "inspect", "pod", "sensitive")["events"].(map[string]any)["items"].([]any)
	msg := items[0].(map[string]any)["message"].(string)
	if strings.Contains(msg, leak) {
		t.Errorf("related event message leaked: %q", msg)
	}
}

func TestInspectEventsAreBounded(t *testing.T) {
	freezeTime(t)
	var objs []runtime.Object
	objs = append(objs, runningPod("default", "busy", nil))
	for i := 0; i < 14; i++ {
		objs = append(objs, event("default", "e"+string(rune('a'+i)), "Normal", "R", "Pod", "busy", time.Duration(i+1)*time.Minute))
	}
	c, _ := readFixture(t, objs)
	ev := runJSON(t, c, "inspect", "pod", "busy")["events"].(map[string]any)
	if ev["count"] != float64(10) || ev["total"] != float64(14) || ev["truncated"] != true || !strings.Contains(ev["note"].(string), "k8s-axi events pods busy") {
		t.Errorf("events: %v", ev)
	}
}

func TestInspectRelatedEventsNoteQuotesShellArguments(t *testing.T) {
	freezeTime(t)
	var objs []runtime.Object
	objs = append(objs, runningPod("team one", "busy pod", nil))
	for i := 0; i < 11; i++ {
		objs = append(objs, event("team one", "e"+string(rune('a'+i)), "Normal", "R", "Pod", "busy pod", time.Duration(i+1)*time.Minute))
	}
	c, _ := readFixture(t, objs)
	ev := runJSON(t, c, "inspect", "pod", "busy pod", "-n", "team one", "--context", "prod east", "--kubeconfig", "/tmp/kube cfg")["events"].(map[string]any)
	if note := ev["note"].(string); !strings.Contains(note, "k8s-axi events pods 'busy pod' --context 'prod east' --kubeconfig '/tmp/kube cfg' -n 'team one'") {
		t.Errorf("note = %q", note)
	}
}

func TestInspectWorkloadAndClusterScopedDetails(t *testing.T) {
	c := inspectFixture(t)
	d := runJSON(t, c, "inspect", "deploy", "web")["details"].(map[string]any)
	if d["desired"] != float64(3) || d["ready"] != float64(2) || d["available"] != float64(2) || d["updated"] != float64(3) || d["strategy"] != "RollingUpdate" || d["selector"] != "app=web" {
		t.Errorf("deployment details: %v", d)
	}
	m := runJSON(t, c, "inspect", "deploy", "web")
	cond := m["conditions"].([]any)[0].(map[string]any)
	if cond["type"] != "Available" || cond["status"] != "False" || cond["reason"] != "MinimumReplicasUnavailable" {
		t.Errorf("condition: %v", cond)
	}
	n := runJSON(t, c, "inspect", "node", "n1")
	if _, ok := n["namespace"]; ok {
		t.Errorf("cluster-scoped object has a namespace: %v", n)
	}
	if nd := n["details"].(map[string]any); nd["status"] != "Ready" || nd["roles"] != "control-plane" || nd["version"] != "v1.37.0" {
		t.Errorf("node details: %v", nd)
	}
	if nev := n["events"].(map[string]any); nev["count"] != float64(1) {
		t.Errorf("node events: %v", nev)
	}
}

func TestInspectCustomResourceInNonPreferredVersion(t *testing.T) {
	c := inspectFixture(t)
	m := runJSON(t, c, "inspect", "widgets", "w1")
	if m["kind"] != "Widget" || m["resource"] != "widgets.v1beta1.example.com" {
		t.Fatalf("identity: %v", m)
	}
	d := m["details"].(map[string]any)
	if d["spec"].(map[string]any)["size"] != float64(3) || !strings.Contains(d["note"].(string), "--full") {
		t.Errorf("details: %v", d)
	}
	if conds := m["conditions"].([]any); len(conds) != 1 {
		t.Errorf("conditions: %v", conds)
	}
}

func TestInspectLabelsAndAnnotationsOnRequest(t *testing.T) {
	c := inspectFixture(t)
	m := runJSON(t, c, "inspect", "secret", "db", "--annotations")
	if _, ok := m["labels"]; ok {
		t.Errorf("labels without --labels: %v", m)
	}
	if _, ok := m["annotations"]; ok {
		t.Errorf("the last-applied annotation is dropped, leaving none: %v", m["annotations"])
	}
	p := runningPod("default", "lab", map[string]string{"app": "x"})
	p.Annotations = map[string]string{"note": "hello", "auth": "Authorization: Bearer abcdefghijklmnop12345"}
	c2, _ := readFixture(t, []runtime.Object{p})
	got := runJSON(t, c2, "inspect", "pod", "lab", "--labels", "--annotations")
	if got["labels"].(map[string]any)["app"] != "x" {
		t.Errorf("labels: %v", got["labels"])
	}
	ann := got["annotations"].(map[string]any)
	if ann["note"] != "hello" || strings.Contains(ann["auth"].(string), "abcdefghijklmnop12345") {
		t.Errorf("annotations: %v", ann)
	}
}

func TestInspectSecretShowsKeysAndSizesOnly(t *testing.T) {
	c := inspectFixture(t)
	d := runJSON(t, c, "inspect", "secret", "db")["details"].(map[string]any)
	if d["type"] != "Opaque" {
		t.Errorf("details: %v", d)
	}
	keys := d["keys"].([]any)
	if len(keys) != 2 || keys[0].(map[string]any)["name"] != "password" || keys[0].(map[string]any)["bytes"] != float64(len(sentinel)) {
		t.Errorf("keys: %v", keys)
	}
}

func TestInspectSecretNeverLeaksInAnyFormat(t *testing.T) {
	c := inspectFixture(t)
	for _, flags := range [][]string{nil, {"--json"}, {"--yaml"}, {"--full"}, {"--full", "--json"}, {"--full", "--yaml"}, {"--labels", "--annotations", "--full"}} {
		args := append([]string{"inspect", "secret", "db"}, flags...)
		code, out, errOut := run(t, c, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		for _, leak := range []string{sentinel, sentinelB64, "admin-user", b64("admin-user")} {
			if strings.Contains(out+errOut, leak) {
				t.Errorf("%v leaked %q:\n%s", args, leak, out)
			}
		}
	}
}

func TestInspectFullAddsTheScrubbedObject(t *testing.T) {
	c := inspectFixture(t)
	code, out, _ := run(t, c, "inspect", "pod", "web-rs-1", "--full", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "env-literal-pw") {
		t.Errorf("credential env literal leaked:\n%s", out)
	}
	m := runJSON(t, c, "inspect", "pod", "web-rs-1", "--full")
	obj := m["object"].(map[string]any)
	if obj["kind"] != "Pod" || obj["metadata"].(map[string]any)["name"] != "web-rs-1" {
		t.Errorf("object: %v", obj)
	}
}

func TestInspectNextSteps(t *testing.T) {
	c := inspectFixture(t)
	next := runJSON(t, c, "inspect", "pod", "web-rs-1")["next"].([]any)
	if len(next) != 2 || !strings.Contains(next[0].(string), "k8s-axi events pods web-rs-1 -n default") || !strings.Contains(next[1].(string), "k8s-axi logs pods/web-rs-1 -n default") {
		t.Errorf("next: %v", next)
	}
}

func TestInspectMissingObjectAndUsage(t *testing.T) {
	c := inspectFixture(t)
	code, out, errOut := run(t, c, "inspect", "pod", "nope")
	if code != 1 || out != "" || !strings.Contains(errOut, "not found") {
		t.Errorf("missing: exit %d stdout=%q stderr=%s", code, out, errOut)
	}
	for _, args := range [][]string{{"inspect"}, {"inspect", "pods"}, {"inspect", "frobnicators", "x"}} {
		if code, _, _ := run(t, c, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
