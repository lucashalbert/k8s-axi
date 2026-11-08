package kube

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func scaleEnv(t *testing.T) *mutEnv {
	return newMutEnv(t,
		workload("apps/v1", "Deployment", "default", "web", 3, nil),
		workload("apps/v1", "StatefulSet", "default", "db", 2, nil),
		workload("apps/v1", "ReplicaSet", "default", "rs", 1, nil),
		workload("apps/v1", "DaemonSet", "default", "agent", 0, nil),
		workload("example.com/v1", "Gadget", "default", "g", 1, nil),
	)
}

func TestScaleApplies(t *testing.T) {
	cases := []struct {
		args []string
		gvr  schema.GroupVersionResource
		name string
		want string // plan target
	}{
		{[]string{"scale", "deploy/web", "--replicas", "5"}, deployGVR, "web", "deployment/web"},
		{[]string{"scale", "statefulset", "db", "--replicas", "4"}, stsGVR, "db", "statefulset/db"},
		{[]string{"scale", "rs/rs", "--replicas", "2"}, rsGVR, "rs", "replicaset/rs"},
		{[]string{"scale", "gadgets/g", "--replicas", "7"}, gadgetGVR, "g", "gadget/g"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			e := scaleEnv(t)
			code, out, errOut := run(t, e.c, append(tc.args, "-n", "default", "--json")...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			docs := jsonDocs(t, out)
			if len(docs) != 2 {
				t.Fatalf("want plan and result documents, got %d:\n%s", len(docs), out)
			}
			plan := docs[0]["plan"].(map[string]any)
			scope := plan["scope"].(map[string]any)
			if plan["operation"] != "scale" || plan["target"] != tc.want || plan["mode"] != "apply" || plan["destructive"] != false {
				t.Errorf("plan = %v", plan)
			}
			if scope["context"] != "kind-test" || scope["namespace"] != "default" || scope["resource"] == "" {
				t.Errorf("scope = %v", scope)
			}
			res := docs[1]["result"].(map[string]any)
			if res["status"] != "applied" || res["target"] != tc.want {
				t.Errorf("result = %v", res)
			}
			want := e.replicas(tc.gvr, "default", tc.name)
			if float64(want) != res["replicas"] {
				t.Errorf("stored %d, result %v", want, res["replicas"])
			}
			if len(e.dryRuns) != 0 {
				t.Errorf("unexpected dry-run requests: %v", e.dryRuns)
			}
		})
	}
}

func TestScalePlanShowsChange(t *testing.T) {
	e := scaleEnv(t)
	_, out, _ := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default", "--json")
	changes := jsonDocs(t, out)[0]["plan"].(map[string]any)["changes"].([]any)
	if len(changes) != 1 || changes[0] != "replicas 3 -> 5" {
		t.Errorf("changes = %v", changes)
	}
}

func TestScaleToZeroIsNotable(t *testing.T) {
	e := scaleEnv(t)
	code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "0", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	changes := jsonDocs(t, out)[0]["plan"].(map[string]any)["changes"].([]any)
	if len(changes) != 2 || changes[0] != "replicas 3 -> 0" || !strings.HasPrefix(changes[1].(string), "notable: scales to zero") {
		t.Errorf("changes = %v", changes)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 0 {
		t.Errorf("stored replicas = %d", got)
	}
}

func TestScaleNoOp(t *testing.T) {
	e := scaleEnv(t)
	code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "3", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	if len(docs) != 1 || docs[0]["plan"].(map[string]any)["noop"] != true {
		t.Errorf("want a single no-op plan: %s", out)
	}
	for _, a := range e.dyn.Actions() {
		if a.GetVerb() == "update" {
			t.Errorf("no-op scale sent %v", a)
		}
	}
}

func TestScaleDryRunServer(t *testing.T) {
	e := scaleEnv(t)
	code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "9", "--dry-run=server", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	if docs[0]["plan"].(map[string]any)["mode"] != "dry-run" {
		t.Errorf("plan = %v", docs[0])
	}
	if docs[1]["result"].(map[string]any)["status"] != "validated" {
		t.Errorf("result = %v", docs[1])
	}
	if len(e.dryRuns) != 1 || e.dryRuns[0] != "update deployments/web/scale" {
		t.Errorf("dry runs = %v", e.dryRuns)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 3 {
		t.Errorf("dry run changed the object: replicas = %d", got)
	}
}

func TestScaleUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"client dry-run":      {"scale", "deploy/web", "--replicas", "2", "--dry-run=client", "-n", "default"},
		"bogus dry-run":       {"scale", "deploy/web", "--replicas", "2", "--dry-run=maybe", "-n", "default"},
		"missing replicas":    {"scale", "deploy/web", "-n", "default"},
		"negative replicas":   {"scale", "deploy/web", "--replicas", "-1", "-n", "default"},
		"no name":             {"scale", "deploy", "--replicas", "2", "-n", "default"},
		"no scale subres":     {"scale", "daemonset/agent", "--replicas", "2", "-n", "default"},
		"unknown kind":        {"scale", "nopes/x", "--replicas", "2", "-n", "default"},
		"ambiguous namespace": {"scale", "deploy/web", "--replicas", "2"},
		"cluster-scoped -n":   {"scale", "clusterthing/x", "--replicas", "2", "-n", "default"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			e := scaleEnv(t)
			code, out, errOut := run(t, e.c, args...)
			if code != 2 || out != "" {
				t.Fatalf("exit %d, stdout %q, stderr %s", code, out, errOut)
			}
			if !strings.Contains(errOut, "hint") {
				t.Errorf("no hint: %s", errOut)
			}
			for _, a := range e.dyn.Actions() {
				if a.GetVerb() == "update" || a.GetVerb() == "patch" {
					t.Errorf("usage error still sent %v", a)
				}
			}
		})
	}
}

func TestScaleNamespaceFromContext(t *testing.T) {
	e := scaleEnv(t)
	e.c.NamespaceFromContext = true
	if code, _, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "4", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 4 {
		t.Errorf("replicas = %d", got)
	}
}

func TestScaleAPIErrors(t *testing.T) {
	cases := map[string]struct {
		verb string
		err  error
		want string
	}{
		"conflict":  {"update", apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "web", nil), "re-run"},
		"forbidden": {"update", apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "web", nil), "RBAC denied"},
		"not found": {"get", apierrors.NewNotFound(schema.GroupResource{Resource: "deployments"}, "web"), "check the name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := scaleEnv(t)
			e.failOn[tc.verb+" deployments"] = tc.err
			code, _, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default")
			if code != 1 || !strings.Contains(errOut, tc.want) {
				t.Errorf("exit %d, want hint %q: %s", code, tc.want, errOut)
			}
		})
	}
}

func TestScaleTOON(t *testing.T) {
	e := scaleEnv(t)
	code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"plan:", "target: deployment/web", "replicas 3 -> 5", "result:", "status: applied", "next["} {
		if !strings.Contains(out, want) {
			t.Errorf("TOON output lacks %q:\n%s", want, out)
		}
	}
}
