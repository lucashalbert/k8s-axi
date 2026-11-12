//go:build integration

package kube

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSmokeEveryCommandAgainstKind runs every command once, with a real
// object where one is needed, on each Kubernetes minor of the kind matrix.
// The deeper cases (CRD in a non-preferred version, Secret redaction, delete
// confirmation, RBAC errors) live in the other integration tests and run in
// the same matrix.
func TestSmokeEveryCommandAgainstKind(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	itDeployment(t, c, ns, "web", 1)
	eventually(t, "the pod to run", 3*time.Minute, func() bool {
		pods, err := c.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=web"})
		return err == nil && len(pods.Items) == 1 && pods.Items[0].Status.Phase == corev1.PodRunning
	})
	dir := t.TempDir()
	manifest := filepath.Join(dir, "cm.yaml")
	if err := os.WriteFile(manifest, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: smoke\ndata:\n  k: v\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ran := map[string]bool{}
	smoke := func(path string, wantExit int, args ...string) string {
		t.Helper()
		ran[path] = true
		code, out, errOut := realRoot(append(strings.Fields(path), args...)...)
		if code != wantExit {
			t.Errorf("%s: exit %d, want %d: %s", path, code, wantExit, errOut)
		}
		if out == "" {
			t.Errorf("%s printed nothing", path)
		}
		return out
	}
	nsFlag := []string{"-n", ns, "--json"}
	with := func(args ...string) []string { return append(args, nsFlag...) }

	if got := decode(t, smoke("version", 0, "--json")); got["version"] == nil {
		t.Errorf("version: %v", got)
	}
	smoke("update", 0, "--help")
	if got := decode(t, smoke("cluster info", 0, "--json")); got["reachable"] != true {
		t.Errorf("cluster info: %v", got)
	}
	if got := decode(t, smoke("discover", 0, "--json", "--kind", "deployment")); got["matched"] != float64(1) {
		t.Errorf("discover: %v", got)
	}
	smoke("get", 0, with("pods")...)
	smoke("inspect", 0, with("deploy", "web")...)
	smoke("events", 0, nsFlag...)
	smoke("logs", 0, with("deploy/web")...)
	if got := decode(t, smoke("diagnose", 0, with("deploy/web")...)); got["status"] == nil {
		t.Errorf("diagnose: %v", got)
	}

	smoke("plan scale", 0, with("deploy/web", "--replicas", "2")...)
	smoke("plan rollout restart", 0, with("deploy/web")...)
	smoke("plan apply", 0, with("-f", manifest)...)
	smoke("plan delete", 0, with("deploy/web")...)
	smoke("scale", 0, with("deploy/web", "--replicas", "2")...)
	smoke("rollout restart", 0, with("deploy/web")...)
	smoke("apply", 0, with("-f", manifest)...)
	smoke("delete", 0, with("configmap/smoke", "--confirm", "configmap/smoke")...)
	if _, err := c.Typed.CoreV1().ConfigMaps(ns).Get(ctx, "smoke", metav1.GetOptions{}); err == nil {
		t.Error("delete left the config map")
	}
	smoke("setup hooks", 0, "--print")
	smoke("setup skill", 0, "--dest", filepath.Join(dir, "skill"))

	var got []string
	for p := range ran {
		got = append(got, p)
	}
	sort.Strings(got)
	want := slices.Clone(smokePaths)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("smoke ran %v, want every command %v", got, want)
	}
}

// TestHomeViewAgainstKind runs k8s-axi with no arguments, which is not a
// subcommand and so not in smokePaths: it must print live cluster data.
func TestHomeViewAgainstKind(t *testing.T) {
	c, _ := itSetup(t)
	code, out, errOut := realRoot("--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := decode(t, out)
	if got["state"] != "ready" || got["reachable"] != true || got["context"] != c.Context || got["version"] == "" {
		t.Errorf("home: %v", got)
	}
}
