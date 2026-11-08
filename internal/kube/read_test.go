package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

func pod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
}

func widget(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1beta1", "kind": "Widget",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{"size": int64(3)},
	}}
}

func verbs(actions []clienttesting.Action, verb, resource string) int {
	n := 0
	for _, a := range actions {
		if a.Matches(verb, resource) {
			n++
		}
	}
	return n
}

func mustResolve(t *testing.T, c *Clients, arg string) Resource {
	t.Helper()
	r, err := c.Resolve(arg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestListBuiltinUsesTypedClient(t *testing.T) {
	c, cs := readFixture(t, []runtime.Object{pod("default", "a", nil), pod("default", "b", map[string]string{"app": "x"}), pod("other", "c", nil)})
	pods := mustResolve(t, c, "pods")

	res, err := c.List(context.Background(), pods, "default", metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.More {
		t.Fatalf("items=%d more=%v", len(res.Items), res.More)
	}
	if u := res.Items[0]; u.GetKind() != "Pod" || u.GetAPIVersion() != "v1" || u.GetName() != "a" {
		t.Errorf("typed object not stamped: %v", u.Object)
	}
	if verbs(cs.Actions(), "list", "pods") != 1 || len(c.Dynamic.(*dynamicfake.FakeDynamicClient).Actions()) != 0 {
		t.Errorf("expected only the typed client to be used: typed=%v", cs.Actions())
	}

	all, err := c.List(context.Background(), pods, "", metav1.ListOptions{})
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("all namespaces: %d items, %v", len(all.Items), err)
	}
	sel, err := c.List(context.Background(), pods, "", metav1.ListOptions{LabelSelector: "app=x"})
	if err != nil || len(sel.Items) != 1 || sel.Items[0].GetName() != "b" {
		t.Fatalf("selector: %v %v", sel.Items, err)
	}
}

func TestListCRDUsesDynamicClient(t *testing.T) {
	c, cs := readFixture(t, nil, widget("default", "w1"), widget("default", "w2"))
	w := mustResolve(t, c, "widgets") // served only in example.com/v1beta1
	res, err := c.List(context.Background(), w, "default", metav1.ListOptions{})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("%d items, %v", len(res.Items), err)
	}
	if verbs(cs.Actions(), "list", "widgets") != 0 || len(c.Dynamic.(*dynamicfake.FakeDynamicClient).Actions()) != 1 {
		t.Errorf("expected only the dynamic client to be used")
	}
	got, err := c.Get(context.Background(), w, "default", "w1")
	if err != nil || got.GetName() != "w1" {
		t.Fatalf("get: %v %v", got, err)
	}
}

func TestListBuiltinOutsideTypedTableUsesDynamicClient(t *testing.T) {
	ev := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "events.k8s.io/v1", "kind": "Event", "metadata": map[string]any{"namespace": "default", "name": "e1"}}}
	c, _ := readFixture(t, nil, ev)
	r := mustResolve(t, c, "events.events.k8s.io")
	res, err := c.List(context.Background(), r, "default", metav1.ListOptions{})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("%d items, %v", len(res.Items), err)
	}
}

func TestListReportsTruncation(t *testing.T) {
	c, cs := readFixture(t, []runtime.Object{pod("default", "a", nil)})
	var seen metav1.ListOptions
	cs.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		seen = a.(clienttesting.ListActionImpl).ListOptions
		rem := int64(41)
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "tok", RemainingItemCount: &rem}, Items: []corev1.Pod{*pod("default", "a", nil)}}, nil
	})
	res, err := c.List(context.Background(), mustResolve(t, c, "pods"), "default", metav1.ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.More || res.Remaining == nil || *res.Remaining != 41 || seen.Limit != 1 {
		t.Errorf("more=%v remaining=%v limit sent=%d", res.More, res.Remaining, seen.Limit)
	}
}

func TestGetClusterScopedIgnoresNamespace(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	c, _ := readFixture(t, []runtime.Object{node})
	got, err := c.Get(context.Background(), mustResolve(t, c, "nodes"), "default", "n1")
	if err != nil || got.GetName() != "n1" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	c, _ := readFixture(t, nil)
	_, err := c.Get(context.Background(), mustResolve(t, c, "pods"), "default", "nope")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestAPIErrorHints(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		err  error
		hint string
	}{
		{apierrors.NewNotFound(gr, "x"), "check the name and namespace"},
		{apierrors.NewForbidden(gr, "x", errors.New("no")), "RBAC denied 'list' on pods"},
		{apierrors.NewUnauthorized("bad token"), "rejected the credentials"},
		{context.DeadlineExceeded, "raise --timeout"},
		{errors.New(`Get "https://u:pw@h:1/api": refused`), "reachable"},
	}
	for _, tt := range tests {
		err := apiError("get", "pods", tt.err, "list", "pods")
		var ae *axierr.Error
		if !errors.As(err, &ae) || !strings.Contains(ae.Hint, tt.hint) || ae.Code != axierr.CodeRuntime {
			t.Errorf("%v: got %+v, want hint containing %q", tt.err, ae, tt.hint)
		}
		if strings.Contains(err.Error(), "pw@") {
			t.Errorf("credentials leaked: %v", err)
		}
	}
}
