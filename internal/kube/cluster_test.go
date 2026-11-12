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

func requireStdoutError(t testing.TB, stdout, stderr string, wants ...string) {
	t.Helper()
	if stderr != "" {
		t.Errorf("stderr should be empty: %s", stderr)
	}
	if stdout == "" {
		t.Error("stdout should contain error output")
	}
	for _, want := range wants {
		if !strings.Contains(stdout, want) {
			t.Errorf("error output missing %q:\n%s", want, stdout)
		}
	}
}

// testRoot builds the real command tree around fake clients.
func testRoot(t testing.TB, c *Clients, loadErr error) (root *cobra.Command, stdout, stderr *bytes.Buffer) {
	t.Helper()
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	m := Module{load: func(o Options) (*Clients, error) {
		if c != nil && o.Namespace != "" { // like Load, -n overrides the context's namespace
			cc := *c
			cc.Namespace = o.Namespace
			return &cc, loadErr
		}
		return c, loadErr
	}}
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
		return true, nil, errors.New(`Get "https://user:pw@127.0.0.1:6443/version": dial tcp 127.0.0.1:6443: connect: connection refused`)
	})
	root, out, errb := testRoot(t, c, nil)
	root.SetArgs([]string{"cluster", "info"})
	if code := cli.Execute(root); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	stdout := out.String()
	requireStdoutError(t, stdout, errb.String(), "kind-test", "connection refused", "hint")
	if strings.Contains(stdout, "goroutine") {
		t.Error("stack trace in error output")
	}
	if strings.Contains(stdout, "user") || strings.Contains(stdout, "pw") {
		t.Errorf("credentials leaked in error output:\n%s", stdout)
	}
}

func TestLoadUsageErrorPropagates(t *testing.T) {
	root, out, errb := testRoot(t, nil, usageForTest())
	root.SetArgs([]string{"cluster", "info"})
	if code := cli.Execute(root); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	requireStdoutError(t, out.String(), errb.String(), "alpha")
	if !strings.Contains(out.String(), "beta") {
		t.Errorf("contexts not listed: %s", out)
	}
}
