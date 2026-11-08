package kube

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/restmapper"
	clienttesting "k8s.io/client-go/testing"
)

// applyCall is one server-side apply request the fake API server received.
type applyCall struct {
	resource, ns, name, manager string
	force, dry                  bool
	body                        *unstructured.Unstructured
}

// applyEnv models server-side apply on top of the mutation fake cluster: it
// stores what is applied, merges it over the live object, enforces field
// ownership conflicts and namespaces, and learns new kinds from applied CRDs.
type applyEnv struct {
	*mutEnv
	calls      []applyCall
	owners     map[string]map[string]string // "resource/name" -> field -> manager that owns it
	invalid    map[string]string            // "resource/name" -> message of a 422
	namespaces map[string]bool              // existing namespaces; nil means every namespace exists
}

var (
	nsGVR     = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	configGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

// resettableMapper rebuilds itself from discovery on Reset, like the real
// deferred discovery mapper.
type resettableMapper struct {
	meta.RESTMapper
	c *Clients
}

func (m *resettableMapper) Reset() {
	groups, err := restmapper.GetAPIGroupResources(m.c.Discovery)
	if err != nil {
		panic(err)
	}
	m.RESTMapper = restmapper.NewShortcutExpander(restmapper.NewDiscoveryRESTMapper(groups), m.c.Discovery, func(string) {})
}

func newApplyEnv(t *testing.T, objs ...runtime.Object) *applyEnv {
	t.Helper()
	e := &applyEnv{mutEnv: newMutEnv(t, objs...), owners: map[string]map[string]string{},
		invalid: map[string]string{}}
	fd := e.c.Discovery.(*fakediscovery.FakeDiscovery)
	fd.Resources = append(fd.Resources, &metav1.APIResourceList{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{
		{Name: "customresourcedefinitions", SingularName: "customresourcedefinition", Kind: "CustomResourceDefinition", ShortNames: []string{"crd"}, Verbs: metav1.Verbs{"get", "list"}}}})
	m := &resettableMapper{c: e.c}
	m.Reset()
	e.c.Mapper = m
	e.dyn.PrependReactor("patch", "*", e.apply)
	return e
}

func (e *applyEnv) apply(a clienttesting.Action) (bool, runtime.Object, error) {
	act, ok := a.(clienttesting.PatchActionImpl)
	if !ok || act.GetPatchType() != types.ApplyPatchType {
		return false, nil, nil
	}
	body := &unstructured.Unstructured{}
	if err := utiljson.Unmarshal(act.GetPatch(), &body.Object); err != nil {
		return true, nil, err
	}
	gvr, ns, name := act.GetResource(), act.GetNamespace(), act.GetName()
	o := act.PatchOptions
	call := applyCall{resource: gvr.Resource, ns: ns, name: name, manager: o.FieldManager,
		force: o.Force != nil && *o.Force, dry: len(o.DryRun) > 0, body: body}
	e.calls = append(e.calls, call)
	key := gvr.Resource + "/" + name
	gk := schema.GroupKind{Group: gvr.Group, Kind: body.GetKind()}

	if err, ok := e.failOn["patch "+gvr.Resource]; ok {
		return true, nil, err
	}
	if call.dry && e.noDry[gvr.Resource] {
		return true, nil, apierrors.NewBadRequest("the server does not support dry run for " + gvr.Resource)
	}
	if e.namespaces != nil && ns != "" && !e.namespaces[ns] {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
	}
	if msg, ok := e.invalid[key]; ok {
		return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 422, Reason: metav1.StatusReasonInvalid,
			Message: msg, Details: &metav1.StatusDetails{Name: name, Group: gk.Group, Kind: gk.Kind}}}
	}
	if owners := e.owners[key]; owners != nil && !call.force {
		return true, nil, fieldConflict(gvr, name, owners)
	}

	live, err := e.dyn.Tracker().Get(gvr, ns, name)
	exists := err == nil
	merged := body.DeepCopy()
	if exists {
		merged.Object = mergeMaps(live.(*unstructured.Unstructured).Object, body.Object)
	} else {
		merged.SetResourceVersion("1")
	}
	if !call.dry {
		if exists {
			merged.SetResourceVersion(live.(*unstructured.Unstructured).GetResourceVersion() + "1")
			err = e.dyn.Tracker().Update(gvr, merged, ns)
		} else {
			err = e.dyn.Tracker().Create(gvr, merged, ns)
		}
		if err != nil {
			return true, nil, err
		}
		e.learn(gvr, merged)
	}
	return true, merged, nil
}

// learn makes the fake cluster serve what an applied object introduces:
// a namespace, or the kind of a CRD.
func (e *applyEnv) learn(gvr schema.GroupVersionResource, o *unstructured.Unstructured) {
	switch gvr {
	case nsGVR:
		if e.namespaces != nil {
			e.namespaces[o.GetName()] = true
		}
	case crdGVR:
		group, _, _ := unstructured.NestedString(o.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(o.Object, "spec", "names", "kind")
		plural, _, _ := unstructured.NestedString(o.Object, "spec", "names", "plural")
		scope, _, _ := unstructured.NestedString(o.Object, "spec", "scope")
		vers, _, _ := unstructured.NestedSlice(o.Object, "spec", "versions")
		v := vers[0].(map[string]any)["name"].(string)
		fd := e.c.Discovery.(*fakediscovery.FakeDiscovery)
		fd.Resources = append(fd.Resources, &metav1.APIResourceList{GroupVersion: group + "/" + v, APIResources: []metav1.APIResource{
			{Name: plural, SingularName: strings.ToLower(kind), Kind: kind, Namespaced: scope == "Namespaced", Verbs: metav1.Verbs{"get", "list"}}}})
	}
}

func (e *applyEnv) real() []applyCall {
	var out []applyCall
	for _, c := range e.calls {
		if !c.dry {
			out = append(out, c)
		}
	}
	return out
}

func (e *applyEnv) has(gvr schema.GroupVersionResource, ns, name string) bool {
	_, err := e.dyn.Tracker().Get(gvr, ns, name)
	return err == nil
}

func fieldConflict(gvr schema.GroupVersionResource, name string, owners map[string]string) error {
	st := metav1.Status{Status: metav1.StatusFailure, Code: 409, Reason: metav1.StatusReasonConflict,
		Message: "Apply failed with conflicts", Details: &metav1.StatusDetails{Name: name, Group: gvr.Group, Kind: gvr.Resource}}
	for field, mgr := range owners {
		st.Details.Causes = append(st.Details.Causes, metav1.StatusCause{Type: metav1.CauseTypeFieldManagerConflict, Field: field,
			Message: `conflict with "` + mgr + `" using ` + gvr.GroupVersion().String()})
	}
	return &apierrors.StatusError{ErrStatus: st}
}

// mergeMaps overlays b on a: maps merge, everything else is replaced.
func mergeMaps(a, b map[string]any) map[string]any {
	out := runtime.DeepCopyJSON(a)
	for k, v := range b {
		if bm, ok := v.(map[string]any); ok {
			if am, ok := out[k].(map[string]any); ok {
				out[k] = mergeMaps(am, bm)
				continue
			}
		}
		out[k] = runtime.DeepCopyJSONValue(v)
	}
	return out
}

func manifest(t *testing.T, body string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "m.yaml", body)
}

// applyRun runs "apply -f FILE" plus args and returns exit code and streams.
func applyRun(t *testing.T, e *applyEnv, file string, args ...string) (int, string, string) {
	t.Helper()
	return run(t, e.c, append([]string{"apply", "-f", file}, args...)...)
}
