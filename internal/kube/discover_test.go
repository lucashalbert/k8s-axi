package kube

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
)

type runtimeObject = runtime.Object

func usageForTest() error {
	return axierr.Usage("kubeconfig", "no current context is set; available contexts: alpha, beta", "")
}

func crdObject(group, plural string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec":       map[string]any{"group": group, "names": map[string]any{"plural": plural}},
	}}
}

func discoverFixture(t *testing.T, crds ...runtime.Object) (*Clients, *fakediscovery.FakeDiscovery) {
	t.Helper()
	c, fd := fakeClients()
	fd.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, ShortNames: []string{"po"}, Verbs: metav1.Verbs{"get", "list"}},
			{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get"}},
			{Name: "nodes", SingularName: "node", Kind: "Node", Verbs: metav1.Verbs{"get", "list", "watch"}},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true, ShortNames: []string{"deploy"}, Verbs: metav1.Verbs{"get", "list"}},
		}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Kind: "Widget", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}},
		}},
	}
	c.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crds...)
	return c, fd
}

func runDiscover(t *testing.T, c *Clients, args ...string) (code int, out map[string]any, raw, errOut string) {
	t.Helper()
	root, stdout, stderr := testRoot(t, c, nil)
	root.SetArgs(append([]string{"discover", "--json"}, args...))
	code = cli.Execute(root)
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("%v\n%s", err, stdout)
		}
	}
	return code, out, stdout.String(), stderr.String()
}

func kinds(out map[string]any) map[string]map[string]any {
	m := map[string]map[string]any{}
	for _, k := range out["kinds"].([]any) {
		row := k.(map[string]any)
		m[row["kind"].(string)] = row
	}
	return m
}

func TestDiscoverListsBuiltinAndCRDKinds(t *testing.T) {
	c, _ := discoverFixture(t, crdObject("example.com", "widgets"))
	code, out, _, _ := runDiscover(t, c)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	k := kinds(out)
	if len(k) != 4 { // subresource pods/log is not a kind of its own
		t.Fatalf("want 4 kinds, got %d: %v", len(k), k)
	}
	if k["Pod"]["group"] != "core" || k["Pod"]["scope"] != "namespaced" || k["Pod"]["crd"] != false {
		t.Errorf("Pod row: %v", k["Pod"])
	}
	if k["Node"]["scope"] != "cluster" || k["Deployment"]["group"] != "apps" || k["Deployment"]["version"] != "v1" {
		t.Errorf("Node/Deployment rows: %v %v", k["Node"], k["Deployment"])
	}
	if k["Widget"]["crd"] != true || k["Pod"]["verbs"] != "get,list" {
		t.Errorf("Widget/verbs: %v", k["Widget"])
	}
	if out["crd_source"] != "apiextensions" {
		t.Errorf("crd_source = %v", out["crd_source"])
	}
}

func TestDiscoverCRDHeuristicWhenListForbidden(t *testing.T) {
	c, _ := discoverFixture(t)
	c.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "customresourcedefinitions",
		func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") })
	_, out, _, _ := runDiscover(t, c)
	k := kinds(out)
	if out["crd_source"] != "heuristic" || k["Widget"]["crd"] != true || k["Deployment"]["crd"] != false || k["Pod"]["crd"] != false {
		t.Errorf("heuristic: source=%v widget=%v deployment=%v", out["crd_source"], k["Widget"], k["Deployment"])
	}
}

func TestDiscoverFilters(t *testing.T) {
	c, _ := discoverFixture(t)
	_, out, _, _ := runDiscover(t, c, "--group", "apps")
	if k := kinds(out); len(k) != 1 || k["Deployment"] == nil {
		t.Errorf("--group apps: %v", k)
	}
	_, out, _, _ = runDiscover(t, c, "--group", "core")
	if k := kinds(out); len(k) != 2 {
		t.Errorf("--group core: %v", k)
	}
	for _, q := range []string{"deploy", "Deployment", "deployments"} {
		_, out, _, _ = runDiscover(t, c, "--kind", q)
		if k := kinds(out); len(k) != 1 || k["Deployment"] == nil {
			t.Errorf("--kind %s: %v", q, k)
		}
	}
}

func TestDiscoverEmptyResultIsStated(t *testing.T) {
	c, _ := discoverFixture(t)
	code, out, _, _ := runDiscover(t, c, "--kind", "nosuchthing")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out["matched"] != float64(0) || !strings.Contains(out["note"].(string), "no kinds match") {
		t.Errorf("empty result not stated: %v", out)
	}
	if len(out["kinds"].([]any)) != 0 {
		t.Error("kinds should be an empty list")
	}
}

func TestDiscoverLimitStatesTruncation(t *testing.T) {
	c, _ := discoverFixture(t)
	_, out, _, _ := runDiscover(t, c, "--limit", "2")
	if out["matched"] != float64(4) || out["shown"] != float64(2) || len(out["kinds"].([]any)) != 2 {
		t.Errorf("counts: %v", out)
	}
	note := out["note"].(string)
	if !strings.Contains(note, "showing 2 of 4") || !strings.Contains(note, "--limit 0") {
		t.Errorf("truncation note: %q", note)
	}
	_, out, _, _ = runDiscover(t, c, "--limit", "0")
	if out["shown"] != float64(4) || out["note"] != nil {
		t.Errorf("--limit 0: %v", out)
	}
}

func TestDiscoverFullAddsShortNames(t *testing.T) {
	c, _ := discoverFixture(t)
	_, out, _, _ := runDiscover(t, c, "--full", "--kind", "pods")
	if k := kinds(out); k["Pod"]["short_names"] != "po" {
		t.Errorf("full row: %v", k["Pod"])
	}
}

func TestDiscoverPartialFailureWarns(t *testing.T) {
	c, fd := discoverFixture(t)
	fd.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("metrics.k8s.io/v1beta1: the server is currently unable to handle the request")
	})
	code, out, _, _ := runDiscover(t, c)
	if code != 0 || len(kinds(out)) == 0 || out["warnings"] == nil {
		t.Errorf("partial discovery: code=%d out=%v", code, out)
	}
}

func TestDiscoverTOONIsTabular(t *testing.T) {
	c, _ := discoverFixture(t)
	root, stdout, _ := testRoot(t, c, nil)
	root.SetArgs([]string{"discover"})
	if code := cli.Execute(root); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(stdout.String(), "kinds[4]{") {
		t.Errorf("expected tabular TOON rows:\n%s", stdout)
	}
}
