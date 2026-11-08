//go:build integration

package kube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
)

// The read commands against a real cluster. Every test works in its own
// namespace and removes it afterwards.

// busybox is served from registry.k8s.io, which has no pull rate limit.
const busybox = "registry.k8s.io/e2e-test-images/busybox:1.36.1-1"

func itSetup(t *testing.T) (*Clients, string) {
	t.Helper()
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	c, err := Load(Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ns := strings.ToLower(fmt.Sprintf("k8s-axi-it-%s-%d", strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()), time.Now().UnixNano()%100000))
	if len(ns) > 63 {
		ns = ns[:63]
	}
	ns = strings.TrimRight(ns, "-")
	ctx := context.Background()
	if _, err := c.Typed.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Typed.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}) })
	return c, ns
}

func eventually(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(time.Second)
	}
}

// itJSON runs a command that must succeed and returns its JSON output.
func itJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	code, out, errOut := realRoot(append(args, "--json")...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, errOut)
	}
	return decode(t, out)
}

// itTry runs a command and reports its JSON output, or nil when it fails.
func itTry(args ...string) map[string]any {
	code, out, _ := realRoot(append(args, "--json")...)
	if code != 0 {
		return nil
	}
	var m map[string]any
	_ = jsonUnmarshal(out, &m)
	return m
}

func rowsOf(m map[string]any, key string) []map[string]any {
	var out []map[string]any
	if l, ok := m[key].([]any); ok {
		for _, r := range l {
			out = append(out, r.(map[string]any))
		}
	}
	return out
}

func crdWithFreeSpec(group, plural, kind, version string) *unstructured.Unstructured {
	crd := integrationCRD(group, plural, kind, version)
	v := crd.Object["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	v["schema"] = map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}}
	return crd
}

func TestReadCoreCustomResourceInNonPreferredVersion(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	const group = "readcore.example.com"
	crds := c.Dynamic.Resource(crdGVR)
	for _, crd := range []*unstructured.Unstructured{
		crdWithFreeSpec(group, "gadgets", "Gadget", "v1"),
		crdWithFreeSpec(group, "widgets", "Widget", "v1beta1"), // v1 is preferred, so Widget is only in a non-preferred version
	} {
		name := crd.GetName()
		if _, err := crds.Create(ctx, crd, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = crds.Delete(ctx, name, metav1.DeleteOptions{}) })
	}
	eventually(t, "the CRDs to be served", 90*time.Second, func() bool {
		return itTry("get", "widgets."+group, "-n", ns) != nil && itTry("get", "gadgets."+group, "-n", ns) != nil
	})

	w := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1beta1", "kind": "Widget",
		"metadata": map[string]any{"name": "w1", "namespace": ns},
		"spec":     map[string]any{"size": int64(3)},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "AllGood", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}}},
	}}
	wgvr := c.Dynamic.Resource(gvr(group, "v1beta1", "widgets")).Namespace(ns)
	eventually(t, "creating the custom resource", 60*time.Second, func() bool {
		_, err := wgvr.Create(ctx, w, metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err)
	})

	for _, kind := range []string{"widgets." + group, "widgets.v1beta1." + group, group + "/v1beta1/widgets", "widgets"} {
		m := itJSON(t, "get", kind, "-n", ns)
		rows := rowsOf(m, "items")
		if m["resource"] != "widgets.v1beta1."+group || m["count"] != float64(1) || len(rows) != 1 || rows[0]["name"] != "w1" || rows[0]["status"] != "Ready" {
			t.Errorf("get %s: %v", kind, m)
		}
	}
	if m := itJSON(t, "get", "gadgets."+group, "-n", ns); m["count"] != float64(0) || !strings.Contains(m["note"].(string), "no gadgets found") {
		t.Errorf("empty gadgets: %v", m)
	}
	m := itJSON(t, "inspect", "widgets."+group, "w1", "-n", ns)
	details := m["details"].(map[string]any)
	if m["kind"] != "Widget" || details["spec"].(map[string]any)["size"] != float64(3) || len(rowsOf(m, "conditions")) != 1 {
		t.Errorf("inspect: %v", m)
	}
	if m := itJSON(t, "get", "widgets."+group, "w1", "-n", ns, "--full"); rowsOf(m, "items")[0]["spec"].(map[string]any)["size"] != float64(3) {
		t.Errorf("get --full: %v", m)
	}
}

func TestReadCoreSecretValuesNeverLeak(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns, Annotations: map[string]string{
			lastApplied: `{"data":{"password":"` + sentinelB64 + `"},"stringData":{"token":"` + sentinel + `"}}`}},
		Data: map[string][]byte{"password": []byte(sentinel), "user": []byte("admin-user")},
	}
	if _, err := c.Typed.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: ns}, Data: map[string]string{"db_password": sentinel, "mode": "fast"}}
	if _, err := c.Typed.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"get", "secrets"}, {"get", "secret", "db"}, {"get", "secret", "db", "--full"}, {"get", "secrets", "-A", "--full"},
		{"inspect", "secret", "db"}, {"inspect", "secret", "db", "--full"}, {"inspect", "secret", "db", "--labels", "--annotations", "--full"},
		{"get", "configmap", "cfg", "--full"}, {"inspect", "configmap", "cfg", "--full", "--annotations"},
		{"events"}, {"events", "secret/db"},
	} {
		for _, format := range []string{"", "--json", "--yaml"} {
			full := append([]string{}, args...)
			if !slices.Contains(args, "-A") {
				full = append(full, "-n", ns)
			}
			if format != "" {
				full = append(full, format)
			}
			code, out, errOut := realRoot(full...)
			if code != 0 && !strings.HasPrefix(args[0], "events") {
				t.Fatalf("%v: exit %d: %s", full, code, errOut)
			}
			for _, leak := range []string{sentinel, sentinelB64, "admin-user", b64("admin-user")} {
				if strings.Contains(out+errOut, leak) {
					t.Errorf("%v leaked %q:\n%s", full, leak, out)
				}
			}
		}
	}

	// Keys and sizes are what a Secret read shows.
	d := itJSON(t, "inspect", "secret", "db", "-n", ns)["details"].(map[string]any)
	keys := rowsOf(d, "keys")
	if d["type"] != "Opaque" || len(keys) != 2 || keys[0]["name"] != "password" || keys[0]["bytes"] != float64(len(sentinel)) {
		t.Errorf("details: %v", d)
	}
	if rows := rowsOf(itJSON(t, "get", "secrets", "-n", ns), "items"); len(rows) != 1 || rows[0]["keys"] != float64(2) {
		t.Errorf("get secrets: %v", rows)
	}
}

func TestReadCoreWorkloadOwnersEventsAndLogs(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	labels := map[string]string{"app": "web"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Image: busybox,
				Command: []string{"sh", "-c", `i=0; while true; do echo "tick $i token=sentinelTok123abc"; i=$((i+1)); sleep 1; done`},
				Env:     []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "env-literal-sentinel"}},
			}}}},
		},
	}
	if _, err := c.Typed.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	bad := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "badimage", Namespace: ns},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.invalid/nope:1"}}}}
	if _, err := c.Typed.CoreV1().Pods(ns).Create(ctx, bad, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	var pod string
	eventually(t, "the web pod to be running with output", 180*time.Second, func() bool {
		m := itTry("get", "pods", "-l", "app=web", "-n", ns)
		rows := rowsOf(m, "items")
		if len(rows) != 1 || rows[0]["status"] != "Running" {
			return false
		}
		pod = rows[0]["name"].(string)
		l := itTry("logs", "deploy/web", "-n", ns)
		s := rowsOf(l, "streams")
		return len(s) == 1 && s[0]["lines"].(float64) >= 3
	})

	// get
	m := itJSON(t, "get", "pods", "-n", ns)
	if m["count"] != float64(2) || m["truncated"] != false {
		t.Errorf("get pods: %v", m)
	}
	if m := itJSON(t, "get", "deploy", "web", "-n", ns); rowsOf(m, "items")[0]["ready"] != "1/1" {
		t.Errorf("get deploy: %v", m)
	}
	if m := itJSON(t, "get", "pods", "-n", ns, "--limit", "1"); m["count"] != float64(1) || m["truncated"] != true || !strings.Contains(m["note"].(string), "--limit 0") {
		t.Errorf("get --limit 1: %v", m)
	}
	if m := itJSON(t, "get", "nodes"); m["count"].(float64) < 1 || rowsOf(m, "items")[0]["status"] != "Ready" {
		t.Errorf("get nodes: %v", m)
	}
	if m := itJSON(t, "get", "ns", ns); rowsOf(m, "items")[0]["status"] != "Active" {
		t.Errorf("get ns: %v", m)
	}

	// inspect: owner chain Pod -> ReplicaSet -> Deployment
	ins := itJSON(t, "inspect", "pod", pod, "-n", ns)
	owners := rowsOf(ins, "owners")
	if len(owners) != 2 || owners[0]["kind"] != "ReplicaSet" || owners[1]["kind"] != "Deployment" || owners[1]["name"] != "web" {
		t.Errorf("owners: %v", owners)
	}
	if d := ins["details"].(map[string]any); d["status"] != "Running" || len(rowsOf(d, "containers")) != 1 {
		t.Errorf("details: %v", d)
	}
	if ev := ins["events"].(map[string]any); ev["count"].(float64) < 1 {
		t.Errorf("pod events: %v", ev)
	}
	if code, out, _ := realRoot("inspect", "pod", pod, "-n", ns, "--full", "--json"); code != 0 || strings.Contains(out, "env-literal-sentinel") {
		t.Errorf("env credential literal in --full output (exit %d)", code)
	}

	// events: a pod that cannot pull its image produces Warnings
	eventually(t, "a Warning event for the bad image", 120*time.Second, func() bool {
		m := itTry("events", "pod/badimage", "--type", "Warning", "-n", ns)
		return m != nil && m["count"].(float64) >= 1
	})
	ev := itJSON(t, "events", "-n", ns)
	if ev["warnings"].(float64) < 1 || ev["scope"] != "namespace/"+ns || ev["total"].(float64) < 2 {
		t.Errorf("events: %v", ev)
	}
	rows := rowsOf(ev, "events")
	if len(rows) == 0 || rows[0]["age"] == nil {
		t.Errorf("event rows: %v", rows)
	}
	if only := itJSON(t, "events", "pod/badimage", "-n", ns); only["total"].(float64) < 1 {
		t.Errorf("object events: %v", only)
	} else {
		for _, r := range rowsOf(only, "events") {
			if r["object"] != "Pod/badimage" {
				t.Errorf("event about another object: %v", r)
			}
		}
	}
	eventually(t, "the image pull status", 60*time.Second, func() bool {
		s := itTry("inspect", "pod", "badimage", "-n", ns)["details"]
		st, _ := s.(map[string]any)["status"].(string)
		return st == "ErrImagePull" || st == "ImagePullBackOff"
	})

	// logs: bounded, redacted, workload selector
	l := itJSON(t, "logs", "deploy/web", "--tail", "2", "-n", ns)
	streams := rowsOf(l, "streams")
	if l["target"] != "deployment/web" || l["pods"] != float64(1) || len(streams) != 1 || streams[0]["lines"].(float64) > 2 || l["truncated"] != true {
		t.Errorf("logs: %v", l)
	}
	code, out, _ := realRoot("logs", "deploy/web", "-n", ns, "--json")
	if code != 0 || strings.Contains(out, "sentinelTok123abc") || !strings.Contains(out, "[REDACTED]") {
		t.Errorf("logs not redacted (exit %d):\n%s", code, out)
	}
	if l := itJSON(t, "logs", "-l", "app=web", "-c", "app", "--since", "1m", "-n", ns); l["pods"] != float64(1) {
		t.Errorf("logs -l: %v", l)
	}
	if code, _, errOut := realRoot("logs", pod, "--previous", "-n", ns); code != 1 || !strings.Contains(errOut, "drop --previous") {
		t.Errorf("--previous without a previous instance: exit %d: %s", code, errOut)
	}
}

func TestReadCoreLogsPreviousOfARestartedContainer(t *testing.T) {
	c, ns := itSetup(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "crasher", Namespace: ns}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app", Image: busybox, Command: []string{"sh", "-c", "echo first-run-line; sleep 1; exit 1"}}}}}
	if _, err := c.Typed.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the container to restart", 180*time.Second, func() bool {
		rows := rowsOf(itTry("get", "pods", "-n", ns), "items")
		return len(rows) == 1 && rows[0]["restarts"].(float64) >= 1
	})
	var lines []string
	eventually(t, "previous logs", 60*time.Second, func() bool {
		m := itTry("logs", "crasher", "--previous", "-n", ns)
		if m == nil {
			return false
		}
		lines = nil
		for _, l := range rowsOf(rowsOf(m, "streams")[0], "log") {
			lines = append(lines, l["line"].(string))
		}
		return len(lines) > 0
	})
	if lines[0] != "first-run-line" {
		t.Errorf("previous log = %v", lines)
	}
}

func TestReadCoreForbiddenIsAStructuredError(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	if _, err := c.Typed.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "nobody", Namespace: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	tok, err := c.Typed.CoreV1().ServiceAccounts(ns).CreateToken(ctx, "nobody", &authv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token := tok.Status.Token

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: c.REST.Host, CertificateAuthorityData: c.REST.CAData, CertificateAuthority: c.REST.CAFile}
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts["x"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u", Namespace: ns}
	cfg.CurrentContext = "x"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"get", "pods"}, {"inspect", "pod", "x"}, {"events"}, {"logs", "x"}} {
		code, out, errOut := realRoot(append(args, "--kubeconfig", path)...)
		if code != 1 || out != "" || !strings.Contains(errOut, "RBAC denied") {
			t.Errorf("%v: exit %d stdout=%q stderr=%s", args, code, out, errOut)
		}
		if strings.Contains(errOut, token) {
			t.Errorf("%v leaked the token", args)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestReadCoreHintsPodCapByteBudgetAndEventRedaction(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	for i := 1; i <= 7; i++ {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("many-%d", i), Namespace: ns, Labels: map[string]string{"app": "many"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: busybox,
				Command: []string{"sh", "-c", `i=0; while [ $i -lt 20 ]; do echo "line $i ` + strings.Repeat("x", 90) + `"; i=$((i+1)); done; sleep 3600`}}}}}
		if _, err := c.Typed.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the pods to be running", 180*time.Second, func() bool {
		rows := rowsOf(itTry("get", "pods", "-l", "app=many", "-n", ns), "items")
		if len(rows) != 7 {
			return false
		}
		for _, r := range rows {
			if r["status"] != "Running" {
				return false
			}
		}
		return true
	})

	// Next-step hints carry the cluster the command was pointed at.
	m := itJSON(t, "get", "pods", "-n", ns, "--context", c.Context)
	next, _ := m["next"].([]any)
	if len(next) != 1 || !strings.HasPrefix(next[0].(string), "k8s-axi inspect pods many-1") ||
		!strings.Contains(next[0].(string), " -n "+ns) || !strings.Contains(next[0].(string), " --context "+c.Context) {
		t.Errorf("next = %v", next)
	}

	// The pod cap keeps the first five by name and says so; every selected container is read.
	l := itJSON(t, "logs", "-l", "app=many", "-n", ns)
	streams := rowsOf(l, "streams")
	if l["pods"] != float64(7) || len(streams) != 5 || streams[0]["pod"] != "many-1" || streams[4]["pod"] != "many-5" || !strings.Contains(l["note"].(string), "showing 5 of 7 pods") {
		t.Errorf("pod cap: %v", l)
	}

	// --max-bytes is a total across all streams.
	l = itJSON(t, "logs", "-l", "app=many", "-n", ns, "--max-bytes", "1024")
	streams = rowsOf(l, "streams")
	total := 0
	for _, s := range streams {
		for _, ln := range rowsOf(s, "log") {
			total += len(ln["line"].(string))
		}
	}
	if len(streams) != 5 || total > 1024 || l["truncated"] != true {
		t.Errorf("--max-bytes 1024: %d streams, %d bytes, truncated=%v", len(streams), total, l["truncated"])
	}

	// A secret-shaped event message is redacted wherever events are shown.
	const secretMsg = "retrying with token=sentinelEventTok99 and Authorization: Bearer abcdefghijklmnop12345"
	target, err := c.Typed.CoreV1().Pods(ns).Get(ctx, "many-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "leaky", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "many-1", Namespace: ns, UID: target.UID},
		Type:           corev1.EventTypeWarning, Reason: "Leaky", Message: secretMsg,
		FirstTimestamp: metav1.Now(), LastTimestamp: metav1.Now(), Count: 1,
	}
	if _, err := c.Typed.CoreV1().Events(ns).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"events"}, {"events", "pod/many-1"}, {"inspect", "pod", "many-1"}} {
		for _, extra := range [][]string{nil, {"--full"}, {"--yaml"}, {"--full", "--json"}} {
			full := append(append(append([]string{}, args...), "-n", ns), extra...)
			code, out, errOut := realRoot(full...)
			if code != 0 {
				t.Fatalf("%v: exit %d: %s", full, code, errOut)
			}
			if !strings.Contains(out, "Leaky") {
				t.Errorf("%v: the event is missing:\n%s", full, out)
			}
			for _, leak := range []string{"sentinelEventTok99", "abcdefghijklmnop12345"} {
				if strings.Contains(out, leak) {
					t.Errorf("%v leaked %q:\n%s", full, leak, out)
				}
			}
		}
	}
}

func TestReadCoreNamedClusterScopedReadsIgnoreNamespaceFlags(t *testing.T) {
	_, ns := itSetup(t)
	nodes := itJSON(t, "get", "nodes")
	name := rowsOf(nodes, "items")[0]["name"].(string)
	for _, extra := range [][]string{{"-A"}, {"-n", ns}} {
		m := itJSON(t, append([]string{"get", "node", name}, extra...)...)
		rows := rowsOf(m, "items")
		if m["count"] != float64(1) || rows[0]["name"] != name || m["namespace"] != nil {
			t.Errorf("get node %s %v: %v", name, extra, m)
		}
	}
	full := rowsOf(itJSON(t, "get", "node", name, "-A", "--full"), "items")
	if len(full) != 1 || full[0]["metadata"].(map[string]any)["name"] != name {
		t.Errorf("get node -A --full: %v", full)
	}
	if m := itJSON(t, "inspect", "node", name, "-n", ns); m["kind"] != "Node" || m["namespace"] != nil {
		t.Errorf("inspect node: %v", m)
	}
}
