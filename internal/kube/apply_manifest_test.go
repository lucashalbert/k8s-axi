package kube

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func ids(objs []manifestObject) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.obj.GetKind()+"/"+o.obj.GetName())
	}
	return out
}

func wantUsage(t *testing.T, err error, contains string) {
	t.Helper()
	var ae *axierr.Error
	if err == nil {
		t.Fatalf("want a usage error containing %q, got nil", contains)
	}
	if !errors.As(err, &ae) || ae.Code != 2 || !strings.Contains(ae.Msg, contains) {
		t.Fatalf("want usage error containing %q, got %v", contains, err)
	}
}

func TestReadManifestsMultiDocYAML(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "m.yaml", `
# leading comment
apiVersion: v1
kind: ConfigMap
metadata: {name: a}
---
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: b, namespace: prod}
spec: {replicas: 3}
`)
	objs, err := readManifests([]string{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(objs), ","); got != "ConfigMap/a,Deployment/b" {
		t.Errorf("objects = %s", got)
	}
	if n, _, _ := unstructured.NestedInt64(objs[1].obj.Object, "spec", "replicas"); n != 3 {
		t.Errorf("replicas = %v, want int64 3", objs[1].obj.Object["spec"])
	}
	if objs[1].source != p+" (document 2)" {
		t.Errorf("source = %q", objs[1].source)
	}
}

func TestReadManifestsJSONAndList(t *testing.T) {
	dir := t.TempDir()
	one := writeFile(t, dir, "one.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"j"}}`)
	stream := writeFile(t, dir, "stream.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"s1"}}{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"s2"}}`)
	list := writeFile(t, dir, "list.yaml", `
apiVersion: v1
kind: List
items:
- {apiVersion: v1, kind: Secret, metadata: {name: l1}}
- {apiVersion: v1, kind: Secret, metadata: {name: l2}}
`)
	objs, err := readManifests([]string{one, stream, list}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(objs), ","); got != "ConfigMap/j,ConfigMap/s1,ConfigMap/s2,Secret/l1,Secret/l2" {
		t.Errorf("objects = %s", got)
	}
}

func TestReadManifestsDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.yml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: b}\n")
	writeFile(t, dir, "a.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n")
	writeFile(t, dir, "c.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}`)
	writeFile(t, dir, "notes.txt", "not a manifest")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub"), "d.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: d}\n")
	objs, err := readManifests([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(objs), ","); got != "ConfigMap/a,ConfigMap/b,ConfigMap/c" {
		t.Errorf("objects = %s (sorted, non-recursive, manifests only)", got)
	}

	_, err = readManifests([]string{t.TempDir()}, nil)
	wantUsage(t, err, "no manifests")
}

func TestReadManifestsStdin(t *testing.T) {
	objs, err := readManifests([]string{"-"}, strings.NewReader("apiVersion: v1\nkind: Namespace\nmetadata: {name: x}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || !strings.HasPrefix(objs[0].source, "stdin") {
		t.Errorf("objs = %+v", objs)
	}
}

func TestReadManifestsRejects(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, body, want string }{
		{"no kind", "apiVersion: v1\nmetadata: {name: a}\n", "kind"},
		{"no apiVersion", "kind: ConfigMap\nmetadata: {name: a}\n", "apiVersion"},
		{"no name", "apiVersion: v1\nkind: ConfigMap\nmetadata: {labels: {a: b}}\n", "metadata.name"},
		{"no metadata", "apiVersion: v1\nkind: ConfigMap\n", "metadata.name"},
		{"generateName only", "apiVersion: v1\nkind: ConfigMap\nmetadata: {generateName: a-}\n", "generateName"},
		{"scalar document", "just a string\n", "not an object"},
		{"bad yaml", "apiVersion: v1\nkind: [\n", "document 1"},
		{"empty file", "# nothing\n---\n", "no objects"},
		{"bad list item", "apiVersion: v1\nkind: List\nitems: [1]\n", "not an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, dir, strings.ReplaceAll(tc.name, " ", "_")+".yaml", tc.body)
			_, err := readManifests([]string{p}, nil)
			wantUsage(t, err, tc.want)
		})
	}
	_, err := readManifests([]string{filepath.Join(dir, "missing.yaml")}, nil)
	wantUsage(t, err, "missing.yaml")
	_, err = readManifests(nil, nil)
	wantUsage(t, err, "no manifest given")
}

func TestReadManifestsSizeCap(t *testing.T) {
	old := maxManifestBytes
	maxManifestBytes = 64
	t.Cleanup(func() { maxManifestBytes = old })
	_, err := readManifests([]string{"-"}, strings.NewReader("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata: {k: "+strings.Repeat("x", 100)+"}\n"))
	wantUsage(t, err, "larger than")
}

func TestManifestErrorsNeverEchoValues(t *testing.T) {
	_, err := readManifests([]string{"-"}, strings.NewReader("apiVersion: v1\nkind: Secret\nmetadata: {name: s}\nstringData:\n  password: hunter2-super-secret\n  broken: [\n"))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v", err)
	}
}
