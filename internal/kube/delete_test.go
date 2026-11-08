package kube

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

type deleteCall struct {
	resource, ns, name string
	policy             metav1.DeletionPropagation
	uid                string
	dry                bool
}

// delEnv adds delete semantics to the fake cluster: dry-run deletes keep the
// object, a UID precondition is enforced, and finalizers leave the object
// terminating instead of removing it.
type delEnv struct {
	*applyEnv
	deletes      []deleteCall
	beforeDelete func() // runs once, just before a delete is checked
}

func newDelEnv(t *testing.T, objs ...runtime.Object) *delEnv {
	t.Helper()
	e := &delEnv{applyEnv: newApplyEnv(t, objs...)}
	e.dyn.PrependReactor("delete", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		act := a.(clienttesting.DeleteActionImpl)
		gvr := act.GetResource()
		o := act.DeleteOptions
		call := deleteCall{resource: gvr.Resource, ns: act.GetNamespace(), name: act.GetName(), dry: len(o.DryRun) > 0}
		if o.PropagationPolicy != nil {
			call.policy = *o.PropagationPolicy
		}
		if o.Preconditions != nil && o.Preconditions.UID != nil {
			call.uid = string(*o.Preconditions.UID)
		}
		e.deletes = append(e.deletes, call)
		if f := e.beforeDelete; f != nil {
			e.beforeDelete = nil
			f()
		}
		if err, ok := e.failOn["delete "+gvr.Resource]; ok {
			return true, nil, err
		}
		live, err := e.dyn.Tracker().Get(gvr, act.GetNamespace(), act.GetName())
		if err != nil {
			return true, nil, err
		}
		u := live.(*unstructured.Unstructured)
		if call.uid != "" && call.uid != string(u.GetUID()) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, act.GetName(), nil)
		}
		if call.dry {
			return true, nil, nil
		}
		if len(u.GetFinalizers()) > 0 {
			cp := u.DeepCopy()
			cp.SetDeletionTimestamp(&metav1.Time{Time: now()})
			return true, nil, e.dyn.Tracker().Update(gvr, cp, act.GetNamespace())
		}
		return false, nil, nil
	})
	return e
}

func (e *delEnv) realDeletes() []deleteCall {
	var out []deleteCall
	for _, d := range e.deletes {
		if !d.dry {
			out = append(out, d)
		}
	}
	return out
}

func (e *delEnv) read(resource string) bool {
	for _, a := range e.dyn.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == resource {
			return true
		}
	}
	return false
}

func delFixture(t *testing.T) *delEnv {
	t.Helper()
	withFinalizer := workload("apps/v1", "Deployment", "default", "stuck", 1, nil)
	withFinalizer.SetFinalizers([]string{"example.com/cleanup"})
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "gadgets.example.com", "uid": "uid-crd"},
		"spec":     map[string]any{"group": "example.com", "names": map[string]any{"kind": "Gadget", "plural": "gadgets"}},
	}}
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "team", "uid": "uid-team"}}}
	return newDelEnv(t, workload("apps/v1", "Deployment", "default", "web", 3, nil), withFinalizer, crd, ns)
}

func delRun(t *testing.T, e *delEnv, args ...string) (int, string, string) {
	t.Helper()
	return run(t, e.c, append([]string{"delete"}, args...)...)
}

func TestDeleteNeedsTheConfirmToken(t *testing.T) {
	cases := map[string][]string{
		"missing":          {"deploy/web", "-n", "default"},
		"abbreviated kind": {"deploy/web", "-n", "default", "--confirm", "deploy/web"},
		"other name":       {"deploy/web", "-n", "default", "--confirm", "deployment/other"},
		"other kind":       {"deploy/web", "-n", "default", "--confirm", "service/web"},
		"wrong case":       {"deploy/web", "-n", "default", "--confirm", "Deployment/web"},
		"empty":            {"deploy/web", "-n", "default", "--confirm", ""},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			e := delFixture(t)
			code, out, errOut := delRun(t, e, args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2: %s", code, errOut)
			}
			if !strings.Contains(errOut, "deployment/web") || !strings.Contains(errOut, "--confirm") {
				t.Errorf("the error must name the exact token and flag: %s", errOut)
			}
			if out != "" || len(e.deletes) != 0 || !e.has(deployGVR, "default", "web") {
				t.Errorf("nothing may be deleted: stdout %q, deletes %v", out, e.deletes)
			}
			if e.read("deployments") {
				t.Errorf("an unauthorized delete must not even read the object: %v", e.dyn.Actions())
			}
		})
	}
}

func TestDeleteWithTheRightToken(t *testing.T) {
	e := delFixture(t)
	code, out, errOut := delRun(t, e, "deploy/web", "-n", "default", "--confirm", "deployment/web", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	plan := docs[0]["plan"].(map[string]any)
	if plan["operation"] != "delete" || plan["target"] != "deployment/web" || plan["destructive"] != true || plan["confirm"] != "deployment/web" || plan["mode"] != "apply" {
		t.Errorf("plan = %v", plan)
	}
	if sc := plan["scope"].(map[string]any); sc["context"] != "kind-test" || sc["namespace"] != "default" || sc["resource"] != "deployments.v1.apps" {
		t.Errorf("scope = %v", sc)
	}
	if got := planChanges(t, out); len(got) != 1 || got[0] != "delete deployment/web -n default (propagation: background)" {
		t.Errorf("changes = %v", got)
	}
	res := docs[1]["result"].(map[string]any)
	if res["status"] != "deleted" || res["target"] != "deployment/web" || res["propagation"] != "background" {
		t.Errorf("result = %v", res)
	}
	if e.has(deployGVR, "default", "web") {
		t.Error("the deployment is still there")
	}
	if !e.has(deployGVR, "default", "stuck") {
		t.Error("delete removed deployment/stuck")
	}
	if !e.has(nsGVR, "", "team") {
		t.Error("delete removed namespace/team")
	}
	if !e.has(crdGVR, "", "gadgets.example.com") {
		t.Error("delete removed customresourcedefinition/gadgets.example.com")
	}
	calls := e.realDeletes()
	if len(calls) != 1 || calls[0].policy != metav1.DeletePropagationBackground || calls[0].uid != "uid-web" {
		t.Errorf("deletes = %+v, want background with the UID precondition", calls)
	}
}

func TestDeletePropagation(t *testing.T) {
	for flag, want := range map[string]metav1.DeletionPropagation{"foreground": metav1.DeletePropagationForeground, "orphan": metav1.DeletePropagationOrphan} {
		e := delFixture(t)
		code, out, errOut := delRun(t, e, "deploy/web", "-n", "default", "--confirm", "deployment/web", "--propagation", flag, "--json")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", flag, code, errOut)
		}
		if c := e.realDeletes(); len(c) != 1 || c[0].policy != want {
			t.Errorf("%s: deletes = %+v", flag, c)
		}
		if got := planChanges(t, out); len(got) < 2 || !strings.HasPrefix(got[1], "notable:") {
			t.Errorf("%s: want a notable line, got %v", flag, got)
		}
	}
	e := delFixture(t)
	code, _, errOut := delRun(t, e, "deploy/web", "-n", "default", "--confirm", "deployment/web", "--propagation", "now")
	if code != 2 || !strings.Contains(errOut, "background") || len(e.deletes) != 0 {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestDeleteWarnsForNamespacesAndCRDs(t *testing.T) {
	e := delFixture(t)
	_, out, errOut := delRun(t, e, "namespace/team", "--confirm", "namespace/team", "--dry-run=server", "--json")
	if got := planChanges(t, out); len(got) < 2 || !strings.HasPrefix(got[1], "WARNING:") || !strings.Contains(got[1], "every object") {
		t.Errorf("namespace changes = %v\n%s", got, errOut)
	}
	_, out, errOut = delRun(t, e, "crd/gadgets.example.com", "--confirm", "customresourcedefinition/gadgets.example.com", "--dry-run=server", "--json")
	got := planChanges(t, out)
	if len(got) < 2 || !strings.HasPrefix(got[1], "WARNING:") || !strings.Contains(got[1], "Gadget") {
		t.Errorf("crd changes = %v\n%s", got, errOut)
	}
	_, out, _ = delRun(t, e, "deploy/web", "-n", "default", "--confirm", "deployment/web", "--dry-run=server", "--json")
	for _, c := range planChanges(t, out) {
		if strings.Contains(c, "WARNING") {
			t.Errorf("a deployment delete must not warn: %s", c)
		}
	}
}

func TestDeleteRefusesBroadForms(t *testing.T) {
	cases := map[string][]string{
		"--all":         {"deploy", "--all", "-n", "default", "--confirm", "deployment/web"},
		"selector":      {"deploy", "-l", "app=web", "-n", "default"},
		"long selector": {"deploy", "--selector", "app=web", "-n", "default"},
		"star":          {"deploy/*", "-n", "default", "--confirm", "deployment/*"},
		"question":      {"deploy/we?", "-n", "default", "--confirm", "deployment/we?"},
		"no name":       {"deploy", "-n", "default", "--confirm", "deployment/"},
		"two targets":   {"deploy/a", "deploy/b", "-n", "default"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			e := delFixture(t)
			code, out, errOut := delRun(t, e, args...)
			if code != 2 || out != "" || len(e.deletes) != 0 {
				t.Errorf("exit %d, stdout %q, deletes %v: %s", code, out, e.deletes, errOut)
			}
		})
	}
}

func TestDeleteDryRunServer(t *testing.T) {
	e := delFixture(t)
	code, out, errOut := delRun(t, e, "deploy/web", "-n", "default", "--dry-run=server", "--json")
	if code != 0 {
		t.Fatalf("a dry run needs no token: exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	if docs[0]["plan"].(map[string]any)["mode"] != "dry-run" || docs[1]["result"].(map[string]any)["status"] != "validated" {
		t.Errorf("docs = %v", docs)
	}
	if len(e.realDeletes()) != 0 || len(e.deletes) != 1 || !e.has(deployGVR, "default", "web") {
		t.Errorf("a dry run deleted something: %+v", e.deletes)
	}
	next := docs[1]["next"].([]any)
	if len(next) != 1 || !strings.Contains(next[0].(string), "delete deployments.v1.apps/web --confirm deployment/web") || !strings.Contains(next[0].(string), "-n default") {
		t.Errorf("next = %v, want the exact delete command", next)
	}

	code, _, errOut = delRun(t, e, "deploy/web", "-n", "default", "--dry-run=client")
	if code != 2 || !strings.Contains(errOut, "--dry-run=server") {
		t.Errorf("client dry run: exit %d: %s", code, errOut)
	}
}

func TestPlanDelete(t *testing.T) {
	e := delFixture(t)
	code, out, errOut := run(t, e.c, "plan", "delete", "deploy/web", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(e.realDeletes()) != 0 || !e.has(deployGVR, "default", "web") {
		t.Error("plan deleted something")
	}
	if !strings.Contains(out, "--confirm deployment/web") {
		t.Errorf("plan must end with the exact command:\n%s", out)
	}
}

func TestDeleteTerminatingObject(t *testing.T) {
	e := delFixture(t)
	code, out, errOut := delRun(t, e, "deploy/stuck", "-n", "default", "--confirm", "deployment/stuck", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := planChanges(t, out); len(got) < 2 || !strings.Contains(got[1], "example.com/cleanup") {
		t.Errorf("finalizers must be called out: %v", got)
	}
	res := jsonDocs(t, out)[1]["result"].(map[string]any)
	if res["status"] != "terminating" {
		t.Errorf("result = %v", res)
	}
}

func TestDeleteFailures(t *testing.T) {
	e := delFixture(t)
	code, _, errOut := delRun(t, e, "deploy/ghost", "-n", "default", "--confirm", "deployment/ghost")
	if code != 1 || !strings.Contains(errOut, "not found") && !strings.Contains(errOut, "check the name") {
		t.Errorf("not found: exit %d: %s", code, errOut)
	}
	e = delFixture(t)
	e.failOn["delete deployments"] = apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", nil)
	code, _, errOut = delRun(t, e, "deploy/web", "-n", "default", "--confirm", "deployment/web")
	if code != 1 || !strings.Contains(errOut, "RBAC denied 'delete'") {
		t.Errorf("forbidden: exit %d: %s", code, errOut)
	}
	// The object is replaced between the plan and the delete: the UID
	// precondition stops the stale delete from removing the new one.
	e = delFixture(t)
	e.beforeDelete = func() {
		o := e.object(deployGVR, "default", "web").DeepCopy()
		o.SetUID("uid-new")
		if err := e.dyn.Tracker().Update(deployGVR, o, "default"); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errOut = delRun(t, e, "deploy/web", "-n", "default", "--confirm", "deployment/web")
	if code != 1 || !e.has(deployGVR, "default", "web") || !strings.Contains(errOut, "re-run") {
		t.Errorf("stale uid: exit %d: %s", code, errOut)
	}
}
