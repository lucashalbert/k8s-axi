package kube

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func svcFix(t *testing.T, spec string) *unstructured.Unstructured {
	return objPtr(t, `
apiVersion: v1
kind: Service
metadata: {name: web, namespace: default}
spec:
`+spec+`
status: {loadBalancer: {}}`)
}

func slice(t *testing.T, ready, notReady int) unstructured.Unstructured {
	var eps string
	for i := 0; i < ready; i++ {
		eps += "\n  - {addresses: [10.0.0.1], conditions: {ready: true}}"
	}
	for i := 0; i < notReady; i++ {
		eps += "\n  - {addresses: [10.0.0.2], conditions: {ready: false}}"
	}
	if eps == "" {
		eps = " []"
	}
	return obj(t, "kind: EndpointSlice\nmetadata: {name: web-abc, namespace: default}\nendpoints:"+eps)
}

func readyPod(t *testing.T, name string, ports string) unstructured.Unstructured {
	p := podFix(t, name, `
  phase: Running
  conditions: [{type: Ready, status: "True"}]
  containerStatuses:
  - {name: app, ready: true, restartCount: 0, state: {running: {startedAt: "2026-11-07T11:00:00Z"}}}`)
	if ports != "" {
		var cps []any
		for _, port := range []int64{80, 8080} {
			if ports == "80" && port != 80 || ports == "8080" && port != 8080 {
				continue
			}
			cps = append(cps, map[string]any{"containerPort": port, "name": map[int64]string{80: "http", 8080: "alt"}[port]})
		}
		c := p.Object["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		c["ports"] = cps
	}
	return p
}

func TestServiceNoSelector(t *testing.T) {
	s := snapOf(t, "services", svcFix(t, "  type: ClusterIP\n  ports: [{port: 80}]"))
	s.Slices, s.SlicesRead = []unstructured.Unstructured{slice(t, 0, 0)}, true
	fs := ranked(s)
	if got := find(fs, "service-no-selector"); got == nil || got.Severity != sevInfo || got.Basis != basisObserved {
		t.Errorf("service-no-selector = %+v", got)
	}
	if got := find(fs, "service-no-ready-endpoints"); got == nil || got.Severity != sevCritical {
		t.Errorf("a selectorless service with no endpoints is unreachable: %v", ruleSet(fs))
	}
	s.Slices = []unstructured.Unstructured{slice(t, 2, 0)}
	if fs := ranked(s); find(fs, "service-no-ready-endpoints") != nil {
		t.Errorf("manual endpoints that are ready: %v", ruleSet(fs))
	}
}

func TestServiceNoPodsAndSelectorMismatch(t *testing.T) {
	s := snapOf(t, "services", svcFix(t, "  selector: {app: web, tier: front}\n  ports: [{port: 80}]"))
	s.Slices, s.SlicesRead = []unstructured.Unstructured{slice(t, 0, 0)}, true
	near := readyPod(t, "web-7", "")
	near.SetLabels(map[string]string{"app": "webapp", "tier": "front"})
	missing := readyPod(t, "web-6", "")
	missing.SetLabels(map[string]string{"app": "web"})
	far := readyPod(t, "other", "")
	far.SetLabels(map[string]string{"app": "other"})
	unrelated := readyPod(t, "cache", "")
	unrelated.SetLabels(map[string]string{"role": "cache"})
	s.NSPods = []unstructured.Unstructured{far, near, missing, unrelated}
	fs := ranked(s)
	np := find(fs, "service-no-pods")
	if np == nil || np.Severity != sevCritical || np.Basis != basisObserved || !hasEvidence(np, "selector app=web,tier=front matches 0 pods in namespace default") {
		t.Fatalf("service-no-pods = %+v", np)
	}
	mm := find(fs, "service-selector-mismatch")
	if mm == nil || mm.Basis != basisInferred || mm.Severity != sevCritical {
		t.Fatalf("service-selector-mismatch = %+v", mm)
	}
	if len(mm.Evidence) < 3 || mm.Evidence[0] != "pod web-6 has missing tier (selector wants tier=front)" {
		t.Errorf("closest pod first: %v", mm.Evidence)
	}
	if !hasEvidence(mm, "pod web-7 has app=webapp (selector wants app=web)") {
		t.Errorf("wrong selector values are still reported: %v", mm.Evidence)
	}
	for _, e := range mm.Evidence {
		if e == "pod cache" || hasEvidence(mm, "cache") {
			t.Errorf("pods sharing no selector key are not near misses: %v", mm.Evidence)
		}
	}
	if fs[0].Rule != "service-no-pods" {
		t.Errorf("top = %s", fs[0].Rule)
	}
}

func TestServiceNoReadyAndPartialEndpoints(t *testing.T) {
	notReady := podFix(t, "web-1", `
  phase: Running
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - {name: app, ready: false, state: {running: {startedAt: "2026-11-07T11:00:00Z"}}}`)
	notReady.SetLabels(map[string]string{"app": "web"})
	s := snapOf(t, "services", svcFix(t, "  selector: {app: web}\n  ports: [{port: 80}]"), notReady)
	s.Slices, s.SlicesRead = []unstructured.Unstructured{slice(t, 0, 1)}, true
	fs := ranked(s)
	nr := find(fs, "service-no-ready-endpoints")
	if nr == nil || nr.Severity != sevCritical || !hasEvidence(nr, "1 pod(s) match the selector; 0 ready endpoint(s) of 1") {
		t.Fatalf("service-no-ready-endpoints = %+v", nr)
	}
	if find(fs, "not-ready") == nil {
		t.Errorf("the pod's own finding is included: %v", ruleSet(fs))
	}
	s.Slices = []unstructured.Unstructured{slice(t, 1, 2)}
	pe := find(ranked(s), "service-partial-endpoints")
	if pe == nil || pe.Severity != sevWarning || !hasEvidence(pe, "1 ready endpoint(s) of 3") {
		t.Fatalf("service-partial-endpoints = %+v", pe)
	}
	// when slices cannot be read the pods' readiness is the evidence
	s.Slices, s.SlicesRead = nil, false
	if f := find(ranked(s), "service-no-ready-endpoints"); f == nil || !hasEvidence(f, "0 of 1 matching pod(s) are Ready") {
		t.Errorf("fallback = %+v", f)
	}
}

func TestServicePortMismatch(t *testing.T) {
	p := readyPod(t, "web-1", "80")
	p.SetLabels(map[string]string{"app": "web"})
	mk := func(port string) *snapshot {
		s := snapOf(t, "services", svcFix(t, "  selector: {app: web}\n  ports: [{port: 80, targetPort: "+port+"}]"), p)
		s.Slices, s.SlicesRead = []unstructured.Unstructured{slice(t, 1, 0)}, true
		return s
	}
	got := find(ranked(mk("9090")), "service-port-mismatch")
	if got == nil || got.Basis != basisInferred || got.Severity != sevWarning || !hasEvidence(got, "targetPort 9090") || !hasEvidence(got, "containerPort 80") {
		t.Fatalf("service-port-mismatch = %+v", got)
	}
	if find(ranked(mk("80")), "service-port-mismatch") != nil || find(ranked(mk("http")), "service-port-mismatch") != nil {
		t.Errorf("a declared number or name matches")
	}
	if find(ranked(mk("nope")), "service-port-mismatch") == nil {
		t.Errorf("an undeclared named port cannot resolve")
	}
	undeclared := readyPod(t, "web-2", "")
	undeclared.SetLabels(map[string]string{"app": "web"})
	s := snapOf(t, "services", svcFix(t, "  selector: {app: web}\n  ports: [{port: 80, targetPort: 9090}]"), undeclared)
	s.Slices, s.SlicesRead = []unstructured.Unstructured{slice(t, 1, 0)}, true
	if find(ranked(s), "service-port-mismatch") != nil {
		t.Errorf("pods that declare no ports prove nothing about a numeric targetPort")
	}
}

func TestServiceLoadBalancerPendingAndHealthy(t *testing.T) {
	p := readyPod(t, "web-1", "")
	p.SetLabels(map[string]string{"app": "web"})
	s := snapOf(t, "services", svcFix(t, "  type: LoadBalancer\n  selector: {app: web}\n  ports: [{port: 80}]"), p)
	s.Slices, s.SlicesRead = []unstructured.Unstructured{slice(t, 1, 0)}, true
	lb := find(ranked(s), "service-lb-pending")
	if lb == nil || lb.Severity != sevWarning || lb.Basis != basisObserved {
		t.Fatalf("service-lb-pending = %+v", lb)
	}
	healthy := snapOf(t, "services", svcFix(t, "  selector: {app: web}\n  ports: [{port: 80}]"), p)
	healthy.Slices, healthy.SlicesRead = []unstructured.Unstructured{slice(t, 1, 0)}, true
	if fs := ranked(healthy); len(fs) != 0 {
		t.Errorf("findings = %v", ruleSet(fs))
	}
	ext := snapOf(t, "services", svcFix(t, "  type: ExternalName\n  externalName: example.com"))
	if fs := ranked(ext); len(fs) != 0 {
		t.Errorf("ExternalName has no pods to check: %v", ruleSet(fs))
	}
}
