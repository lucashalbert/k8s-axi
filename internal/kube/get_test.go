package kube

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/lucashalbert/axi-sdk-go/cli"
)

var fixedNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// freezeTime pins the clock the age columns read.
func freezeTime(t *testing.T) {
	t.Helper()
	old := now
	now = func() time.Time { return fixedNow }
	t.Cleanup(func() { now = old })
}

func created(d time.Duration) metav1.Time { return metav1.NewTime(fixedNow.Add(-d)) }

func runningPod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels, CreationTimestamp: created(3 * time.Hour)},
		Spec:       corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "app", Image: "app:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
}

func crashingPod(ns, name string) *corev1.Pod {
	p := runningPod(ns, name, nil)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	return p
}

func webDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web", CreationTimestamp: created(26 * time.Hour)},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
	}
}

func dbSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "db", CreationTimestamp: created(time.Hour),
			Annotations: map[string]string{lastApplied: `{"data":{"password":"` + sentinelB64 + `"}}`}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": []byte(sentinel), "user": []byte("admin-user")},
	}
}

const sentinel = "s3cr3t-sentinel-value"

var sentinelB64 = b64(sentinel)

func controlPlaneNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}, CreationTimestamp: created(72 * time.Hour)},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.37.0"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func readyWidget(ns, name string) *unstructured.Unstructured {
	w := widget(ns, name)
	w.SetCreationTimestamp(created(10 * time.Minute))
	w.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	return w
}

func getFixture(t *testing.T) (*Clients, *kubefake.Clientset) {
	t.Helper()
	freezeTime(t)
	return readFixture(t, []runtime.Object{
		runningPod("default", "a", map[string]string{"app": "x"}), crashingPod("default", "b"), runningPod("other", "c", nil),
		webDeployment(), dbSecret(), controlPlaneNode(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cfg"}, Data: map[string]string{"a": "1", "b": "2"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}, Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.1", Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}}}},
	}, readyWidget("default", "w1"))
}

// run executes the command line against c and returns the exit code and both
// streams.
func run(t *testing.T, c *Clients, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	root, out, errb := testRoot(t, c, nil)
	root.SetArgs(args)
	code = cli.Execute(root)
	return code, out.String(), errb.String()
}

func runJSON(t *testing.T, c *Clients, args ...string) map[string]any {
	t.Helper()
	code, out, errOut := run(t, c, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return m
}

func items(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, i := range m["items"].([]any) {
		out = append(out, i.(map[string]any))
	}
	return out
}

func TestGetPodsCompactColumns(t *testing.T) {
	c, _ := getFixture(t)
	m := runJSON(t, c, "get", "pods")
	if m["resource"] != "pods" || m["namespace"] != "default" || m["count"] != float64(2) || m["truncated"] != false {
		t.Fatalf("envelope: %v", m)
	}
	rows := items(m)
	if rows[0]["name"] != "a" || rows[0]["ready"] != "1/1" || rows[0]["status"] != "Running" || rows[0]["restarts"] != float64(0) || rows[0]["node"] != "n1" || rows[0]["age"] != "3h" {
		t.Errorf("row a: %v", rows[0])
	}
	if rows[1]["status"] != "CrashLoopBackOff" || rows[1]["ready"] != "0/1" || rows[1]["restarts"] != float64(3) {
		t.Errorf("row b: %v", rows[1])
	}
	if _, ok := rows[0]["namespace"]; ok {
		t.Errorf("namespace column without -A: %v", rows[0])
	}
	next := m["next"].([]any)
	if len(next) == 0 || !strings.Contains(next[0].(string), "k8s-axi inspect pods a -n default") {
		t.Errorf("next = %v", next)
	}
}

func TestGetTOONIsATable(t *testing.T) {
	c, _ := getFixture(t)
	code, out, errOut := run(t, c, "get", "pods")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"resource: pods", "count: 2", "truncated: false", "items[2]{name,ready,status,restarts,node,age}:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGetAllNamespacesAddsNamespaceColumn(t *testing.T) {
	c, _ := getFixture(t)
	m := runJSON(t, c, "get", "pods", "-A")
	if m["namespace"] != "*" || m["count"] != float64(3) {
		t.Fatalf("envelope: %v", m)
	}
	if rows := items(m); rows[2]["namespace"] != "other" || rows[2]["name"] != "c" {
		t.Errorf("rows: %v", rows)
	}
}

func TestGetSelectorAndNamespace(t *testing.T) {
	c, _ := getFixture(t)
	if m := runJSON(t, c, "get", "pods", "-l", "app=x"); m["count"] != float64(1) {
		t.Errorf("selector: %v", m)
	}
	m := runJSON(t, c, "get", "pods", "-n", "other")
	if m["namespace"] != "other" || m["count"] != float64(1) {
		t.Errorf("-n other: %v", m)
	}
}

func TestGetStatesEmptyResult(t *testing.T) {
	c, _ := getFixture(t)
	m := runJSON(t, c, "get", "pods", "-n", "empty", "-l", "app=none")
	if m["count"] != float64(0) || m["truncated"] != false {
		t.Fatalf("envelope: %v", m)
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "no pods found in namespace empty") || !strings.Contains(note, "app=none") {
		t.Errorf("note = %q", note)
	}
}

func TestGetStatesTruncation(t *testing.T) {
	c, cs := getFixture(t)
	cs.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		rem := int64(41)
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "tok", RemainingItemCount: &rem}, Items: []corev1.Pod{*runningPod("default", "a", nil)}}, nil
	})
	m := runJSON(t, c, "get", "pods", "--limit", "1")
	if m["count"] != float64(1) || m["truncated"] != true || m["remaining"] != float64(41) {
		t.Fatalf("envelope: %v", m)
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "showing 1 of 42") || !strings.Contains(note, "--limit 0") {
		t.Errorf("note = %q", note)
	}
}

func TestGetCapsWhenServerIgnoresLimit(t *testing.T) {
	c, _ := getFixture(t)
	m := runJSON(t, c, "get", "pods", "-A", "--limit", "2")
	if m["count"] != float64(2) || m["truncated"] != true {
		t.Errorf("envelope: %v", m)
	}
}

func TestGetOneByName(t *testing.T) {
	c, _ := getFixture(t)
	for _, args := range [][]string{{"get", "pod", "a"}, {"get", "pod/a"}} {
		m := runJSON(t, c, args...)
		if m["count"] != float64(1) || items(m)[0]["name"] != "a" {
			t.Errorf("%v: %v", args, m)
		}
	}
	code, out, errOut := run(t, c, "get", "pod", "missing")
	if code != 1 || out != "" || !strings.Contains(errOut, "not found") || !strings.Contains(errOut, "hint") {
		t.Errorf("missing: exit %d stdout=%q stderr=%s", code, out, errOut)
	}
}

func TestGetClusterScopedNameIgnoresAllNamespaces(t *testing.T) {
	c, _ := getFixture(t)
	m := runJSON(t, c, "get", "node", "n1", "-A")
	if m["count"] != float64(1) || items(m)[0]["name"] != "n1" || m["namespace"] != nil {
		t.Errorf("node read with -A: %v", m)
	}
	m = runJSON(t, c, "get", "node", "n1", "-n", "default", "-A")
	if m["count"] != float64(1) || items(m)[0]["name"] != "n1" || m["namespace"] != nil {
		t.Errorf("node read with -n and -A: %v", m)
	}
	if code, _, _ := run(t, c, "get", "pod", "a", "-A"); code != 2 {
		t.Errorf("namespaced object with -A exit %d, want 2", code)
	}
	if code, _, _ := run(t, c, "get", "pods", "-n", "x", "-A"); code != 2 {
		t.Errorf("namespaced list with -n and -A exit %d, want 2", code)
	}
}

func TestGetNextStepHintsQuoteShellArguments(t *testing.T) {
	freezeTime(t)
	c, _ := readFixture(t, []runtime.Object{runningPod("team one", "pod one", nil)})
	m := runJSON(t, c, "get", "pods", "-n", "team one", "--context", "prod east", "--kubeconfig", "/tmp/kube cfg")
	next := m["next"].([]any)[0].(string)
	want := "k8s-axi inspect pods 'pod one' --context 'prod east' --kubeconfig '/tmp/kube cfg' -n 'team one'"
	if next != want {
		t.Errorf("next = %q, want %q", next, want)
	}
}

func TestGetKinds(t *testing.T) {
	c, _ := getFixture(t)
	tests := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"get", "deploy"}, map[string]any{"name": "web", "ready": "2/3", "age": "26h"}},
		{[]string{"get", "nodes"}, map[string]any{"name": "n1", "status": "Ready", "roles": "control-plane", "version": "v1.37.0", "age": "3d"}},
		{[]string{"get", "svc"}, map[string]any{"name": "web", "type": "ClusterIP", "cluster_ip": "10.0.0.1", "ports": "80/TCP"}},
		{[]string{"get", "cm"}, map[string]any{"name": "cfg", "keys": float64(2)}},
		{[]string{"get", "secrets"}, map[string]any{"name": "db", "type": "Opaque", "keys": float64(2)}},
		{[]string{"get", "widgets"}, map[string]any{"name": "w1", "status": "Ready", "age": "10m"}},
	}
	for _, tt := range tests {
		m := runJSON(t, c, tt.args...)
		row := items(m)[0]
		for k, v := range tt.want {
			if row[k] != v {
				t.Errorf("%v: %s = %v, want %v (row %v)", tt.args, k, row[k], v, row)
			}
		}
	}
	// Served only in the non-preferred version of its group.
	if m := runJSON(t, c, "get", "widgets"); m["resource"] != "widgets.v1beta1.example.com" {
		t.Errorf("resource = %v", m["resource"])
	}
	// Cluster-scoped kinds have no namespace.
	if m := runJSON(t, c, "get", "nodes"); m["namespace"] != nil {
		t.Errorf("cluster-scoped namespace = %v", m["namespace"])
	}
}

func TestGetFullIsScrubbed(t *testing.T) {
	c, _ := getFixture(t)
	m := runJSON(t, c, "get", "secret", "db", "--full")
	obj := items(m)[0]
	data := obj["data"].(map[string]any)
	if data["password"] != "<21 bytes>" {
		t.Errorf("data = %v", data)
	}
	if obj["kind"] != "Secret" || obj["apiVersion"] != "v1" {
		t.Errorf("object not stamped: %v", obj)
	}
}

func TestGetSecretNeverLeaksInAnyFormat(t *testing.T) {
	c, _ := getFixture(t)
	for _, args := range [][]string{
		{"get", "secrets"}, {"get", "secret", "db"}, {"get", "secret", "db", "--full"},
		{"get", "secrets", "-A", "--full"}, {"get", "secret", "db", "--yaml"}, {"get", "secrets", "--json"},
	} {
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

func TestGetUsageErrors(t *testing.T) {
	c, _ := getFixture(t)
	for _, args := range [][]string{
		{"get"}, {"get", "frobnicators"}, {"get", "pods", "-n", "x", "-A"}, {"get", "pod", "a", "-l", "app=x"}, {"get", "a", "b", "c"},
	} {
		if code, _, _ := run(t, c, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
