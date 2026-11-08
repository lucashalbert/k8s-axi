package kube

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// diagNow is the fixed clock of the diagnose tests.
var diagNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func freezeClock(t *testing.T) {
	t.Helper()
	old := now
	now = func() time.Time { return diagNow }
	t.Cleanup(func() { now = old })
}

// obj parses a YAML fixture into an unstructured object.
func obj(t *testing.T, y string) unstructured.Unstructured {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatalf("bad fixture: %v\n%s", err, y)
	}
	return unstructured.Unstructured{Object: m}
}

func objPtr(t *testing.T, y string) *unstructured.Unstructured {
	u := obj(t, y)
	return &u
}

// warn builds a Warning event about an object.
func warn(kind, name, reason, msg string, count int32) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name + "." + reason, Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: "default"},
		Type:           corev1.EventTypeWarning, Reason: reason, Message: msg, Count: count,
		LastTimestamp: metav1.NewTime(diagNow.Add(-2 * time.Minute)),
	}
}

// ruleSet returns the rules of findings, in output order.
func ruleSet(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func find(fs []finding, rule string) *finding {
	for i := range fs {
		if fs[i].Rule == rule {
			return &fs[i]
		}
	}
	return nil
}

// podFix is a pod named name with one container "app" (image registry.test/app:1,
// memory limit 64Mi, cpu request 500m) and the given status fragment, which is
// indented YAML under "status:".
func podFix(t *testing.T, name, status string) unstructured.Unstructured {
	t.Helper()
	return obj(t, `
apiVersion: v1
kind: Pod
metadata: {name: `+name+`, namespace: default, uid: uid-`+name+`}
spec:
  nodeName: node-1
  containers:
  - name: app
    image: registry.test/app:1
    resources:
      limits: {memory: 64Mi}
      requests: {cpu: 500m, memory: 32Mi}
status:
`+status)
}

func snapOf(t *testing.T, resource string, target *unstructured.Unstructured, pods ...unstructured.Unstructured) *snapshot {
	t.Helper()
	freezeClock(t)
	r := map[string]Resource{
		"pods":         {GVR: gvr("", "v1", "pods"), Kind: "Pod", Namespaced: true},
		"services":     {GVR: gvr("", "v1", "services"), Kind: "Service", Namespaced: true},
		"deployments":  {GVR: gvr("apps", "v1", "deployments"), Kind: "Deployment", Namespaced: true},
		"statefulsets": {GVR: gvr("apps", "v1", "statefulsets"), Kind: "StatefulSet", Namespaced: true},
		"daemonsets":   {GVR: gvr("apps", "v1", "daemonsets"), Kind: "DaemonSet", Namespaced: true},
		"jobs":         {GVR: gvr("batch", "v1", "jobs"), Kind: "Job", Namespaced: true},
	}[resource]
	return &snapshot{Resource: r, Target: target, Pods: pods, Nodes: map[string]*unstructured.Unstructured{}}
}
