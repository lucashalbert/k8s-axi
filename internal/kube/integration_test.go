//go:build integration

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/update"
)

// realRoot is the command tree against the cluster the default kubeconfig points at.
func realRoot(args ...string) (code int, stdout, stderr string) {
	root := cli.New("k8s-axi", update.Info{Version: "0.0.0"}, cli.EnvPrefix("K8S_AXI_"), Module{})
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	code = cli.Execute(root)
	return code, out.String(), errb.String()
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("%v\n%s", err, s)
	}
	return m
}

func TestClusterInfoAgainstKind(t *testing.T) {
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	code, out, errOut := realRoot("cluster", "info", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := decode(t, out)
	if got["reachable"] != true {
		t.Errorf("not reachable: %v", got)
	}
	if want := os.Getenv("K8S_AXI_EXPECT_MINOR"); want != "" {
		if v, _ := got["version"].(string); !strings.HasPrefix(v, "v"+want+".") {
			t.Errorf("server version %q, want minor %s", v, want)
		}
	}
}

func TestDiscoverAgainstKind(t *testing.T) {
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	c, err := Load(Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "widgets.example.com"},
		"spec": map[string]any{
			"group": "example.com", "scope": "Namespaced",
			"names": map[string]any{"plural": "widgets", "singular": "widget", "kind": "Widget", "listKind": "WidgetList"},
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object"}},
			}},
		},
	}}
	crds := c.Dynamic.Resource(crdGVR)
	if _, err := crds.Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = crds.Delete(ctx, "widgets.example.com", metav1.DeleteOptions{}) })

	deadline := time.Now().Add(60 * time.Second)
	for {
		code, out, errOut := realRoot("discover", "--json", "--limit", "0", "--group", "example.com")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if got := decode(t, out); got["matched"] == float64(1) {
			row := got["kinds"].([]any)[0].(map[string]any)
			if row["kind"] != "Widget" || row["crd"] != true || got["crd_source"] != "apiextensions" {
				t.Fatalf("custom resource row: %v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("custom resource never showed up in discovery")
		}
		time.Sleep(time.Second)
	}

	code, out, _ := realRoot("discover", "--json", "--limit", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	have := map[string]bool{}
	for _, r := range decode(t, out)["kinds"].([]any) {
		have[r.(map[string]any)["kind"].(string)] = true
	}
	for _, k := range []string{"Pod", "Node", "Deployment", "CustomResourceDefinition"} {
		if !have[k] {
			t.Errorf("built-in kind %s missing", k)
		}
	}
}

func TestDiscoverAgainstKindIncludesNonPreferredVersionOnlyResource(t *testing.T) {
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	c, err := Load(Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	crds := c.Dynamic.Resource(crdGVR)
	for _, crd := range []*unstructured.Unstructured{
		integrationCRD("nonpreferred.example.com", "gadgets", "Gadget", "v1"),
		integrationCRD("nonpreferred.example.com", "widgets", "Widget", "v1beta1"),
	} {
		name := crd.GetName()
		if _, err := crds.Create(ctx, crd, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = crds.Delete(ctx, name, metav1.DeleteOptions{}) })
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		code, out, errOut := realRoot("discover", "--json", "--limit", "0", "--group", "nonpreferred.example.com")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		got := decode(t, out)
		rows := map[string]map[string]any{}
		for _, r := range got["kinds"].([]any) {
			row := r.(map[string]any)
			rows[row["kind"].(string)] = row
		}
		if widget := rows["Widget"]; widget != nil {
			if widget["version"] != "v1beta1" || widget["group"] != "nonpreferred.example.com" || widget["crd"] != true {
				t.Fatalf("Widget row: %v", widget)
			}
			if gadget := rows["Gadget"]; gadget == nil || gadget["version"] != "v1" {
				t.Fatalf("Gadget row: %v", gadget)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Widget never showed up in non-preferred discovery version: %v", got)
		}
		time.Sleep(time.Second)
	}
}

func integrationCRD(group, plural, kind, version string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group": group, "scope": "Namespaced",
			"names": map[string]any{"plural": plural, "singular": strings.TrimSuffix(plural, "s"), "kind": kind, "listKind": kind + "List"},
			"versions": []any{map[string]any{
				"name": version, "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object"}},
			}},
		},
	}}
}

func TestUnreachableServerIsBoundedRuntimeError(t *testing.T) {
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	body := `apiVersion: v1
kind: Config
current-context: dead
clusters:
- name: dead
  cluster: {server: "https://192.0.2.1:6443"}
contexts:
- name: dead
  context: {cluster: dead, user: u}
users:
- name: u
  user: {token: x}
`
	if err := os.WriteFile(kc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, out, errOut := realRoot("cluster", "info", "--kubeconfig", kc, "--timeout", "2s")
	if code != 1 || out != "" || !strings.Contains(errOut, "dead") {
		t.Fatalf("exit %d stdout=%q stderr=%s", code, out, errOut)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("not bounded by --timeout: %v", d)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
