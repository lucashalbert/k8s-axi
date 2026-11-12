//go:build integration

package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// diagnose against a real cluster: each test builds one kind of broken or
// healthy workload and waits for the diagnosis to name it.

const envSentinel = "diag-env-literal-sentinel"

func sleeper(name string) corev1.Container {
	return corev1.Container{
		Name: name, Image: busybox, Command: []string{"sh", "-c", "sleep 3600"},
		Env: []corev1.EnvVar{{Name: "DB_PASSWORD", Value: envSentinel}},
	}
}

func diagUntil(t *testing.T, what string, within time.Duration, args []string, ok func(map[string]any) bool) map[string]any {
	t.Helper()
	var last map[string]any
	eventually(t, what, within, func() bool {
		last = itTry(append([]string{"diagnose"}, args...)...)
		return last != nil && ok(last)
	})
	return last
}

func ruleOf(m map[string]any, rule string) map[string]any {
	for _, f := range rowsOf(m, "findings") {
		if f["rule"] == rule {
			return f
		}
	}
	return nil
}

func evidenceHas(f map[string]any, sub string) bool {
	for _, e := range f["evidence"].([]any) {
		if strings.Contains(e.(string), sub) {
			return true
		}
	}
	return false
}

func hasRule(rule string) func(map[string]any) bool {
	return func(m map[string]any) bool { return ruleOf(m, rule) != nil }
}

func TestDiagnoseImagePullAndUnschedulable(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	labels := map[string]string{"app": "bad"}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns}, Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(2)), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.invalid/nope:1"}}}}}}
	if _, err := c.Typed.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pending := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: ns}, Spec: corev1.PodSpec{
		NodeSelector: map[string]string{"k8s-axi/none": "true"}, Containers: []corev1.Container{sleeper("app")}}}
	if _, err := c.Typed.CoreV1().Pods(ns).Create(ctx, pending, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	m := diagUntil(t, "both image pull failures to be diagnosed", 180*time.Second, []string{"deploy/bad", "-n", ns}, func(m map[string]any) bool {
		f := ruleOf(m, "image-pull")
		if f == nil || f["count"] != float64(2) {
			return false
		}
		pods := m["pods"].(map[string]any)
		return pods["total"] == float64(2) && pods["unhealthy"] == float64(2)
	})
	f := ruleOf(m, "image-pull")
	if m["status"] != "unhealthy" || f["basis"] != "observed" || f["severity"] != "critical" || f["count"] != float64(2) || !evidenceHas(f, "registry.invalid/nope:1") {
		t.Errorf("deployment diagnosis: %v", m)
	}
	if cause := ruleOf(m, "image-pull-cause"); cause == nil || cause["basis"] != "inferred" {
		t.Errorf("cause = %v", cause)
	}
	if u := ruleOf(m, "replicas-unavailable"); u == nil || u["basis"] != "observed" {
		t.Errorf("replicas-unavailable = %v", u)
	}
	if pods := m["pods"].(map[string]any); pods["total"] != float64(2) || pods["unhealthy"] != float64(2) {
		t.Errorf("pods = %v", pods)
	}
	if next := m["next"].([]any); len(next) == 0 {
		t.Errorf("next = %v", next)
	}

	p := diagUntil(t, "the unschedulable pod to be diagnosed", 120*time.Second, []string{"pod", "pending", "-n", ns}, hasRule("unschedulable"))
	if cause := ruleOf(p, "unschedulable-cause"); cause == nil || cause["basis"] != "inferred" || !strings.Contains(cause["summary"].(string), "nodeSelector") {
		t.Errorf("unschedulable-cause = %v", cause)
	}
	if p["status"] != "unhealthy" {
		t.Errorf("status = %v", p["status"])
	}
}

func TestDiagnoseCrashLoopAndFailedJob(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "crasher", Namespace: ns}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app", Image: busybox, Command: []string{"sh", "-c", "echo boom-line token=sentinelTok123abc; sleep 1; exit 3"}}}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "failing", Namespace: ns}, Spec: batchv1.JobSpec{
		BackoffLimit: ptr.To(int32(0)),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{
			Name: "app", Image: busybox, Command: []string{"sh", "-c", "echo job-boom; exit 1"}}}}}}}
	if _, err := c.Typed.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Typed.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	waitForRestartedPod(t, c, ns, "crasher", "app", 180*time.Second, "boom-line")
	m := diagUntil(t, "the crash loop with its previous log", 240*time.Second, []string{"pod", "crasher", "-n", ns},
		func(m map[string]any) bool {
			logs := rowsOf(m, "logs")
			if ruleOf(m, "crash-loop") == nil || len(logs) != 1 || logs[0]["previous"] != true {
				return false
			}
			lines := rowsOf(logs[0], "log")
			return len(lines) > 0 && strings.Contains(lines[0]["line"].(string), "boom-line")
		})
	f := ruleOf(m, "crash-loop")
	if f["basis"] != "observed" || f["severity"] != "critical" || !evidenceHas(f, "exit code 3") {
		t.Errorf("crash-loop = %v", f)
	}
	l := rowsOf(m, "logs")[0]
	lines := rowsOf(l, "log")
	if l["previous"] != true || len(lines) == 0 || !strings.Contains(lines[0]["line"].(string), "boom-line") {
		t.Errorf("log = %v", l)
	}
	code, out, _ := realRoot("diagnose", "pod", "crasher", "-n", ns, "--json", "--full")
	if code != 0 || strings.Contains(out, "sentinelTok123abc") || strings.Contains(out, envSentinel) {
		t.Errorf("a credential reached the output (exit %d)", code)
	}

	j := diagUntil(t, "the failed job to be diagnosed", 180*time.Second, []string{"job", "failing", "-n", ns}, hasRule("job-failed"))
	jf := ruleOf(j, "job-failed")
	if jf["severity"] != "critical" || jf["basis"] != "observed" || !evidenceHas(jf, "BackoffLimitExceeded") {
		t.Errorf("job-failed = %v", jf)
	}
	if len(rowsOf(j, "logs")) == 0 {
		t.Errorf("the failed pod's log is read: %v", j["logs"])
	}
}

func TestDiagnoseProbeAndServiceEndpoints(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	labels := map[string]string{"app": "probe"}
	ctr := sleeper("app")
	ctr.ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}},
		PeriodSeconds: 2, FailureThreshold: 1,
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unready", Namespace: ns, Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{ctr}}}
	if _, err := c.Typed.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, sel := range map[string]string{"svc-unready": "probe", "svc-typo": "probee"} {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": sel}, Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(80)}}}}
		if _, err := c.Typed.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	p := diagUntil(t, "the failing probe to be diagnosed", 180*time.Second, []string{"pod", "unready", "-n", ns}, hasRule("probe-failed"))
	if f := ruleOf(p, "probe-failed"); f["basis"] != "observed" || f["severity"] != "warning" || !evidenceHas(f, "Readiness probe failed") {
		t.Errorf("probe-failed = %v", f)
	}
	if p["status"] != "degraded" {
		t.Errorf("status = %v", p["status"])
	}

	s := diagUntil(t, "the service without ready endpoints", 120*time.Second, []string{"svc/svc-unready", "-n", ns}, hasRule("service-no-ready-endpoints"))
	if f := ruleOf(s, "service-no-ready-endpoints"); f["severity"] != "critical" || f["basis"] != "observed" {
		t.Errorf("service-no-ready-endpoints = %v", f)
	}
	if ruleOf(s, "probe-failed") == nil {
		t.Errorf("the pod's own finding appears under the service: %v", rowsOf(s, "findings"))
	}

	typo := diagUntil(t, "the service with a selector typo", 60*time.Second, []string{"svc", "svc-typo", "-n", ns}, hasRule("service-selector-mismatch"))
	if f := ruleOf(typo, "service-no-pods"); f == nil || f["basis"] != "observed" {
		t.Errorf("service-no-pods = %v", f)
	}
	if f := ruleOf(typo, "service-selector-mismatch"); f["basis"] != "inferred" || !evidenceHas(f, "app=probe (selector wants app=probee)") {
		t.Errorf("service-selector-mismatch = %v", f)
	}
}

func TestDiagnoseServiceSelectorMissingKey(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	labels := map[string]string{"app": "web"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-missing-tier", Namespace: ns, Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{sleeper("app")}}}
	if _, err := c.Typed.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-missing-key", Namespace: ns}, Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": "web", "tier": "front"}, Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(80)}}}}
	if _, err := c.Typed.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	m := diagUntil(t, "the service with a selector missing a required pod label", 60*time.Second, []string{"svc/svc-missing-key", "-n", ns},
		func(m map[string]any) bool {
			return ruleOf(m, "service-no-pods") != nil && ruleOf(m, "service-selector-mismatch") != nil
		})
	if f := ruleOf(m, "service-no-pods"); f["basis"] != "observed" || f["severity"] != "critical" {
		t.Errorf("service-no-pods = %v", f)
	}
	if f := ruleOf(m, "service-selector-mismatch"); f["basis"] != "inferred" || f["severity"] != "critical" || !evidenceHas(f, "pod web-missing-tier has missing tier (selector wants tier=front)") {
		t.Errorf("service-selector-mismatch = %v", f)
	}
}

func TestDiagnoseResourceQuota(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "pods", Namespace: ns}, Spec: corev1.ResourceQuotaSpec{
		Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("1")}}}
	if _, err := c.Typed.CoreV1().ResourceQuotas(ns).Create(ctx, q, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app": "quota"}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: ns}, Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(3)), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{sleeper("app")}}}}}
	if _, err := c.Typed.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	m := diagUntil(t, "the quota failure to be diagnosed", 180*time.Second, []string{"deploy/quota", "-n", ns},
		func(m map[string]any) bool {
			f := ruleOf(m, "quota-exceeded")
			return f != nil && f["basis"] == "observed"
		})
	f := ruleOf(m, "quota-exceeded")
	if f["severity"] != "critical" || f["basis"] != "observed" || !evidenceHas(f, "exceeded quota") || !evidenceHas(f, "ResourceQuota pods: pods 1/1") {
		t.Errorf("quota-exceeded = %v", f)
	}
	if m["findings"].([]any)[0].(map[string]any)["rule"] != "quota-exceeded" {
		t.Errorf("the cause ranks first: %v", rowsOf(m, "findings"))
	}
}

func TestDiagnoseHealthyWorkloads(t *testing.T) {
	c, ns := itSetup(t)
	ctx := context.Background()
	labels := map[string]string{"app": "ok"}
	tmpl := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
		Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}}, Containers: []corev1.Container{sleeper("app")}}}
	sel := &metav1.LabelSelector{MatchLabels: labels}
	if _, err := c.Typed.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: ns},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: sel, Template: tmpl}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Typed.AppsV1().StatefulSets(ns).Create(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "sts", Namespace: ns},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1)), ServiceName: "sts", Selector: sel, Template: tmpl}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Typed.AppsV1().DaemonSets(ns).Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: ns},
		Spec: appsv1.DaemonSetSpec{Selector: sel, Template: tmpl}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Typed.BatchV1().Jobs(ns).Create(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: ns},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "app", Image: busybox, Command: []string{"sh", "-c", "echo ok"}}}}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Typed.CoreV1().Services(ns).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: ns},
		Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 80}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	for _, target := range [][]string{{"deploy/dep"}, {"sts/sts"}, {"ds/ds"}, {"job/done"}, {"svc/svc"}} {
		args := append(append([]string{}, target...), "-n", ns)
		m := diagUntil(t, strings.Join(target, " ")+" to report healthy", 240*time.Second, args, func(m map[string]any) bool { return m["status"] == "healthy" })
		if m["count"] != float64(0) || !strings.Contains(m["conclusion"].(string), "no problems found") || len(m["checked"].([]any)) < 3 {
			t.Errorf("%v: %v", target, m)
		}
		code, out, _ := realRoot(append([]string{"diagnose"}, append(args, "--json", "--full")...)...)
		if code != 0 || strings.Contains(out, envSentinel) {
			t.Errorf("%v: env credential literal in --full output (exit %d)", target, code)
		}
	}
	podName := rowsOf(itJSON(t, "get", "pods", "-l", "app=ok", "-n", ns, "--limit", "1"), "items")[0]["name"].(string)
	if m := itJSON(t, "diagnose", "pod", podName, "-n", ns); m["status"] != "healthy" {
		t.Errorf("pod %s: %v", podName, m)
	}
}
