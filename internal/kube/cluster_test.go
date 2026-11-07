package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/update"
)

// testRoot builds the real command tree around fake clients.
func testRoot(t *testing.T, c *Clients, loadErr error) (root *cobra.Command, stdout, stderr *bytes.Buffer) {
	t.Helper()
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	m := Module{load: func(Options) (*Clients, error) { return c, loadErr }}
	root = cli.New("k8s-axi", update.Info{Version: "0.0.1"}, cli.EnvPrefix("K8S_AXI_"), m)
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root, stdout, stderr
}

func fakeClients() (*Clients, *fakediscovery.FakeDiscovery) {
	cs := kubefake.NewSimpleClientset()
	fd := cs.Discovery().(*fakediscovery.FakeDiscovery)
	fd.FakedServerVersion = &version.Info{Major: "1", Minor: "37", GitVersion: "v1.37.0"}
	return &Clients{Typed: cs, Discovery: fd, Context: "kind-test", Namespace: "default", Server: "127.0.0.1:6443"}, fd
}

func TestClusterInfo(t *testing.T) {
	c, _ := fakeClients()
	root, out, _ := testRoot(t, c, nil)
	root.SetArgs([]string{"cluster", "info", "--json"})
	if code := cli.Execute(root); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := map[string]any{"context": "kind-test", "server": "127.0.0.1:6443", "version": "v1.37.0", "namespace": "default", "reachable": true}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestClusterInfoTOONDefault(t *testing.T) {
	c, _ := fakeClients()
	root, out, _ := testRoot(t, c, nil)
	root.SetArgs([]string{"cluster", "info"})
	if code := cli.Execute(root); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "context: kind-test") || strings.HasPrefix(out.String(), "{") {
		t.Errorf("expected TOON, got:\n%s", out)
	}
}

func TestClusterInfoUnreachableIsRuntimeError(t *testing.T) {
	c, fd := fakeClients()
	fd.PrependReactor("get", "version", func(clienttesting.Action) (bool, runtimeObject, error) {
		return true, nil, errors.New(`Get "https://127.0.0.1:6443/version": dial tcp 127.0.0.1:6443: connect: connection refused`)
	})
	root, out, errb := testRoot(t, c, nil)
	root.SetArgs([]string{"cluster", "info"})
	if code := cli.Execute(root); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty: %s", out)
	}
	for _, want := range []string{"kind-test", "connection refused", "hint"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errb)
		}
	}
	if strings.Contains(errb.String(), "goroutine") {
		t.Error("stack trace in error output")
	}
}

func TestLoadUsageErrorPropagates(t *testing.T) {
	root, _, errb := testRoot(t, nil, usageForTest())
	root.SetArgs([]string{"cluster", "info"})
	if code := cli.Execute(root); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "alpha") {
		t.Errorf("contexts not listed: %s", errb)
	}
}
