package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

func TestResolve(t *testing.T) {
	c, _ := readFixture(t, nil)
	tests := []struct {
		arg        string
		wantRes    string // GroupVersionResource as group/version/resource
		wantKind   string
		namespaced bool
	}{
		{"pods", "/v1/pods", "Pod", true},
		{"pod", "/v1/pods", "Pod", true},
		{"po", "/v1/pods", "Pod", true},
		{"Pod", "/v1/pods", "Pod", true},
		{"nodes", "/v1/nodes", "Node", false},
		{"deploy", "apps/v1/deployments", "Deployment", true},
		{"deployments.apps", "apps/v1/deployments", "Deployment", true},
		{"deployments.v1.apps", "apps/v1/deployments", "Deployment", true},
		{"apps/v1/deployments", "apps/v1/deployments", "Deployment", true},
		{"gadgets", "example.com/v1/gadgets", "Gadget", true},
		{"clusterthings.example.com", "example.com/v1/clusterthings", "ClusterThing", false},
		// Served only in the non-preferred version of its group.
		{"widgets", "example.com/v1beta1/widgets", "Widget", true},
		{"wg", "example.com/v1beta1/widgets", "Widget", true},
		{"widgets.example.com", "example.com/v1beta1/widgets", "Widget", true},
		{"widgets.v1beta1.example.com", "example.com/v1beta1/widgets", "Widget", true},
		{"example.com/v1beta1/widgets", "example.com/v1beta1/widgets", "Widget", true},
		// Core wins the bare name, the other group is reachable by qualifying it.
		{"events", "/v1/events", "Event", true},
		{"events.events.k8s.io", "events.k8s.io/v1/events", "Event", true},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			r, err := c.Resolve(tt.arg)
			if err != nil {
				t.Fatal(err)
			}
			got := r.GVR.Group + "/" + r.GVR.Version + "/" + r.GVR.Resource
			if got != tt.wantRes || r.Kind != tt.wantKind || r.Namespaced != tt.namespaced {
				t.Errorf("Resolve(%q) = %s %s namespaced=%v, want %s %s namespaced=%v", tt.arg, got, r.Kind, r.Namespaced, tt.wantRes, tt.wantKind, tt.namespaced)
			}
		})
	}
}

func TestResourceString(t *testing.T) {
	c, _ := readFixture(t, nil)
	for arg, want := range map[string]string{"po": "pods", "deploy": "deployments.v1.apps", "widgets": "widgets.v1beta1.example.com"} {
		r, err := c.Resolve(arg)
		if err != nil {
			t.Fatal(err)
		}
		if r.String() != want {
			t.Errorf("%s: String() = %q, want %q", arg, r.String(), want)
		}
	}
}

func TestResolveUnknownKindIsUsageError(t *testing.T) {
	c, _ := readFixture(t, nil)
	_, err := c.Resolve("frobnicators")
	var ae *axierr.Error
	if !errors.As(err, &ae) || ae.Code != axierr.CodeUsage {
		t.Fatalf("want usage error, got %v", err)
	}
	if !strings.Contains(ae.Hint, "k8s-axi discover --kind frobnicators") {
		t.Errorf("hint = %q", ae.Hint)
	}
}

func TestSplitTarget(t *testing.T) {
	tests := []struct {
		args       []string
		kind, name string
		wantErr    bool
	}{
		{[]string{"pod", "web"}, "pod", "web", false},
		{[]string{"pod/web"}, "pod", "web", false},
		{[]string{"deployments.apps/web"}, "deployments.apps", "web", false},
		{[]string{"example.com/v1beta1/widgets"}, "example.com/v1beta1/widgets", "", false}, // a kind, no name
		{[]string{"pods"}, "pods", "", false},
		{[]string{"example.com/v1beta1/widgets", "w1"}, "example.com/v1beta1/widgets", "w1", false},
		{[]string{"example.com/v1beta1/widgets/w1"}, "example.com/v1beta1/widgets", "w1", false},
		{[]string{"pod/web", "x"}, "", "", true},
		{[]string{"a", "b", "c"}, "", "", true},
		{[]string{"pod/"}, "", "", true},
	}
	for _, tt := range tests {
		kind, name, err := splitTarget(tt.args)
		if (err != nil) != tt.wantErr || kind != tt.kind || name != tt.name {
			t.Errorf("splitTarget(%v) = %q %q %v, want %q %q err=%v", tt.args, kind, name, err, tt.kind, tt.name, tt.wantErr)
		}
	}
}

// failingMapper is a REST mapper whose discovery cannot reach the server.
type failingMapper struct {
	meta.RESTMapper
	err error
}

func (m failingMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, m.err
}

func TestResolveDiscoveryFailureIsARuntimeErrorWithAHint(t *testing.T) {
	for name, cause := range map[string]error{
		"connection refused": errors.New(`Get "https://10.0.0.1:6443/api": dial tcp 10.0.0.1:6443: connect: connection refused`),
		"timeout":            fmt.Errorf("Get \"https://10.0.0.1:6443/api?timeout=1s\": %w", context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := readFixture(t, nil)
			c.Mapper = failingMapper{err: cause}
			_, err := c.Resolve("pods")
			var ae *axierr.Error
			if !errors.As(err, &ae) || ae.Code != axierr.CodeRuntime || ae.Hint == "" || ae.Op == "" {
				t.Fatalf("want a runtime error with an op and a hint, got %#v", err)
			}
		})
	}
}
