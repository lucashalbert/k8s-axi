package kube

import (
	"strings"
	"testing"
)

func TestPlanScaleChangesNothing(t *testing.T) {
	e := scaleEnv(t)
	code, out, errOut := run(t, e.c, "plan", "scale", "deploy/web", "--replicas", "0", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	plan := docs[0]["plan"].(map[string]any)
	if plan["mode"] != "dry-run" || len(plan["changes"].([]any)) != 2 {
		t.Errorf("plan = %v", plan)
	}
	res := docs[1]
	if r := res["result"].(map[string]any); r["status"] != "validated" {
		t.Errorf("result = %v", r)
	}
	next := res["next"].([]any)
	if len(next) != 1 || next[0] != "k8s-axi scale deployments.v1.apps/web --replicas 0 -n default" {
		t.Errorf("next = %v", next)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 3 {
		t.Errorf("plan changed the object: %d", got)
	}
	if len(e.dryRuns) != 1 {
		t.Errorf("want one server dry run, got %v", e.dryRuns)
	}
}

func TestPlanScaleNoOp(t *testing.T) {
	e := scaleEnv(t)
	code, out, _ := run(t, e.c, "plan", "scale", "deploy/web", "--replicas", "3", "-n", "default", "--json")
	if docs := jsonDocs(t, out); code != 0 || len(docs) != 1 || docs[0]["plan"].(map[string]any)["noop"] != true {
		t.Errorf("exit %d: %s", code, out)
	}
	if len(e.dryRuns) != 0 {
		t.Errorf("no-op plan still called the server: %v", e.dryRuns)
	}
}

func TestPlanRestartChangesNothing(t *testing.T) {
	e := restartEnv(t)
	code, out, errOut := run(t, e.c, "plan", "rollout", "restart", "sts/db", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	next := docs[1]["next"].([]any)
	if docs[0]["plan"].(map[string]any)["mode"] != "dry-run" || len(next) != 1 || next[0] != "k8s-axi rollout restart statefulsets.v1.apps/db -n default" {
		t.Errorf("docs = %v", docs)
	}
	if len(e.patches) != 0 || len(e.dryRuns) != 1 {
		t.Errorf("patches %v, dry runs %v", e.patches, e.dryRuns)
	}
}

func TestPlanDegradesWithoutServerDryRun(t *testing.T) {
	e := scaleEnv(t)
	e.noDry["deployments"] = true
	code, out, errOut := run(t, e.c, "plan", "scale", "deploy/web", "--replicas", "5", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	r := jsonDocs(t, out)[1]["result"].(map[string]any)
	if r["status"] != "not-validated" || !strings.Contains(r["note"].(string), "does not support dry-run") {
		t.Errorf("result = %v", r)
	}

	// The same server limit is an error when the caller asked for server validation.
	code, out, errOut = run(t, e.c, "scale", "deploy/web", "--replicas", "5", "--dry-run=server", "-n", "default")
	if code != 1 {
		t.Errorf("exit %d: %s", code, errOut)
	} else {
		requireStdoutError(t, out, errOut, "does not support dry-run")
	}
}

func TestPlanTakesNoDryRunFlag(t *testing.T) {
	e := scaleEnv(t)
	if code, out, errOut := run(t, e.c, "plan", "scale", "deploy/web", "--replicas", "5", "--dry-run=server", "-n", "default"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	} else {
		requireStdoutError(t, out, errOut)
	}
}

func TestPlanNeedsNamespace(t *testing.T) {
	e := scaleEnv(t)
	if code, out, errOut := run(t, e.c, "plan", "scale", "deploy/web", "--replicas", "5"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	} else {
		requireStdoutError(t, out, errOut)
	}
}
