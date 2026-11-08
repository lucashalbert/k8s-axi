package kube

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/restmapper"
)

// servedResources is what the fake API server advertises: the built-in kinds
// the read commands care about, a kind that exists in two groups (events), a
// CRD group whose preferred version is v1 and a kind served only in the
// non-preferred v1beta1.
func servedResources() []*metav1.APIResourceList {
	v := metav1.Verbs{"get", "list"}
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, ShortNames: []string{"po"}, Verbs: v},
			{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: v},
			{Name: "nodes", SingularName: "node", Kind: "Node", ShortNames: []string{"no"}, Verbs: v},
			{Name: "secrets", SingularName: "secret", Kind: "Secret", Namespaced: true, Verbs: v},
			{Name: "configmaps", SingularName: "configmap", Kind: "ConfigMap", Namespaced: true, ShortNames: []string{"cm"}, Verbs: v},
			{Name: "services", SingularName: "service", Kind: "Service", Namespaced: true, ShortNames: []string{"svc"}, Verbs: v},
			{Name: "events", SingularName: "event", Kind: "Event", Namespaced: true, ShortNames: []string{"ev"}, Verbs: v},
			{Name: "namespaces", SingularName: "namespace", Kind: "Namespace", ShortNames: []string{"ns"}, Verbs: v},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true, ShortNames: []string{"deploy"}, Verbs: v},
			{Name: "replicasets", SingularName: "replicaset", Kind: "ReplicaSet", Namespaced: true, ShortNames: []string{"rs"}, Verbs: v},
			{Name: "statefulsets", SingularName: "statefulset", Kind: "StatefulSet", Namespaced: true, ShortNames: []string{"sts"}, Verbs: v},
			{Name: "daemonsets", SingularName: "daemonset", Kind: "DaemonSet", Namespaced: true, ShortNames: []string{"ds"}, Verbs: v},
		}},
		{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
			{Name: "jobs", SingularName: "job", Kind: "Job", Namespaced: true, Verbs: v},
		}},
		{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "events", SingularName: "event", Kind: "Event", Namespaced: true, Verbs: v},
		}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "gadgets", SingularName: "gadget", Kind: "Gadget", Namespaced: true, Verbs: v},
			{Name: "clusterthings", SingularName: "clusterthing", Kind: "ClusterThing", Verbs: v},
		}},
		{GroupVersion: "example.com/v1beta1", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Kind: "Widget", Namespaced: true, ShortNames: []string{"wg"}, Verbs: v},
		}},
	}
}

// listKinds lets the fake dynamic client list kinds it has no scheme for.
func listKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "example.com", Version: "v1beta1", Resource: "widgets"}:                       "WidgetList",
		{Group: "example.com", Version: "v1", Resource: "gadgets"}:                            "GadgetList",
		{Group: "example.com", Version: "v1", Resource: "clusterthings"}:                      "ClusterThingList",
		{Group: "apps", Version: "v1", Resource: "replicasets"}:                               "ReplicaSetList",
		{Group: "apps", Version: "v1", Resource: "deployments"}:                               "DeploymentList",
		{Group: "batch", Version: "v1", Resource: "jobs"}:                                     "JobList",
		{Group: "events.k8s.io", Version: "v1", Resource: "events"}:                           "EventList",
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
	}
}

// readFixture builds fake clients that serve servedResources: typed objects go
// to the fake clientset and unstructured ones to the fake dynamic client.
func readFixture(t *testing.T, typed []runtime.Object, dyn ...runtime.Object) (*Clients, *kubefake.Clientset) {
	t.Helper()
	c, fd := fakeClients()
	cs := c.Typed.(*kubefake.Clientset)
	for _, o := range typed {
		if err := cs.Tracker().Add(o); err != nil {
			t.Fatal(err)
		}
	}
	fd.Resources = servedResources()
	c.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds(), dyn...)
	groups, err := restmapper.GetAPIGroupResources(c.Discovery)
	if err != nil {
		t.Fatal(err)
	}
	c.Mapper = restmapper.NewShortcutExpander(restmapper.NewDiscoveryRESTMapper(groups), c.Discovery, func(string) {})
	return c, cs
}
