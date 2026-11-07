package kube

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

const twoContexts = `apiVersion: v1
kind: Config
current-context: alpha
clusters:
- name: alpha
  cluster: {server: "https://alpha.example:6443"}
- name: beta
  cluster: {server: "https://user:pw@beta.example:6443"}
contexts:
- name: alpha
  context: {cluster: alpha, user: u, namespace: team-a}
- name: beta
  context: {cluster: beta, user: u}
users:
- name: u
  user: {token: super-secret-token}
`

func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadContextFlagBeatsCurrent(t *testing.T) {
	p := writeKubeconfig(t, twoContexts)
	c, err := Load(Options{Kubeconfig: p, Context: "beta", Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "beta" || c.Server != "beta.example:6443" || c.Namespace != "default" {
		t.Errorf("got context=%q server=%q ns=%q", c.Context, c.Server, c.Namespace)
	}
	if c.REST.Timeout != 3*time.Second {
		t.Errorf("timeout %v", c.REST.Timeout)
	}
	if c.Typed == nil || c.Dynamic == nil || c.Discovery == nil || c.Mapper == nil {
		t.Error("client set incomplete")
	}
}

func TestLoadUsesCurrentContextAndNamespace(t *testing.T) {
	p := writeKubeconfig(t, twoContexts)
	c, err := Load(Options{Kubeconfig: p})
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "alpha" || c.Namespace != "team-a" {
		t.Errorf("got context=%q ns=%q", c.Context, c.Namespace)
	}
	c, err = Load(Options{Kubeconfig: p, Namespace: "override"})
	if err != nil || c.Namespace != "override" {
		t.Errorf("namespace flag: %v %v", c, err)
	}
}

func assertUsage(t *testing.T, err error, wantContexts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	if got := axierr.ExitCode(err); got != 2 {
		t.Fatalf("exit code %d, want 2: %v", got, err)
	}
	for _, name := range wantContexts {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not list context %q: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("error leaks a credential: %v", err)
	}
}

func TestLoadUnknownContextListsContexts(t *testing.T) {
	_, err := Load(Options{Kubeconfig: writeKubeconfig(t, twoContexts), Context: "nope"})
	assertUsage(t, err, "alpha", "beta", "nope")
}

func TestLoadNoCurrentContextListsContexts(t *testing.T) {
	body := strings.Replace(twoContexts, "current-context: alpha\n", "", 1)
	_, err := Load(Options{Kubeconfig: writeKubeconfig(t, body)})
	assertUsage(t, err, "alpha", "beta")
}

func TestLoadMissingKubeconfigIsUsage(t *testing.T) {
	_, err := Load(Options{Kubeconfig: filepath.Join(t.TempDir(), "absent")})
	assertUsage(t, err)
}

func TestLoadEmptyKubeconfigIsUsage(t *testing.T) {
	_, err := Load(Options{Kubeconfig: writeKubeconfig(t, "apiVersion: v1\nkind: Config\n")})
	assertUsage(t, err)
}

func TestLoadNeverModifiesKubeconfig(t *testing.T) {
	p := writeKubeconfig(t, twoContexts)
	before, _ := os.ReadFile(p)
	st, _ := os.Stat(p)
	for _, o := range []Options{{Kubeconfig: p}, {Kubeconfig: p, Context: "beta"}, {Kubeconfig: p, Context: "nope"}} {
		_, _ = Load(o)
	}
	after, _ := os.ReadFile(p)
	st2, _ := os.Stat(p)
	if !bytes.Equal(before, after) || !st.ModTime().Equal(st2.ModTime()) {
		t.Error("kubeconfig changed")
	}
}

func TestLoadKeepsExecCredentialPlugins(t *testing.T) {
	body := strings.Replace(twoContexts, "user: {token: super-secret-token}", `user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: aws
      args: [eks, get-token]
      interactiveMode: Never`, 1)
	c, err := Load(Options{Kubeconfig: writeKubeconfig(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	if c.REST.ExecProvider == nil || c.REST.ExecProvider.Command != "aws" {
		t.Errorf("exec provider not preserved: %+v", c.REST.ExecProvider)
	}
}

func TestLoadBrokenKubeconfigIsRuntime(t *testing.T) {
	_, err := Load(Options{Kubeconfig: writeKubeconfig(t, "{{{ not yaml")})
	var ae *axierr.Error
	if !errors.As(err, &ae) || ae.Code != axierr.CodeRuntime {
		t.Fatalf("want runtime error, got %v", err)
	}
}
