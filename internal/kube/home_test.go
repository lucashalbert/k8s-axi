package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clienttesting "k8s.io/client-go/testing"

	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/update"
)

func homeJSON(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return got
}

func TestHomeReady(t *testing.T) {
	c, _ := fakeClients()
	root, out, errOut := testRoot(t, c, nil)
	root.SetArgs([]string{"--json"})
	if code := cli.Execute(root); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := homeJSON(t, out)
	want := map[string]any{"state": "ready", "context": "kind-test", "server": "127.0.0.1:6443", "version": "v1.37.0", "namespace": "default", "reachable": true}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	next, _ := got["next"].([]any)
	if len(next) == 0 || !strings.HasPrefix(next[0].(string), "k8s-axi ") {
		t.Errorf("next = %v", got["next"])
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestHomeTOONAndYAML(t *testing.T) {
	c, _ := fakeClients()
	root, out, _ := testRoot(t, c, nil)
	root.SetArgs([]string{})
	if code := cli.Execute(root); code != 0 || !strings.Contains(out.String(), "state: ready") || !strings.Contains(out.String(), "next[") {
		t.Errorf("toon exit %d:\n%s", code, out)
	}
	root, out, _ = testRoot(t, c, nil)
	root.SetArgs([]string{"--yaml"})
	if code := cli.Execute(root); code != 0 || !strings.Contains(out.String(), "state: ready") || !strings.Contains(out.String(), "next:\n") {
		t.Errorf("yaml exit %d:\n%s", code, out)
	}
}

func TestHomeUnreachableIsStateWithExit0(t *testing.T) {
	c, fd := fakeClients()
	fd.PrependReactor("get", "version", func(clienttesting.Action) (bool, runtimeObject, error) {
		return true, nil, errors.New(`Get "https://user:pw@127.0.0.1:6443/version": dial tcp 127.0.0.1:6443: connect: connection refused`)
	})
	root, out, errOut := testRoot(t, c, nil)
	root.SetArgs([]string{"--json"})
	if code := cli.Execute(root); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := homeJSON(t, out)
	if got["state"] != "unreachable" || got["reachable"] != false || got["context"] != "kind-test" {
		t.Errorf("got %v", got)
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "connection refused") || strings.Contains(msg, "pw") {
		t.Errorf("error = %q", msg)
	}
}

func TestHomeHintsCarryContextFlag(t *testing.T) {
	c, _ := fakeClients()
	root, out, _ := testRoot(t, c, nil)
	root.SetArgs([]string{"--json", "--context", "prod east", "--kubeconfig", "/tmp/kube cfg", "-n", "team one"})
	cli.Execute(root)
	for _, h := range homeJSON(t, out)["next"].([]any) {
		if h != "k8s-axi --help" && !strings.HasSuffix(h.(string), " --context 'prod east' --kubeconfig '/tmp/kube cfg' -n 'team one'") {
			t.Errorf("hint %q lacks explicit scope", h)
		}
	}
}

// homeWithKubeconfig runs the real Load against a kubeconfig file.
func homeWithKubeconfig(t *testing.T, yaml string, args ...string) (code int, out, errOut string) {
	t.Helper()
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "kubeconfig")
	t.Setenv("KUBECONFIG", path)
	if yaml != "" {
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := cli.New("k8s-axi", update.Info{Version: "0.0.1"}, cli.EnvPrefix("K8S_AXI_"), Module{})
	var o, e bytes.Buffer
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetArgs(append([]string{"--json"}, args...))
	return cli.Execute(root), o.String(), e.String()
}

func TestHomeNoKubeconfig(t *testing.T) {
	code, out, errOut := homeWithKubeconfig(t, "")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "no-kubeconfig" || got["reachable"] != false || got["contexts"] != nil {
		t.Errorf("got %v", got)
	}
	if next := got["next"].([]any); len(next) < 2 || !strings.Contains(next[0].(string), "--kubeconfig") {
		t.Errorf("next = %v", next)
	}
}

const noCurrentKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: a
  cluster: {server: "https://user:pw@10.0.0.1:6443"}
users:
- name: u
  user: {token: s3cret-token}
contexts:
- name: zeta
  context: {cluster: a, user: u}
- name: alpha
  context: {cluster: a, user: u}
`

func TestHomeNoCurrentContextListsContextsAndNeverSwitches(t *testing.T) {
	code, out, errOut := homeWithKubeconfig(t, noCurrentKubeconfig)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "no-current-context" {
		t.Fatalf("got %v", got)
	}
	ctxs := got["contexts"].([]any)
	if len(ctxs) != 2 || ctxs[0] != "alpha" || ctxs[1] != "zeta" {
		t.Errorf("contexts = %v", ctxs)
	}
	if next := got["next"].([]any); next[0] != "k8s-axi --context alpha" {
		t.Errorf("next = %v", next)
	}
	if strings.Contains(out, "s3cret-token") || strings.Contains(out, "pw@") {
		t.Errorf("secret leaked:\n%s", out)
	}
	if b, _ := os.ReadFile(os.Getenv("KUBECONFIG")); string(b) != noCurrentKubeconfig {
		t.Error("kubeconfig was modified")
	}
}

func TestHomeNoCurrentContextHintCarriesExplicitKubeconfig(t *testing.T) {
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "kube cfg")
	yaml := strings.Replace(noCurrentKubeconfig, "name: alpha", "name: prod east", 1)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	root := cli.New("k8s-axi", update.Info{Version: "0.0.1"}, cli.EnvPrefix("K8S_AXI_"), Module{})
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"--json", "--kubeconfig", path, "--limit", "1"})
	if code := cli.Execute(root); code != 0 || errOut.Len() != 0 {
		t.Fatalf("exit %d: %s", code, &errOut)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	next := got["next"].([]any)
	want := "k8s-axi --context 'prod east' --kubeconfig '" + path + "'"
	if next[0] != want {
		t.Errorf("next = %v, want first hint %q", next, want)
	}
}

func TestHomeContextsRespectLimit(t *testing.T) {
	_, out, _ := homeWithKubeconfig(t, noCurrentKubeconfig, "--limit", "1")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if ctxs := got["contexts"].([]any); len(ctxs) != 1 || got["contexts_omitted"] != float64(1) {
		t.Errorf("got %v", got)
	}
}

func TestHomeExplicitBadInputStaysAnError(t *testing.T) {
	code, out, errOut := homeWithKubeconfig(t, noCurrentKubeconfig, "--context", "nope")
	if code != 2 {
		t.Errorf("unknown --context: exit %d", code)
	}
	requireStdoutError(t, out, errOut, "not found")
	code, out, errOut = homeWithKubeconfig(t, "", "--kubeconfig", "/nonexistent/kubeconfig")
	if code != 2 {
		t.Errorf("missing --kubeconfig: exit %d", code)
	}
	requireStdoutError(t, out, errOut, "kubeconfig file not found")
	if code, _, _ := homeWithKubeconfig(t, "not: [valid"); code != 1 {
		t.Errorf("invalid kubeconfig: exit %d, want 1", code)
	}
}

func TestHomeLeavesHelpAndUnknownCommandsAlone(t *testing.T) {
	c, _ := fakeClients()
	root, out, _ := testRoot(t, c, nil)
	root.SetArgs([]string{"--help"})
	if code := cli.Execute(root); code != 0 || !strings.Contains(out.String(), "Available Commands") {
		t.Errorf("--help: exit %d:\n%s", code, out)
	}
	root, out, _ = testRoot(t, c, nil)
	root.SetArgs([]string{"help"})
	if code := cli.Execute(root); code != 0 || !strings.Contains(out.String(), "Available Commands") {
		t.Errorf("help: exit %d:\n%s", code, out)
	}
	root, out, errOut := testRoot(t, c, nil)
	root.SetArgs([]string{"bogus"})
	if code := cli.Execute(root); code != 2 {
		t.Errorf("bogus: exit %d", code)
	}
	requireStdoutError(t, out.String(), errOut.String(), "unknown command")
}
