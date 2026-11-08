package kube

import (
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const stamp = "2026-11-07T10:00:00Z"

func restartEnv(t *testing.T) *mutEnv {
	t.Helper()
	old := now
	now = func() time.Time { return time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = old })

	ds := workload("apps/v1", "DaemonSet", "default", "agent", 0, nil)
	unstructured.RemoveNestedField(ds.Object, "spec", "replicas")
	_ = unstructured.SetNestedField(ds.Object, int64(4), "status", "desiredNumberScheduled")
	return newMutEnv(t,
		workload("apps/v1", "Deployment", "default", "web", 3, nil),
		workload("apps/v1", "Deployment", "default", "paused", 3, map[string]any{"paused": true}),
		workload("apps/v1", "Deployment", "default", "recreate", 2, map[string]any{"strategy": map[string]any{"type": "Recreate"}}),
		workload("apps/v1", "Deployment", "default", "idle", 0, nil),
		workload("apps/v1", "StatefulSet", "default", "db", 2, nil),
		workload("apps/v1", "StatefulSet", "default", "manual", 2, map[string]any{"updateStrategy": map[string]any{"type": "OnDelete"}}),
		workload("apps/v1", "Deployment", "default", "again", 1, map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": "2026-01-01T00:00:00Z"}}}}),
		ds,
		workload("apps/v1", "ReplicaSet", "default", "rs", 1, nil),
	)
}

func planChanges(t *testing.T, out string) []string {
	t.Helper()
	var got []string
	for _, c := range jsonDocs(t, out)[0]["plan"].(map[string]any)["changes"].([]any) {
		got = append(got, c.(string))
	}
	return got
}

func TestRestartApplies(t *testing.T) {
	cases := []struct {
		args []string
		gvr  schema.GroupVersionResource
		name string
		want string
		hint string
	}{
		{[]string{"rollout", "restart", "deploy/web"}, deployGVR, "web", "deployment/web", "deployments.v1.apps/web"},
		{[]string{"rollout", "restart", "sts", "db"}, stsGVR, "db", "statefulset/db", "statefulsets.v1.apps/db"},
		{[]string{"rollout", "restart", "daemonsets/agent"}, dsGVR, "agent", "daemonset/agent", "daemonsets.v1.apps/agent"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			e := restartEnv(t)
			code, out, errOut := run(t, e.c, append(tc.args, "-n", "default", "--json")...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			docs := jsonDocs(t, out)
			if len(docs) != 2 {
				t.Fatalf("want plan and result, got %d:\n%s", len(docs), out)
			}
			plan := docs[0]["plan"].(map[string]any)
			if plan["operation"] != "rollout restart" || plan["target"] != tc.want || plan["mode"] != "apply" || plan["destructive"] != false {
				t.Errorf("plan = %v", plan)
			}
			if sc := plan["scope"].(map[string]any); sc["context"] != "kind-test" || sc["namespace"] != "default" {
				t.Errorf("scope = %v", sc)
			}
			res := docs[1]["result"].(map[string]any)
			if res["status"] != "applied" || res["restarted_at"] != stamp {
				t.Errorf("result = %v", res)
			}
			got, _, _ := unstructured.NestedString(e.object(tc.gvr, "default", tc.name).Object, "spec", "template", "metadata", "annotations", restartedAtKey)
			if got != stamp {
				t.Errorf("stored annotation = %q", got)
			}
			next := docs[1]["next"].([]any)
			if len(next) != 2 || next[0] != "k8s-axi inspect "+tc.hint+" -n default" || next[1] != "k8s-axi events "+tc.hint+" -n default" {
				t.Errorf("next = %v", next)
			}
			if len(e.patches) != 1 || e.patches[0] != `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"`+stamp+`"}}}}}` {
				t.Errorf("patches = %v", e.patches)
			}
		})
	}
}

func TestRestartPlanChanges(t *testing.T) {
	cases := []struct {
		target string
		want   []string
	}{
		{"deploy/web", []string{"set pod template annotation " + restartedAtKey + "=" + stamp, "replaces 3 pod(s) with a rolling update"}},
		{"deploy/again", []string{"set pod template annotation " + restartedAtKey + "=" + stamp + " (was 2026-01-01T00:00:00Z)", "replaces 1 pod(s) with a rolling update"}},
		{"daemonset/agent", []string{"set pod template annotation " + restartedAtKey + "=" + stamp, "replaces 4 pod(s) with a rolling update"}},
		{"deploy/recreate", []string{"set pod template annotation " + restartedAtKey + "=" + stamp, "replaces 2 pod(s)", "notable: Recreate strategy, all pods stop before new ones start (downtime)"}},
		{"sts/manual", []string{"set pod template annotation " + restartedAtKey + "=" + stamp, "notable: updateStrategy is OnDelete, no pod is replaced until it is deleted"}},
		{"deploy/idle", []string{"set pod template annotation " + restartedAtKey + "=" + stamp, "notable: 0 replicas, no running pod is restarted"}},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			e := restartEnv(t)
			code, out, errOut := run(t, e.c, "rollout", "restart", tc.target, "-n", "default", "--json")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if got := strings.Join(planChanges(t, out), "\n"); got != strings.Join(tc.want, "\n") {
				t.Errorf("changes:\n%s\nwant:\n%s", got, strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestRestartDryRunServer(t *testing.T) {
	e := restartEnv(t)
	code, out, errOut := run(t, e.c, "rollout", "restart", "deploy/web", "--dry-run=server", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	docs := jsonDocs(t, out)
	if docs[0]["plan"].(map[string]any)["mode"] != "dry-run" || docs[1]["result"].(map[string]any)["status"] != "validated" {
		t.Errorf("docs = %v", docs)
	}
	if len(e.dryRuns) != 1 || len(e.patches) != 0 {
		t.Errorf("dry runs %v, real patches %v", e.dryRuns, e.patches)
	}
	if _, found, _ := unstructured.NestedString(e.object(deployGVR, "default", "web").Object, "spec", "template", "metadata", "annotations", restartedAtKey); found {
		t.Error("dry run changed the object")
	}
}

func TestRestartRefusals(t *testing.T) {
	cases := map[string]struct {
		args []string
		code int
	}{
		"paused":           {[]string{"rollout", "restart", "deploy/paused", "-n", "default"}, 1},
		"replicaset":       {[]string{"rollout", "restart", "rs/rs", "-n", "default"}, 2},
		"pod":              {[]string{"rollout", "restart", "pod/x", "-n", "default"}, 2},
		"client dry-run":   {[]string{"rollout", "restart", "deploy/web", "--dry-run=client", "-n", "default"}, 2},
		"empty dry-run":    {[]string{"rollout", "restart", "deploy/web", "--dry-run=", "-n", "default"}, 2},
		"no namespace":     {[]string{"rollout", "restart", "deploy/web"}, 2},
		"no name":          {[]string{"rollout", "restart", "deploy", "-n", "default"}, 2},
		"missing workload": {[]string{"rollout", "restart", "deploy/nope", "-n", "default"}, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := restartEnv(t)
			code, out, errOut := run(t, e.c, tc.args...)
			if code != tc.code || out != "" || !strings.Contains(errOut, "hint") {
				t.Fatalf("exit %d (want %d), stdout %q, stderr %s", code, tc.code, out, errOut)
			}
			if len(e.patches)+len(e.dryRuns) != 0 {
				t.Errorf("refused restart still sent a patch")
			}
		})
	}
}

func TestRestartForbidden(t *testing.T) {
	e := restartEnv(t)
	e.failOn["patch deployments"] = apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "web", nil)
	code, _, errOut := run(t, e.c, "rollout", "restart", "deploy/web", "-n", "default")
	if code != 1 || !strings.Contains(errOut, "RBAC denied 'patch'") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}
