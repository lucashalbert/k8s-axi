package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/restmapper"
	clienttesting "k8s.io/client-go/testing"
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
		hint string
	}{
		{[]string{"scale", "deploy/web", "--replicas", "5"}, deployGVR, "web", "deployment/web", "deployments.v1.apps/web"},
		{[]string{"scale", "statefulset", "db", "--replicas", "4"}, stsGVR, "db", "statefulset/db", "statefulsets.v1.apps/db"},
		{[]string{"scale", "rs/rs", "--replicas", "2"}, rsGVR, "rs", "replicaset/rs", "replicasets.v1.apps/rs"},
		{[]string{"scale", "gadgets/g", "--replicas", "7"}, gadgetGVR, "g", "gadget/g", "gadgets.v1.example.com/g"},
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
			next := docs[1]["next"].([]any)
			if len(next) != 2 || next[0] != "k8s-axi inspect "+tc.hint+" -n default" || next[1] != "k8s-axi events "+tc.hint+" -n default" {
				t.Errorf("next = %v", next)
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
		"empty dry-run":       {"scale", "deploy/web", "--replicas", "2", "--dry-run=", "-n", "default"},
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
			if code != 2 {
				t.Fatalf("exit %d, stdout %q, stderr %s", code, out, errOut)
			}
			requireStdoutError(t, out, errOut, "hint")
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
			code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default")
			if code != 1 {
				t.Errorf("exit %d, want hint %q: %s", code, tc.want, errOut)
			} else {
				requireStdoutError(t, out, errOut, tc.want)
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

func TestScaleResolveDiscoveryRespectsCommandTimeout(t *testing.T) {
	e := scaleEnv(t)
	cached := memory.NewMemCacheClient(e.c.Discovery)
	e.c.Mapper = restmapper.NewShortcutExpander(restmapper.NewDeferredDiscoveryRESTMapper(cached), cached, func(string) {})
	blockDiscovery(e.c, 500*time.Millisecond)

	start := time.Now()
	code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default", "--timeout", "20ms")
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, out, errOut)
	}
	requireStdoutError(t, out, errOut, "raise --timeout")
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("timeout took %v, want less than stalled discovery", elapsed)
	}
}

func TestScaleSubresourceDiscoveryRespectsCommandTimeout(t *testing.T) {
	e := scaleEnv(t)
	blockDiscovery(e.c, 500*time.Millisecond)

	start := time.Now()
	code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default", "--timeout", "20ms")
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, out, errOut)
	}
	requireStdoutError(t, out, errOut, "raise --timeout")
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("timeout took %v, want less than stalled discovery", elapsed)
	}
}

func TestScaleResolveContextDeadlineIsRuntimeTimeout(t *testing.T) {
	cases := []struct {
		name string
		m    deadlineRESTMapper
	}{
		{name: "kind", m: deadlineRESTMapper{failKind: true}},
		{name: "mapping", m: deadlineRESTMapper{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := scaleEnv(t)
			e.c.Mapper = tc.m
			code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default")
			if code != 1 || strings.Contains(out, "ambiguous") {
				t.Fatalf("exit %d, stdout %q, stderr %s", code, out, errOut)
			}
			requireStdoutError(t, out, errOut, "raise --timeout")
		})
	}
}

func blockDiscovery(c *Clients, delay time.Duration) {
	c.Discovery.(*fakediscovery.FakeDiscovery).PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
		time.Sleep(delay)
		return false, nil, nil
	})
}

type deadlineRESTMapper struct {
	failKind bool
}

func (m deadlineRESTMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return m.KindForWithContext(context.Background(), schema.GroupVersionResource{})
}

func (m deadlineRESTMapper) KindForWithContext(context.Context, schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	if m.failKind {
		return schema.GroupVersionKind{}, context.DeadlineExceeded
	}
	return schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, nil
}

func (deadlineRESTMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineRESTMapper) RESTMappingWithContext(context.Context, schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, context.DeadlineExceeded
}

func (m deadlineRESTMapper) KindsFor(resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	gvk, err := m.KindFor(resource)
	if err != nil {
		return nil, err
	}
	return []schema.GroupVersionKind{gvk}, nil
}

func (m deadlineRESTMapper) KindsForWithContext(ctx context.Context, resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	gvk, err := m.KindForWithContext(ctx, resource)
	if err != nil {
		return nil, err
	}
	return []schema.GroupVersionKind{gvk}, nil
}

func (deadlineRESTMapper) ResourceFor(schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, context.DeadlineExceeded
}

func (deadlineRESTMapper) ResourceForWithContext(context.Context, schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, context.DeadlineExceeded
}

func (deadlineRESTMapper) ResourcesFor(schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineRESTMapper) ResourcesForWithContext(context.Context, schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineRESTMapper) RESTMappings(schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineRESTMapper) RESTMappingsWithContext(context.Context, schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineRESTMapper) ResourceSingularizer(string) (string, error) {
	return "", context.DeadlineExceeded
}

func (deadlineRESTMapper) ResourceSingularizerWithContext(context.Context, string) (string, error) {
	return "", context.DeadlineExceeded
}

// A controller writes the workload's status all the time, which changes its
// resourceVersion without touching the replica count. That must not fail a scale.
func TestScaleRetriesWhenOnlyStatusChanged(t *testing.T) {
	e := scaleEnv(t)
	e.beforeUpdate = func() { e.touch(deployGVR, "default", "web", nil) }
	code, _, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := e.replicas(deployGVR, "default", "web"); got != 5 {
		t.Errorf("replicas = %d, want 5", got)
	}
}

// Someone else changing the replica count after the plan was made means the
// plan no longer describes the change, so the command stops instead of overwriting it.
func TestScaleStopsWhenReplicasChangedMeanwhile(t *testing.T) {
	e := scaleEnv(t)
	other := int64(8)
	e.beforeUpdate = func() { e.touch(deployGVR, "default", "web", &other) }
	code, out, errOut := run(t, e.c, "scale", "deploy/web", "--replicas", "5", "-n", "default", "--json")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	requireStdoutError(t, out, errOut, "re-run")
	if got := e.replicas(deployGVR, "default", "web"); got != 8 {
		t.Errorf("replicas = %d, want the other writer's 8", got)
	}
}
