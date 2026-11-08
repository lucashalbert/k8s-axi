//go:build integration

package kube

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
)

// apply and delete against a real cluster. Each test works in its own
// namespace, and the kubeconfig the commands read must come out unchanged.

func itKubectl(t *testing.T, kc string, args ...string) (code int, docs []map[string]any, errOut string) {
	t.Helper()
	code, out, errOut := realRoot(append(args, "--kubeconfig", kc, "--context", "real", "--json")...)
	return code, jsonDocs(t, out), errOut
}

func itStatuses(t *testing.T, docs []map[string]any) map[string]string {
	t.Helper()
	if len(docs) < 2 {
		t.Fatalf("want a plan and a result, got %v", docs)
	}
	return objStatuses(t, docs[1]["result"].(map[string]any))
}

func itKubeconfigUnchanged(t *testing.T, kc string, before []byte) {
	t.Helper()
	after, _ := os.ReadFile(kc)
	if !bytes.Equal(before, after) {
		t.Error("the kubeconfig changed")
	}
	if raw, err := clientcmd.LoadFromFile(kc); err != nil || raw.CurrentContext != "decoy" {
		t.Errorf("current context = %q (%v), want decoy", raw.CurrentContext, err)
	}
}

func itCRDYAML(group string) string {
	return fmt.Sprintf(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: sprockets.%[1]s}
spec:
  group: %[1]s
  scope: Namespaced
  names: {plural: sprockets, singular: sprocket, kind: Sprocket, listKind: SprocketList}
  versions:
  - name: v1
    served: true
    storage: true
    schema: {openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}}
`, group)
}

func itSprocketYAML(group string, teeth int) string {
	return fmt.Sprintf("apiVersion: %s/v1\nkind: Sprocket\nmetadata: {name: s1}\nspec: {teeth: %d}\n", group, teeth)
}

func itCleanupCRD(t *testing.T, c *Clients, name string) {
	t.Helper()
	t.Cleanup(func() {
		_ = c.Dynamic.Resource(crdGVR).Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

func TestApplyCRDAndCustomResource(t *testing.T) {
	c, ns := itSetup(t)
	kc := itKubeconfig(t, c, "")
	before, _ := os.ReadFile(kc)
	const group = "apply.example.com"
	itCleanupCRD(t, c, "sprockets."+group)
	sprocketGVR := schema.GroupVersionResource{Group: group, Version: "v1", Resource: "sprockets"}
	teeth := func() int64 {
		o, err := c.Dynamic.Resource(sprocketGVR).Namespace(ns).Get(context.Background(), "s1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		n, _, _ := unstructured.NestedInt64(o.Object, "spec", "teeth")
		return n
	}

	// The custom resource is listed before its CRD, which the command orders.
	file := writeFile(t, t.TempDir(), "all.yaml", itSprocketYAML(group, 9)+"---\n"+itCRDYAML(group))
	code, docs, errOut := itKubectl(t, kc, "apply", "-f", file, "-n", ns)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if st := itStatuses(t, docs); st["sprocket/s1"] != "created" || st["customresourcedefinition/sprockets."+group] != "created" {
		t.Fatalf("statuses = %v", st)
	}
	if got := teeth(); got != 9 {
		t.Errorf("teeth = %d, want 9", got)
	}

	// Again: nothing to do.
	code, docs, errOut = itKubectl(t, kc, "apply", "-f", file, "-n", ns)
	if code != 0 || docs[0]["plan"].(map[string]any)["noop"] != true {
		t.Fatalf("re-apply: exit %d, plan %v, %s", code, docs, errOut)
	}
	if st := itStatuses(t, docs); st["sprocket/s1"] != "unchanged" {
		t.Errorf("re-apply statuses = %v", st)
	}

	// A directory of files, one changed and one new; --dry-run=server first.
	dir := t.TempDir()
	writeFile(t, dir, "10-sprocket.yaml", itSprocketYAML(group, 12))
	writeFile(t, dir, "20-cm.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg"},"data":{"a":"1"}}`)
	code, docs, errOut = itKubectl(t, kc, "apply", "-f", dir, "-n", ns, "--dry-run=server")
	if code != 0 {
		t.Fatalf("dry run: exit %d: %s", code, errOut)
	}
	if st := itStatuses(t, docs); st["sprocket/s1"] != "would-configure" || st["configmap/cfg"] != "would-create" {
		t.Errorf("dry-run statuses = %v", st)
	}
	if got := teeth(); got != 9 {
		t.Errorf("a dry run changed teeth to %d", got)
	}
	code, docs, errOut = itKubectl(t, kc, "apply", "-f", dir, "-n", ns)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if st := itStatuses(t, docs); st["sprocket/s1"] != "configured" || st["configmap/cfg"] != "created" {
		t.Errorf("statuses = %v", st)
	}
	if got := teeth(); got != 12 {
		t.Errorf("teeth = %d, want 12", got)
	}
	itKubeconfigUnchanged(t, kc, before)
}

func TestApplyConflictNeedsForce(t *testing.T) {
	c, ns := itSetup(t)
	kc := itKubeconfig(t, c, "")
	before, _ := os.ReadFile(kc)
	itDeployment(t, c, ns, "web", 1) // created by another field manager
	file := writeFile(t, t.TempDir(), "web.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec: {replicas: 3}\n")

	code, docs, errOut := itKubectl(t, kc, "apply", "-f", file, "-n", ns)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, errOut)
	}
	changes := docs[0]["plan"].(map[string]any)["changes"].([]any)
	if len(changes) != 1 || !strings.HasPrefix(changes[0].(string), "conflict deployment/web") || !strings.Contains(changes[0].(string), ".spec.replicas") {
		t.Errorf("changes = %v", changes)
	}
	if !strings.Contains(errOut, "--force-conflicts") {
		t.Errorf("stderr = %s", errOut)
	}
	if got := itReplicas(t, c, ns, "web"); got != 1 {
		t.Errorf("a conflict changed replicas to %d", got)
	}

	code, docs, errOut = itKubectl(t, kc, "apply", "-f", file, "-n", ns, "--force-conflicts")
	if code != 0 {
		t.Fatalf("forced: exit %d: %s", code, errOut)
	}
	if st := itStatuses(t, docs); st["deployment/web"] != "configured" {
		t.Errorf("statuses = %v", st)
	}
	if got := itReplicas(t, c, ns, "web"); got != 3 {
		t.Errorf("replicas = %d, want 3", got)
	}
	itKubeconfigUnchanged(t, kc, before)
}

func TestApplyNamespaceInManifestAndSecretValues(t *testing.T) {
	c, ns := itSetup(t)
	kc := itKubeconfig(t, c, "")
	extra := ns + "-x"
	if len(extra) > 63 {
		extra = extra[:63]
	}
	extra = strings.TrimRight(extra, "-")
	t.Cleanup(func() { _ = c.Typed.CoreV1().Namespaces().Delete(context.Background(), extra, metav1.DeleteOptions{}) })
	const value = "hunter2-super-secret"
	body := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: %[1]s}
data: {a: "1"}
---
apiVersion: v1
kind: Secret
metadata: {name: db, namespace: %[3]s}
stringData: {password: %[2]s}
---
apiVersion: v1
kind: Namespace
metadata: {name: %[1]s}
`, extra, value, ns)
	file := writeFile(t, t.TempDir(), "m.yaml", body)

	for _, args := range [][]string{{"--dry-run=server"}, {}} {
		code, out, errOut := realRoot(append([]string{"apply", "-f", file, "--kubeconfig", kc, "--context", "real"}, args...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		if strings.Contains(out+errOut, value) || !strings.Contains(out, "password 20 B") {
			t.Errorf("%v: Secret value leaked or keys missing:\n%s%s", args, out, errOut)
		}
	}
	if _, err := c.Typed.CoreV1().ConfigMaps(extra).Get(context.Background(), "cfg", metav1.GetOptions{}); err != nil {
		t.Errorf("configmap in the new namespace: %v", err)
	}
	if s, err := c.Typed.CoreV1().Secrets(ns).Get(context.Background(), "db", metav1.GetOptions{}); err != nil || string(s.Data["password"]) != value {
		t.Errorf("secret not stored: %v", err)
	}
	// Invalid objects fail alone and never echo values.
	bad := writeFile(t, t.TempDir(), "bad.yaml", fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata: {name: Bad_Name}\nstringData: {password: %s}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: ok}\n", value))
	code, out, errOut := realRoot("apply", "-f", bad, "-n", ns, "--kubeconfig", kc, "--context", "real")
	if code != 1 || strings.Contains(out+errOut, value) || !strings.Contains(errOut, "1 of 2") {
		t.Errorf("partial failure: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := c.Typed.CoreV1().ConfigMaps(ns).Get(context.Background(), "ok", metav1.GetOptions{}); err != nil {
		t.Errorf("the valid object must still be applied: %v", err)
	}
}

func TestDeleteNeedsTheExactToken(t *testing.T) {
	c, ns := itSetup(t)
	kc := itKubeconfig(t, c, "")
	before, _ := os.ReadFile(kc)
	itDeployment(t, c, ns, "web", 1)
	itDeployment(t, c, ns, "other", 1)
	exists := func(name string) bool {
		_, err := c.Typed.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
		return err == nil
	}

	for _, args := range [][]string{
		{"delete", "deploy/web", "-n", ns},
		{"delete", "deploy/web", "-n", ns, "--confirm", "deploy/web"},
		{"delete", "deploy/web", "-n", ns, "--confirm", "deployment/other"},
	} {
		code, out, errOut := realRoot(append(args, "--kubeconfig", kc, "--context", "real")...)
		if code != 2 || out != "" || !strings.Contains(errOut, "deployment/web") || !strings.Contains(errOut, "--confirm") {
			t.Errorf("%v: exit %d stdout=%q stderr=%s", args, code, out, errOut)
		}
		if !exists("web") {
			t.Fatalf("%v deleted the deployment", args)
		}
	}
	for _, args := range [][]string{{"delete", "deploy", "--all", "-n", ns}, {"delete", "deploy", "-l", "app=web", "-n", ns}, {"delete", "deploy/*", "-n", ns, "--confirm", "deployment/*"}} {
		if code, _, _ := realRoot(append(args, "--kubeconfig", kc, "--context", "real")...); code != 2 || !exists("web") {
			t.Errorf("%v: exit %d, exists %v", args, code, exists("web"))
		}
	}

	// A dry run needs no token and deletes nothing.
	code, docs, errOut := itKubectl(t, kc, "delete", "deploy/web", "-n", ns, "--dry-run=server")
	if code != 0 || docs[1]["result"].(map[string]any)["status"] != "validated" || !exists("web") {
		t.Fatalf("dry run: exit %d: %s %v", code, errOut, docs)
	}

	code, docs, errOut = itKubectl(t, kc, "delete", "deploy/web", "-n", ns, "--confirm", "deployment/web")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	plan := docs[0]["plan"].(map[string]any)
	if plan["destructive"] != true || plan["scope"].(map[string]any)["context"] != "real" {
		t.Errorf("plan = %v", plan)
	}
	if status := docs[1]["result"].(map[string]any)["status"]; status != "deleted" && status != "terminating" {
		t.Errorf("status = %v", status)
	}
	eventually(t, "the deployment to be gone", 60*time.Second, func() bool { return !exists("web") })
	if !exists("other") {
		t.Error("delete removed deployment/other")
	}
	itKubeconfigUnchanged(t, kc, before)
}

func TestDeleteWarnsForNamespaceAndCRD(t *testing.T) {
	c, ns := itSetup(t)
	kc := itKubeconfig(t, c, "")
	const group = "delete.example.com"
	crd := crdWithFreeSpec(group, "gadgets", "Gadget", "v1")
	if _, err := c.Dynamic.Resource(crdGVR).Create(context.Background(), crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	itCleanupCRD(t, c, crd.GetName())

	for _, tc := range []struct{ target, token, want string }{
		{"namespace/" + ns, "namespace/" + ns, "every object in it"},
		{"crd/" + crd.GetName(), "customresourcedefinition/" + crd.GetName(), "Gadget." + group},
	} {
		code, docs, errOut := itKubectl(t, kc, "delete", tc.target, "--confirm", tc.token, "--dry-run=server")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.target, code, errOut)
		}
		changes := docs[0]["plan"].(map[string]any)["changes"].([]any)
		if len(changes) < 2 || !strings.HasPrefix(changes[1].(string), "WARNING:") || !strings.Contains(changes[1].(string), tc.want) {
			t.Errorf("%s: changes = %v", tc.target, changes)
		}
	}
	if _, err := c.Typed.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Errorf("a dry run deleted the namespace: %v", err)
	}
}
