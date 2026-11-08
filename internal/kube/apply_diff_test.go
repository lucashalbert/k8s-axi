package kube

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestChangedPaths(t *testing.T) {
	live := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "resourceVersion": "1", "generation": int64(1), "uid": "u", "creationTimestamp": "t",
			"managedFields": []any{map[string]any{"manager": "x"}}, "labels": map[string]any{"a": "1"}},
		"spec": map[string]any{"replicas": int64(3), "template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "nginx:1"}, map[string]any{"name": "d", "image": "x:1"}}}}},
		"status": map[string]any{"readyReplicas": int64(3)},
	}
	proposed := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "resourceVersion": "2", "generation": int64(2), "uid": "u", "creationTimestamp": "t",
			"managedFields": []any{}, "labels": map[string]any{"a": "1", "b": "2"}},
		"spec": map[string]any{"replicas": int64(5), "template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "nginx:2"}, map[string]any{"name": "d", "image": "x:1"}}}}},
		"status": map[string]any{"readyReplicas": int64(0)},
	}
	want := []string{"metadata.labels.b", "spec.replicas", "spec.template.spec.containers[0].image"}
	if got := changedPaths(live, proposed); !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
	if got := changedPaths(live, live); len(got) != 0 {
		t.Errorf("identical objects differ: %v", got)
	}
	// A list that changes length is one path; a removed key is a path too.
	a := map[string]any{"spec": map[string]any{"ports": []any{int64(1)}, "gone": "x"}}
	b := map[string]any{"spec": map[string]any{"ports": []any{int64(1), int64(2)}}}
	if got := changedPaths(a, b); !reflect.DeepEqual(got, []string{"spec.gone", "spec.ports"}) {
		t.Errorf("paths = %v", got)
	}
}

func TestPathList(t *testing.T) {
	if got := pathList([]string{"a", "b"}, 5); got != "a, b" {
		t.Errorf("got %q", got)
	}
	if got := pathList([]string{"a", "b", "c", "d"}, 2); got != "a, b, +2 more" {
		t.Errorf("got %q", got)
	}
}

func TestConflictsOf(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	err := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 409, Reason: metav1.StatusReasonConflict,
		Message: "Apply failed with 1 conflict: conflict with \"helm\" using apps/v1: .spec.replicas",
		Details: &metav1.StatusDetails{Name: "web", Group: gr.Group, Kind: gr.Resource, Causes: []metav1.StatusCause{
			{Type: metav1.CauseTypeFieldManagerConflict, Field: ".spec.replicas", Message: `conflict with "helm" using apps/v1`},
			{Type: metav1.CauseTypeFieldManagerConflict, Field: ".spec.template.spec.containers[name=\"c\"].image", Message: `conflict with "kubectl-client-side-apply" using apps/v1`},
		}}}}
	got := conflictsOf(err)
	want := []string{`.spec.replicas (owned by "helm")`, `.spec.template.spec.containers[name="c"].image (owned by "kubectl-client-side-apply")`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("conflicts = %v", got)
	}
	if got := conflictsOf(errors.New("nope")); got != nil {
		t.Errorf("non-conflict = %v", got)
	}
	if got := conflictsOf(apierrors.NewConflict(gr, "web", errors.New("stale"))); got != nil {
		t.Errorf("update conflict is not a field conflict: %v", got)
	}
}

func manifestSecret(data, stringData map[string]any) *unstructured.Unstructured {
	o := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s"}}
	if data != nil {
		o["data"] = data
	}
	if stringData != nil {
		o["stringData"] = stringData
	}
	return &unstructured.Unstructured{Object: o}
}

func TestSecretKeys(t *testing.T) {
	o := manifestSecret(map[string]any{"b": base64.StdEncoding.EncodeToString([]byte("12345"))}, map[string]any{"a": "hunter2"})
	if got := secretKeys(o); got != "keys: a 7 B, b 5 B" {
		t.Errorf("got %q", got)
	}
	if got := secretKeys(manifestSecret(nil, nil)); got != "no data keys" {
		t.Errorf("got %q", got)
	}
}

func TestScrubSecretValues(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("s3cr3t-value"))
	o := manifestSecret(map[string]any{"k": enc}, map[string]any{"p": "hunter2", "short": "ab"})
	msg := `Secret "s" is invalid: data[k]: Invalid value: "` + enc + `"; stringData: "hunter2" and s3cr3t-value, ab stays`
	got := scrubSecretValues(msg, o)
	for _, leak := range []string{enc, "hunter2", "s3cr3t-value"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked in %q", leak, got)
		}
	}
	if !strings.Contains(got, "ab stays") || !strings.Contains(got, "<redacted>") {
		t.Errorf("got %q", got)
	}
	cm := &unstructured.Unstructured{Object: map[string]any{"kind": "ConfigMap", "data": map[string]any{"k": "visible-value"}}}
	if got := scrubSecretValues("visible-value", cm); got != "visible-value" {
		t.Errorf("only Secrets are scrubbed, got %q", got)
	}
}
