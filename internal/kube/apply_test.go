package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lucashalbert/axi-sdk-go/cli"
)

func schemaGVR(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

const cfgYAML = `apiVersion: v1
kind: ConfigMap
metadata: {name: cfg}
data: {a: "1"}
`

func webYAML(replicas int) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec: {replicas: " + string(rune('0'+replicas)) + "}\n"
}

func objStatuses(t *testing.T, res map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, o := range res["objects"].([]any) {
		m := o.(map[string]any)
		out[m["object"].(string)] = m["status"].(string)
	}
	return out
}

func TestApplyCreates(t *testing.T) {
	e := newApplyEnv(t)
	code, out, errOut := applyRun(t, e, manifest(t, cfgYAML), "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	if len(docs) != 2 {
		t.Fatalf("want plan and result, got %d:\n%s", len(docs), out)
	}
	plan := docs[0]["plan"].(map[string]any)
	if plan["operation"] != "apply" || plan["target"] != "configmap/cfg" || plan["mode"] != "apply" || plan["destructive"] != false {
		t.Errorf("plan = %v", plan)
	}
	if sc := plan["scope"].(map[string]any); sc["context"] != "kind-test" || sc["namespace"] != "default" || sc["server"] == nil {
		t.Errorf("scope = %v", sc)
	}
	if got := planChanges(t, out); len(got) != 1 || got[0] != "create configmap/cfg -n default" {
		t.Errorf("changes = %v", got)
	}
	res := docs[1]["result"].(map[string]any)
	if res["status"] != "applied" || objStatuses(t, res)["configmap/cfg"] != "created" {
		t.Errorf("result = %v", res)
	}
	if !e.has(configGVR, "default", "cfg") {
		t.Fatal("object was not stored")
	}
	calls := e.real()
	if len(calls) != 1 || calls[0].manager != "k8s-axi" || calls[0].force || calls[0].ns != "default" || calls[0].body.GetNamespace() != "default" {
		t.Errorf("real calls = %+v", calls)
	}
	if len(e.calls) != 2 || !e.calls[0].dry {
		t.Errorf("the plan must come from a dry-run request first: %+v", e.calls)
	}
}

func TestApplyConfiguresAndSkipsUnchanged(t *testing.T) {
	e := newApplyEnv(t,
		workload("apps/v1", "Deployment", "default", "web", 3, nil),
		&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cfg", "namespace": "default", "resourceVersion": "1"}, "data": map[string]any{"a": "1"}}},
	)
	file := manifest(t, webYAML(5)+"---\n"+cfgYAML)
	code, out, errOut := applyRun(t, e, file, "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	changes := planChanges(t, out)
	if len(changes) != 1 || changes[0] != "configure deployment/web -n default: spec.replicas" {
		t.Errorf("changes = %v (unchanged objects are not changes)", changes)
	}
	docs := jsonDocs(t, out)
	st := objStatuses(t, docs[1]["result"].(map[string]any))
	if st["deployment/web"] != "configured" || st["configmap/cfg"] != "unchanged" {
		t.Errorf("statuses = %v", st)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 5 {
		t.Errorf("replicas = %d, want 5", got)
	}
	if n := len(e.real()); n != 1 {
		t.Errorf("%d real requests, want 1: the unchanged object must not be sent", n)
	}
}

func TestApplyNoOp(t *testing.T) {
	e := newApplyEnv(t, workload("apps/v1", "Deployment", "default", "web", 3, nil))
	code, out, errOut := applyRun(t, e, manifest(t, webYAML(3)), "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	if plan := docs[0]["plan"].(map[string]any); plan["noop"] != true || len(planChanges(t, out)) != 0 {
		t.Errorf("plan = %v", plan)
	}
	if st := objStatuses(t, docs[1]["result"].(map[string]any)); st["deployment/web"] != "unchanged" {
		t.Errorf("statuses = %v", st)
	}
	if len(e.real()) != 0 {
		t.Errorf("a no-op must not write: %+v", e.real())
	}
}

func TestApplyConflictThenForce(t *testing.T) {
	e := newApplyEnv(t, workload("apps/v1", "Deployment", "default", "web", 3, nil))
	e.owners["deployments/web"] = map[string]string{".spec.replicas": "helm"}
	file := manifest(t, webYAML(5))

	code, out, errOut := applyRun(t, e, file, "-n", "default", "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errOut)
	}
	if got := planChanges(t, out); len(got) != 1 || !strings.HasPrefix(got[0], "conflict deployment/web -n default: .spec.replicas (owned by \"helm\")") {
		t.Errorf("changes = %v", got)
	}
	if !strings.Contains(errOut, "--force-conflicts") {
		t.Errorf("the error must name the flag: %s", errOut)
	}
	if st := objStatuses(t, jsonDocs(t, out)[1]["result"].(map[string]any)); st["deployment/web"] != "failed" {
		t.Errorf("statuses = %v", st)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 3 {
		t.Errorf("a conflict must change nothing, replicas = %d", got)
	}

	e.calls = nil
	code, out, errOut = applyRun(t, e, file, "-n", "default", "--force-conflicts", "--json")
	if code != 0 {
		t.Fatalf("forced: exit %d: %s", code, errOut)
	}
	if sc := jsonDocs(t, out)[0]["plan"].(map[string]any)["scope"].(map[string]any); sc["force_conflicts"] != "true" {
		t.Errorf("the plan must show force: %v", sc)
	}
	if calls := e.real(); len(calls) != 1 || !calls[0].force {
		t.Errorf("real calls = %+v", calls)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 5 {
		t.Errorf("replicas = %d, want 5", got)
	}
}

func TestApplyDryRunServerChangesNothing(t *testing.T) {
	e := newApplyEnv(t, workload("apps/v1", "Deployment", "default", "web", 3, nil))
	file := manifest(t, webYAML(5)+"---\n"+cfgYAML)
	code, out, errOut := applyRun(t, e, file, "-n", "default", "--dry-run=server", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	if docs[0]["plan"].(map[string]any)["mode"] != "dry-run" {
		t.Errorf("plan = %v", docs[0])
	}
	res := docs[1]["result"].(map[string]any)
	st := objStatuses(t, res)
	if res["status"] != "validated" || st["deployment/web"] != "would-configure" || st["configmap/cfg"] != "would-create" {
		t.Errorf("result = %v", res)
	}
	if len(e.real()) != 0 || e.replicas(deployGVR, "default", "web") != 3 || e.has(configGVR, "default", "cfg") {
		t.Errorf("a dry run changed the cluster: %+v", e.real())
	}
	next := docs[1]["next"].([]any)
	if len(next) == 0 || !strings.Contains(next[0].(string), "k8s-axi apply -f "+file) {
		t.Errorf("next = %v, want the apply command", next)
	}

	code, _, errOut = applyRun(t, e, file, "-n", "default", "--dry-run=client")
	if code != 2 || !strings.Contains(errOut, "--dry-run=server") {
		t.Errorf("client dry run: exit %d: %s", code, errOut)
	}
}

func TestPlanApplyNeverWrites(t *testing.T) {
	e := newApplyEnv(t)
	code, out, errOut := run(t, e.c, "plan", "apply", "-f", manifest(t, cfgYAML), "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if mode := jsonDocs(t, out)[0]["plan"].(map[string]any)["mode"]; mode != "dry-run" || len(e.real()) != 0 {
		t.Errorf("mode %v, real calls %v", mode, e.real())
	}
}

func TestApplyNamespaceRules(t *testing.T) {
	scoped := "apiVersion: v1\nkind: Namespace\nmetadata: {name: team, namespace: ignored}\n"
	cases := []struct {
		name, body string
		args       []string
		fromCtx    bool
		wantCode   int
		wantErr    string
		wantNS     string // namespace of the real apply request
	}{
		{"no namespace anywhere", cfgYAML, nil, false, 2, "ambiguous", ""},
		{"namespace from the context", cfgYAML, nil, true, 0, "", "default"},
		{"flag", cfgYAML, []string{"-n", "prod"}, false, 0, "", "prod"},
		{"manifest namespace", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg, namespace: stage}\n", nil, false, 0, "", "stage"},
		{"manifest and flag agree", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg, namespace: stage}\n", []string{"-n", "stage"}, false, 0, "", "stage"},
		{"manifest and flag differ", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg, namespace: stage}\n", []string{"-n", "prod"}, false, 2, "stage", ""},
		{"cluster scoped ignores -n and namespace", scoped, []string{"-n", "prod"}, false, 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newApplyEnv(t)
			e.c.NamespaceFromContext = tc.fromCtx
			code, _, errOut := applyRun(t, e, manifest(t, tc.body), append(tc.args, "--json")...)
			if code != tc.wantCode || !strings.Contains(errOut, tc.wantErr) {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if tc.wantCode != 0 {
				if len(e.calls) != 0 {
					t.Errorf("a usage error must send nothing: %+v", e.calls)
				}
				return
			}
			if got := e.real()[0]; got.ns != tc.wantNS || got.body.GetNamespace() != tc.wantNS {
				t.Errorf("applied to %q (body %q), want %q", got.ns, got.body.GetNamespace(), tc.wantNS)
			}
		})
	}
}

func TestApplyRejectsBeforeTouchingTheCluster(t *testing.T) {
	cases := map[string]string{
		"unknown kind":   "apiVersion: nope.example.com/v1\nkind: Nope\nmetadata: {name: a}\n",
		"no name":        "apiVersion: v1\nkind: ConfigMap\nmetadata: {labels: {a: b}}\n",
		"no kind":        "apiVersion: v1\nmetadata: {name: a}\n",
		"duplicate":      cfgYAML + "---\n" + cfgYAML,
		"valid then bad": cfgYAML + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := newApplyEnv(t)
			code, out, errOut := applyRun(t, e, manifest(t, body), "-n", "default")
			if code != 2 || out != "" || len(e.calls) != 0 {
				t.Errorf("exit %d, stdout %q, calls %v, stderr %s", code, out, e.calls, errOut)
			}
		})
	}
}

func TestApplyServedCustomResource(t *testing.T) {
	e := newApplyEnv(t)
	body := "apiVersion: example.com/v1\nkind: Gadget\nmetadata: {name: g1}\nspec: {size: 3}\n---\napiVersion: example.com/v1\nkind: ClusterThing\nmetadata: {name: c1}\n"
	code, out, errOut := applyRun(t, e, manifest(t, body), "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	st := objStatuses(t, jsonDocs(t, out)[1]["result"].(map[string]any))
	if st["gadget/g1"] != "created" || st["clusterthing/c1"] != "created" {
		t.Errorf("statuses = %v", st)
	}
}

const widgetCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: sprockets.acme.io}
spec:
  group: acme.io
  scope: Namespaced
  names: {kind: Sprocket, plural: sprockets, singular: sprocket}
  versions: [{name: v1, served: true, storage: true}]
`

const sprocket = "apiVersion: acme.io/v1\nkind: Sprocket\nmetadata: {name: s1}\nspec: {teeth: 9}\n"

func TestApplyCRDAndItsResourceInOneManifest(t *testing.T) {
	e := newApplyEnv(t)
	e.namespaces = map[string]bool{"default": true}
	old := servedPoll
	servedPoll = 0
	t.Cleanup(func() { servedPoll = old })
	// The custom resource comes first in the file; the CRD still goes first.
	nsBody := "apiVersion: v1\nkind: Namespace\nmetadata: {name: team}\n"
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg, namespace: team}\n"
	file := manifest(t, sprocket+"---\n"+cm+"---\n"+widgetCRD+"---\n"+nsBody)

	e.c.NamespaceFromContext = true
	code, out, errOut := applyRun(t, e, file, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	changes := planChanges(t, out)
	if len(changes) != 4 || !strings.HasPrefix(changes[0], "create namespace/team") || !strings.HasPrefix(changes[1], "create customresourcedefinition/sprockets.acme.io") {
		t.Errorf("changes = %v, want namespace and CRD first", changes)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "create sprocket/s1 -n default (after customresourcedefinition/sprockets.acme.io)") ||
		!strings.Contains(strings.Join(changes, "\n"), "create configmap/cfg -n team (after namespace/team)") {
		t.Errorf("pending objects must say what they wait for: %v", changes)
	}
	st := objStatuses(t, jsonDocs(t, out)[1]["result"].(map[string]any))
	for _, id := range []string{"namespace/team", "customresourcedefinition/sprockets.acme.io", "sprocket/s1", "configmap/cfg"} {
		if st[id] != "created" {
			t.Errorf("%s = %q in %v", id, st[id], st)
		}
	}
	if !e.has(schemaGVR("acme.io", "v1", "sprockets"), "default", "s1") || !e.has(configGVR, "team", "cfg") {
		t.Error("dependent objects were not stored")
	}
}

func TestApplyPendingObjectsAreNotValidatedInDryRun(t *testing.T) {
	e := newApplyEnv(t)
	code, out, errOut := applyRun(t, e, manifest(t, widgetCRD+"---\n"+sprocket), "-n", "default", "--dry-run=server", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(e.real()) != 0 {
		t.Fatalf("dry run wrote: %+v", e.real())
	}
	objs := jsonDocs(t, out)[1]["result"].(map[string]any)["objects"].([]any)
	var detail string
	for _, o := range objs {
		if m := o.(map[string]any); m["object"] == "sprocket/s1" {
			detail, _ = m["detail"].(string)
		}
	}
	if !strings.Contains(detail, "not validated") {
		t.Errorf("detail = %q", detail)
	}
}

func TestApplyPartialFailure(t *testing.T) {
	e := newApplyEnv(t)
	e.invalid["deployments/bad"] = `Deployment.apps "bad" is invalid: spec.replicas: Invalid value: -1`
	body := cfgYAML + "---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: bad}\nspec: {replicas: 1}\n"
	code, out, errOut := applyRun(t, e, manifest(t, body), "-n", "default", "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "1 of 2") {
		t.Errorf("stderr = %s", errOut)
	}
	docs := jsonDocs(t, out)
	res := docs[1]["result"].(map[string]any)
	st := objStatuses(t, res)
	if st["configmap/cfg"] != "created" || st["deployment/bad"] != "failed" || res["failed"] != float64(1) || res["created"] != float64(1) || res["status"] != "partial" {
		t.Errorf("result = %v", res)
	}
	if !e.has(configGVR, "default", "cfg") {
		t.Error("the valid object must still be applied")
	}
}

func TestApplyTimeoutIsReportedPerObject(t *testing.T) {
	e := newApplyEnv(t)
	e.failOn["patch configmaps"] = context.DeadlineExceeded
	code, _, errOut := applyRun(t, e, manifest(t, cfgYAML), "-n", "default")
	if code != 1 || !strings.Contains(errOut, "--timeout") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestApplyDryRunUnsupportedIsNotValidated(t *testing.T) {
	e := newApplyEnv(t)
	e.noDry["configmaps"] = true
	code, out, errOut := applyRun(t, e, manifest(t, cfgYAML), "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := planChanges(t, out); len(got) != 1 || !strings.Contains(got[0], "not validated") {
		t.Errorf("changes = %v", got)
	}
}

func TestApplyStdin(t *testing.T) {
	e := newApplyEnv(t)
	root, out, errb := testRoot(t, e.c, nil)
	root.SetIn(strings.NewReader(cfgYAML))
	root.SetArgs([]string{"apply", "-f", "-", "-n", "default", "--json"})
	if code := cli.Execute(root); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out.String(), "configmap/cfg") || !e.has(configGVR, "default", "cfg") {
		t.Errorf("stdout = %s", out)
	}
}

func TestApplyNeedsAFile(t *testing.T) {
	e := newApplyEnv(t)
	code, _, errOut := run(t, e.c, "apply", "-n", "default")
	if code != 2 || !strings.Contains(errOut, "-f") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// A Secret's values never reach any output, in any format, in the plan, the
// result or an error, even when the server echoes them back.
func TestApplyNeverEchoesSecretValues(t *testing.T) {
	const value = "hunter2-super-secret"
	secret := "apiVersion: v1\nkind: Secret\nmetadata: {name: db}\nstringData: {password: " + value + ", user: admin}\n"
	for _, format := range []string{"--json", "--yaml", ""} {
		for _, bad := range []bool{false, true} {
			e := newApplyEnv(t)
			if bad {
				e.invalid["secrets/db"] = `Secret "db" is invalid: stringData[password]: Invalid value: "` + value + `"`
			}
			args := []string{"-n", "default"}
			if format != "" {
				args = append(args, format)
			}
			_, out, errOut := applyRun(t, e, manifest(t, secret), args...)
			if strings.Contains(out, value) || strings.Contains(errOut, value) {
				t.Errorf("format %q bad=%v leaked the value:\n%s\n%s", format, bad, out, errOut)
			}
			if !bad && (!strings.Contains(out, "password 20 B") || !strings.Contains(out, "user 5 B")) {
				t.Errorf("format %q bad=%v: want keys and sizes:\n%s", format, bad, out)
			}
		}
	}
	// A secret value that is also a credential-looking string in a reconfigure.
	e := newApplyEnv(t)
	live := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "db", "namespace": "default", "resourceVersion": "1"}, "stringData": map[string]any{"password": "old-" + value}}}
	if err := e.dyn.Tracker().Add(live); err != nil {
		t.Fatal(err)
	}
	_, out, errOut := applyRun(t, e, manifest(t, secret), "-n", "default", "--json")
	if strings.Contains(out+errOut, value) {
		t.Errorf("reconfigure leaked:\n%s\n%s", out, errOut)
	}
}

func TestApplyErrorForUnreachableClients(t *testing.T) {
	root, _, errb := testRoot(t, nil, errors.New("no kubeconfig"))
	root.SetArgs([]string{"apply", "-f", manifest(t, cfgYAML), "-n", "default"})
	if code := cli.Execute(root); code != 1 || !strings.Contains(errb.String(), "no kubeconfig") {
		t.Errorf("stderr = %s", errb)
	}
}

func TestApplySkipsWhatWaitsForAFailedCRD(t *testing.T) {
	e := newApplyEnv(t)
	e.failOn["patch customresourcedefinitions"] = errors.New("boom")
	code, out, errOut := applyRun(t, e, manifest(t, widgetCRD+"---\n"+sprocket+"---\n"+cfgYAML), "-n", "default", "--json")
	if code != 1 || !strings.Contains(errOut, "2 of 3") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	st := objStatuses(t, jsonDocs(t, out)[1]["result"].(map[string]any))
	if st["customresourcedefinition/sprockets.acme.io"] != "failed" || st["sprocket/s1"] != "failed" || st["configmap/cfg"] != "created" {
		t.Errorf("statuses = %v", st)
	}
}

func TestApplyForbiddenNamesTheVerb(t *testing.T) {
	e := newApplyEnv(t)
	e.failOn["patch configmaps"] = apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "cfg", errors.New("no"))
	code, _, errOut := applyRun(t, e, manifest(t, cfgYAML), "-n", "default")
	if code != 1 || !strings.Contains(errOut, "RBAC denied 'create' on configmaps") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestApplyListsFivePathsUnlessFull(t *testing.T) {
	live := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cfg", "namespace": "default", "resourceVersion": "1"}, "data": map[string]any{}}}
	body := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\ndata: {a: '1', b: '1', c: '1', d: '1', e: '1', f: '1', g: '1'}\n"
	for _, tc := range []struct {
		args []string
		want string
	}{{nil, "+2 more"}, {[]string{"--full"}, "data.g"}} {
		e := newApplyEnv(t, live.DeepCopy())
		_, out, errOut := applyRun(t, e, manifest(t, body), append([]string{"-n", "default", "--dry-run=server", "--json"}, tc.args...)...)
		if got := strings.Join(planChanges(t, out), "\n"); !strings.Contains(got, tc.want) {
			t.Errorf("%v: changes = %q (%s)", tc.args, got, errOut)
		}
	}
}
