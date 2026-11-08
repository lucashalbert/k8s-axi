package kube

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// mutEnv is a fake cluster for the mutating commands. The fake dynamic client
// has no scale subresource and ignores dry-run, so the reactors below model
// both the way the API server does.
type mutEnv struct {
	t       *testing.T
	c       *Clients
	dyn     *dynamicfake.FakeDynamicClient
	dryRuns []string         // "verb resource/name" of every dry-run request
	patches []string         // bodies of real (non dry-run) patches
	failOn  map[string]error // "verb resource" -> error to return
	noDry   map[string]bool  // resources whose dry-run the server rejects
	// beforeUpdate runs once, just before the next scale update is checked,
	// to model another writer touching the object since it was read.
	beforeUpdate func()
}

func newMutEnv(t *testing.T, objs ...runtime.Object) *mutEnv {
	t.Helper()
	c, _ := readFixture(t, nil, objs...)
	e := &mutEnv{t: t, c: c, dyn: c.Dynamic.(*dynamicfake.FakeDynamicClient), failOn: map[string]error{}, noDry: map[string]bool{}}

	scale := metav1.APIResource{Kind: "Scale", Group: "autoscaling", Version: "v1", Namespaced: true, Verbs: metav1.Verbs{"get", "update", "patch"}}
	fd := c.Discovery.(*fakediscovery.FakeDiscovery)
	for _, l := range fd.Resources {
		var names []string
		switch l.GroupVersion {
		case "apps/v1":
			names = []string{"deployments", "statefulsets", "replicasets"}
		case "example.com/v1":
			names = []string{"gadgets"}
		}
		for _, n := range names {
			s := scale
			s.Name = n + "/scale"
			l.APIResources = append(l.APIResources, s)
		}
		// Pods have no scale subresource and ClusterThing is cluster scoped.
	}
	e.dyn.PrependReactor("*", "*", e.react)
	return e
}

func (e *mutEnv) react(a clienttesting.Action) (bool, runtime.Object, error) {
	res := a.GetResource().Resource
	if err, ok := e.failOn[a.GetVerb()+" "+res]; ok {
		return true, nil, err
	}
	dry := false
	switch act := a.(type) {
	case clienttesting.UpdateActionImpl:
		dry = len(act.UpdateOptions.DryRun) > 0
	case clienttesting.PatchActionImpl:
		dry = len(act.PatchOptions.DryRun) > 0
	}
	if dry && e.noDry[res] {
		return true, nil, apierrors.NewBadRequest("the server does not support dry run for " + res)
	}
	switch act := a.(type) {
	case clienttesting.GetActionImpl:
		if act.GetSubresource() == "scale" {
			return e.getScale(a)
		}
	case clienttesting.UpdateActionImpl:
		if act.GetSubresource() == "scale" {
			return e.updateScale(act, dry)
		}
	case clienttesting.PatchActionImpl:
		if dry {
			e.dryRuns = append(e.dryRuns, "patch "+res+"/"+act.GetName())
			obj, err := e.dyn.Tracker().Get(a.GetResource(), a.GetNamespace(), act.GetName())
			return true, obj, err
		}
		e.patches = append(e.patches, string(act.GetPatch()))
	}
	return false, nil, nil
}

func (e *mutEnv) getScale(a clienttesting.Action) (bool, runtime.Object, error) {
	name := a.(clienttesting.GetAction).GetName()
	obj, err := e.dyn.Tracker().Get(a.GetResource(), a.GetNamespace(), name)
	if err != nil {
		return true, nil, err
	}
	u := obj.(*unstructured.Unstructured)
	n, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	obs, found, _ := unstructured.NestedInt64(u.Object, "status", "replicas")
	if !found {
		obs = n
	}
	return true, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling/v1", "kind": "Scale",
		"metadata": map[string]any{"name": name, "namespace": a.GetNamespace(), "resourceVersion": u.GetResourceVersion()},
		"spec":     map[string]any{"replicas": n},
		"status":   map[string]any{"replicas": obs},
	}}, nil
}

func (e *mutEnv) updateScale(act clienttesting.UpdateActionImpl, dry bool) (bool, runtime.Object, error) {
	in := act.GetObject().(*unstructured.Unstructured)
	name := in.GetName()
	if f := e.beforeUpdate; f != nil {
		e.beforeUpdate = nil
		f()
	}
	obj, err := e.dyn.Tracker().Get(act.GetResource(), act.GetNamespace(), name)
	if err != nil {
		return true, nil, err
	}
	cur := obj.(*unstructured.Unstructured)
	if in.GetResourceVersion() != cur.GetResourceVersion() {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: act.GetResource().Group, Resource: act.GetResource().Resource}, name, nil)
	}
	if dry {
		e.dryRuns = append(e.dryRuns, "update "+act.GetResource().Resource+"/"+name+"/scale")
		return true, in, nil
	}
	n, _, _ := unstructured.NestedInt64(in.Object, "spec", "replicas")
	cp := cur.DeepCopy()
	_ = unstructured.SetNestedField(cp.Object, n, "spec", "replicas")
	cp.SetResourceVersion(cp.GetResourceVersion() + "1")
	if err := e.dyn.Tracker().Update(act.GetResource(), cp, act.GetNamespace()); err != nil {
		return true, nil, err
	}
	return true, in, nil
}

// object reads the stored object, bypassing the reactors.
func (e *mutEnv) object(gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	e.t.Helper()
	o, err := e.dyn.Tracker().Get(gvr, ns, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return o.(*unstructured.Unstructured)
}

func (e *mutEnv) replicas(gvr schema.GroupVersionResource, ns, name string) int64 {
	n, _, _ := unstructured.NestedInt64(e.object(gvr, ns, name).Object, "spec", "replicas")
	return n
}

// workload builds an unstructured workload with spec.replicas set.
func workload(apiVersion, kind, ns, name string, replicas int64, spec map[string]any) *unstructured.Unstructured {
	s := map[string]any{"replicas": replicas}
	for k, v := range spec {
		s[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "resourceVersion": "1", "uid": "uid-" + name},
		"spec":     s,
	}}
}

var (
	deployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	stsGVR    = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	dsGVR     = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	rsGVR     = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	gadgetGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "gadgets"}
)

// jsonDocs decodes a stream of JSON objects, which is what the mutating
// commands print: the plan first, then the result.
func jsonDocs(t *testing.T, s string) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var docs []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			return docs
		} else if err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
		docs = append(docs, m)
	}
}

// touch bumps the stored object's resourceVersion, optionally setting replicas
// first, as a controller's status write or another user's scale would.
func (e *mutEnv) touch(gvr schema.GroupVersionResource, ns, name string, replicas *int64) {
	e.t.Helper()
	o := e.object(gvr, ns, name).DeepCopy()
	if replicas != nil {
		_ = unstructured.SetNestedField(o.Object, *replicas, "spec", "replicas")
	}
	o.SetResourceVersion(o.GetResourceVersion() + "9")
	if err := e.dyn.Tracker().Update(gvr, o, ns); err != nil {
		e.t.Fatal(err)
	}
}
