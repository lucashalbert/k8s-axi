//go:build integration

package kube

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
)

// The mutating commands against a real cluster. Each test works in its own
// namespace, and the kubeconfig the commands read must come out unchanged.

func itPodTemplate(app string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			Containers:                    []corev1.Container{{Name: "c", Image: busybox, Command: []string{"sleep", "3600"}}},
		},
	}
}

func itDeployment(t *testing.T, c *Clients, ns, name string, replicas int32) {
	t.Helper()
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
	_, err := c.Typed.AppsV1().Deployments(ns).Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Selector: sel, Template: itPodTemplate(name)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// itKubeconfig writes a kubeconfig with two contexts and returns its path:
// "decoy" is current and unreachable, "real" reaches the test cluster and
// names no namespace. A token of "" reuses the test client's own credentials.
func itKubeconfig(t *testing.T, c *Clients, token string) string {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["real"] = &clientcmdapi.Cluster{Server: c.REST.Host, CertificateAuthorityData: c.REST.CAData, CertificateAuthority: c.REST.CAFile}
	cfg.Clusters["decoy"] = &clientcmdapi.Cluster{Server: "https://192.0.2.1:6443"}
	user := &clientcmdapi.AuthInfo{Token: token}
	if token == "" {
		user = &clientcmdapi.AuthInfo{ClientCertificateData: c.REST.CertData, ClientKeyData: c.REST.KeyData, Token: c.REST.BearerToken,
			ClientCertificate: c.REST.CertFile, ClientKey: c.REST.KeyFile}
	}
	cfg.AuthInfos["u"] = user
	cfg.Contexts["real"] = &clientcmdapi.Context{Cluster: "real", AuthInfo: "u"}
	cfg.Contexts["decoy"] = &clientcmdapi.Context{Cluster: "decoy", AuthInfo: "u"}
	cfg.CurrentContext = "decoy"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// itMutate runs a mutating command and returns its plan and result documents.
func itMutate(t *testing.T, kc string, args ...string) (plan, result map[string]any) {
	t.Helper()
	code, out, errOut := realRoot(append(args, "--kubeconfig", kc, "--context", "real", "--json")...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, errOut)
	}
	docs := jsonDocs(t, out)
	if len(docs) == 0 {
		t.Fatalf("%v printed nothing", args)
	}
	plan = docs[0]["plan"].(map[string]any)
	if len(docs) > 1 {
		result = docs[1]["result"].(map[string]any)
	}
	return plan, result
}

func itReplicas(t *testing.T, c *Clients, ns, name string) int32 {
	t.Helper()
	d, err := c.Typed.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func TestMutateScaleDeployment(t *testing.T) {
	c, ns := itSetup(t)
	itDeployment(t, c, ns, "web", 1)
	kc := itKubeconfig(t, c, "")
	before, _ := os.ReadFile(kc)

	plan, res := itMutate(t, kc, "scale", "deploy/web", "--replicas", "3", "-n", ns)
	scope := plan["scope"].(map[string]any)
	if plan["target"] != "deployment/web" || scope["context"] != "real" || scope["namespace"] != ns || res["status"] != "applied" {
		t.Errorf("plan %v result %v", plan, res)
	}
	if got := itReplicas(t, c, ns, "web"); got != 3 {
		t.Errorf("replicas = %d, want 3", got)
	}

	_, res = itMutate(t, kc, "scale", "deploy/web", "--replicas", "6", "--dry-run=server", "-n", ns)
	if res["status"] != "validated" || itReplicas(t, c, ns, "web") != 3 {
		t.Errorf("dry run: result %v, replicas %d", res, itReplicas(t, c, ns, "web"))
	}
	plan, res = itMutate(t, kc, "plan", "scale", "deploy/web", "--replicas", "2", "-n", ns)
	if plan["mode"] != "dry-run" || res["status"] != "validated" || itReplicas(t, c, ns, "web") != 3 {
		t.Errorf("plan: %v %v replicas %d", plan, res, itReplicas(t, c, ns, "web"))
	}
	plan, res = itMutate(t, kc, "scale", "deploy/web", "--replicas", "3", "-n", ns)
	if plan["noop"] != true || res != nil {
		t.Errorf("same replicas: plan %v result %v", plan, res)
	}

	plan, _ = itMutate(t, kc, "scale", "deploy/web", "--replicas", "0", "-n", ns)
	if ch := plan["changes"].([]any); len(ch) != 2 || !strings.HasPrefix(ch[1].(string), "notable: scales to zero") {
		t.Errorf("scale to zero changes = %v", ch)
	}
	if got := itReplicas(t, c, ns, "web"); got != 0 {
		t.Errorf("replicas = %d, want 0", got)
	}

	// No namespace on the context and no -n: refused before anything is sent.
	if code, out, errOut := realRoot("scale", "deploy/web", "--replicas", "1", "--kubeconfig", kc, "--context", "real"); code != 2 || out != "" || !strings.Contains(errOut, "ambiguous") {
		t.Errorf("no namespace: exit %d stdout=%q stderr=%s", code, out, errOut)
	}
	if code, _, _ := realRoot("scale", "deploy/web", "--replicas", "1", "-n", ns, "--dry-run=client", "--kubeconfig", kc, "--context", "real"); code != 2 {
		t.Errorf("client dry-run exit %d, want 2", code)
	}

	after, _ := os.ReadFile(kc)
	if !bytes.Equal(before, after) {
		t.Error("the kubeconfig changed")
	}
	raw, err := clientcmd.LoadFromFile(kc)
	if err != nil || raw.CurrentContext != "decoy" {
		t.Errorf("current context = %q (%v), want decoy", raw.CurrentContext, err)
	}
}

func TestMutateScaleStatefulSetAndCustomResource(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	kc := itKubeconfig(t, c, "")

	_, err := c.Typed.AppsV1().StatefulSets(ns).Create(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(0)), ServiceName: "db",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}, Template: itPodTemplate("db")},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	itMutate(t, kc, "scale", "statefulset/db", "--replicas", "2", "-n", ns)
	if s, _ := c.Typed.AppsV1().StatefulSets(ns).Get(ctx, "db", metav1.GetOptions{}); *s.Spec.Replicas != 2 {
		t.Errorf("statefulset replicas = %d", *s.Spec.Replicas)
	}

	// A custom resource that declares a scale subresource scales through discovery alone.
	const group = "mutate.example.com"
	crd := crdWithFreeSpec(group, "gadgets", "Gadget", "v1")
	crd.Object["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["subresources"] = map[string]any{
		"scale": map[string]any{"specReplicasPath": ".spec.replicas", "statusReplicasPath": ".status.replicas"},
	}
	crds := c.Dynamic.Resource(crdGVR)
	if _, err := crds.Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = crds.Delete(ctx, crd.GetName(), metav1.DeleteOptions{}) })
	eventually(t, "the CRD to be served", 90*time.Second, func() bool {
		return itTry("get", "gadgets."+group, "-n", ns) != nil
	})
	gvr := gadgetGVR
	gvr.Group = group
	g := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1", "kind": "Gadget",
		"metadata": map[string]any{"name": "g", "namespace": ns}, "spec": map[string]any{"replicas": int64(1)},
	}}
	if _, err := c.Dynamic.Resource(gvr).Namespace(ns).Create(ctx, g, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	itMutate(t, kc, "scale", "gadgets."+group+"/g", "--replicas", "4", "-n", ns)
	got, _ := c.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, "g", metav1.GetOptions{})
	if n, _, _ := unstructured.NestedInt64(got.Object, "spec", "replicas"); n != 4 {
		t.Errorf("custom resource replicas = %d", n)
	}

	// A kind without a scale subresource is a usage error.
	if code, _, errOut := realRoot("scale", "configmaps/x", "--replicas", "1", "-n", ns, "--kubeconfig", kc, "--context", "real"); code != 2 || !strings.Contains(errOut, "no scale subresource") {
		t.Errorf("configmap: exit %d: %s", code, errOut)
	}
}

func TestMutateRolloutRestart(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	kc := itKubeconfig(t, c, "")
	itDeployment(t, c, ns, "web", 1)
	podName := func() string {
		pods, err := c.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=web"})
		if err != nil || len(pods.Items) != 1 || pods.Items[0].Status.Phase != corev1.PodRunning || pods.Items[0].DeletionTimestamp != nil {
			return ""
		}
		return pods.Items[0].Name
	}
	var first string
	eventually(t, "the first pod to run", 3*time.Minute, func() bool { first = podName(); return first != "" })

	plan, res := itMutate(t, kc, "rollout", "restart", "deploy/web", "--dry-run=server", "-n", ns)
	if plan["mode"] != "dry-run" || res["status"] != "validated" {
		t.Errorf("dry run: %v %v", plan, res)
	}
	if d, _ := c.Typed.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{}); d.Spec.Template.Annotations[restartedAtKey] != "" {
		t.Error("dry run set the annotation")
	}
	plan, res = itMutate(t, kc, "plan", "rollout", "restart", "deploy/web", "-n", ns)
	if plan["operation"] != "rollout restart" || res["status"] != "validated" {
		t.Errorf("plan: %v %v", plan, res)
	}

	plan, res = itMutate(t, kc, "rollout", "restart", "deploy/web", "-n", ns)
	stamp := res["restarted_at"].(string)
	if res["status"] != "applied" || !strings.Contains(plan["changes"].([]any)[0].(string), stamp) {
		t.Errorf("plan %v result %v", plan, res)
	}
	if d, _ := c.Typed.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{}); d.Spec.Template.Annotations[restartedAtKey] != stamp {
		t.Errorf("annotation = %q, want %q", d.Spec.Template.Annotations[restartedAtKey], stamp)
	}
	eventually(t, "the pod to be replaced", 3*time.Minute, func() bool { n := podName(); return n != "" && n != first })

	// StatefulSets and DaemonSets take the same annotation.
	_, err := c.Typed.AppsV1().StatefulSets(ns).Create(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(0)), ServiceName: "db",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}, Template: itPodTemplate("db")},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = itMutate(t, kc, "rollout", "restart", "sts/db", "-n", ns)
	if ch := plan["changes"].([]any); !strings.HasPrefix(ch[len(ch)-1].(string), "notable: 0 replicas") {
		t.Errorf("zero replica statefulset changes = %v", ch)
	}
	if s, _ := c.Typed.AppsV1().StatefulSets(ns).Get(ctx, "db", metav1.GetOptions{}); s.Spec.Template.Annotations[restartedAtKey] == "" {
		t.Error("statefulset annotation missing")
	}
	_, err = c.Typed.AppsV1().DaemonSets(ns).Create(ctx, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}}, Template: itPodTemplate("agent")},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	itMutate(t, kc, "rollout", "restart", "ds/agent", "-n", ns)
	if d, _ := c.Typed.AppsV1().DaemonSets(ns).Get(ctx, "agent", metav1.GetOptions{}); d.Spec.Template.Annotations[restartedAtKey] == "" {
		t.Error("daemonset annotation missing")
	}

	if code, _, errOut := realRoot("rollout", "restart", "cm/x", "-n", ns, "--kubeconfig", kc, "--context", "real"); code != 2 || !strings.Contains(errOut, "supports deployments") {
		t.Errorf("configmap: exit %d: %s", code, errOut)
	}
}

func TestMutateForbiddenIsAStructuredError(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	itDeployment(t, c, ns, "web", 1)
	if _, err := c.Typed.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "viewer", Namespace: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "viewer", Namespace: ns}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "deployments/scale"}, Verbs: []string{"get"}},
	}}
	if _, err := c.Typed.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "viewer", Namespace: ns},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "viewer"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "viewer", Namespace: ns}}}
	if _, err := c.Typed.RbacV1().RoleBindings(ns).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	tok, err := c.Typed.CoreV1().ServiceAccounts(ns).CreateToken(ctx, "viewer", &authv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token := tok.Status.Token
	kc := itKubeconfig(t, c, token)

	for _, args := range [][]string{
		{"scale", "deploy/web", "--replicas", "2"},
		{"plan", "scale", "deploy/web", "--replicas", "2"},
		{"rollout", "restart", "deploy/web"},
	} {
		// RBAC changes take a moment to reach the authorizer, and a get that is
		// not yet allowed also fails with exit 1, so retry until the verb is named.
		var errOut string
		eventually(t, strings.Join(args, " ")+" to be denied for its verb", 60*time.Second, func() bool {
			var code int
			code, _, errOut = realRoot(append(args, "-n", ns, "--kubeconfig", kc, "--context", "real")...)
			return code == 1 && (strings.Contains(errOut, "'update'") || strings.Contains(errOut, "'patch'"))
		})
		if !strings.Contains(errOut, "RBAC denied") || strings.Contains(errOut, token) {
			t.Errorf("%v: stderr=%s", args, errOut)
		}
	}
	if got := itReplicas(t, c, ns, "web"); got != 1 {
		t.Errorf("a denied scale changed replicas to %d", got)
	}
}
