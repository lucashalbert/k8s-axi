package kube

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
)

const maxMismatchPods = 3

func (s *snapshot) serviceRules() []finding {
	o, subject := s.Target.Object, s.targetSubject()
	typ := str(o, "spec", "type")
	if typ == "ExternalName" {
		return nil
	}
	var out []finding
	if typ == "LoadBalancer" && len(list(o, "status", "loadBalancer", "ingress")) == 0 {
		out = append(out, newFinding("service-lb-pending", sevWarning, basisObserved, subject, "",
			"the load balancer has no address yet", "status.loadBalancer.ingress is empty; service created "+createdAge(s.Target)+" ago"))
	}
	ready, total, sliceInfo := s.endpointCounts()

	if len(dict(o, "spec", "selector")) == 0 {
		out = append(out, newFinding("service-no-selector", sevInfo, basisObserved, subject, "", "the service has no selector, so its endpoints are managed by hand", "spec.selector is empty"))
		if s.SlicesRead && ready == 0 {
			out = append(out, newFinding("service-no-ready-endpoints", sevCritical, basisObserved, subject, "", "the service has no ready endpoints", sliceInfo))
		}
		return out
	}

	sel := mapSelector(o)
	if len(s.Pods) == 0 {
		out = append(out, newFinding("service-no-pods", sevCritical, basisObserved, subject, "", "the service selects no pods",
			fmt.Sprintf("selector %s matches 0 pods in namespace %s", sel, s.Target.GetNamespace())))
		return append(out, s.selectorMismatch(subject)...)
	}

	podsReady := 0
	for i := range s.Pods {
		if podIsReady(s.Pods[i].Object) {
			podsReady++
		}
	}
	switch {
	case s.SlicesRead && ready == 0:
		out = append(out, newFinding("service-no-ready-endpoints", sevCritical, basisObserved, subject, "", "the service has pods but no ready endpoints",
			fmt.Sprintf("%d pod(s) match the selector; %d ready endpoint(s) of %d", len(s.Pods), ready, total)))
	case s.SlicesRead && ready < total:
		out = append(out, newFinding("service-partial-endpoints", sevWarning, basisObserved, subject, "", "some of the service's endpoints are not ready",
			fmt.Sprintf("%d ready endpoint(s) of %d", ready, total)))
	case !s.SlicesRead && podsReady == 0:
		out = append(out, newFinding("service-no-ready-endpoints", sevCritical, basisObserved, subject, "", "the service has pods but none are ready",
			fmt.Sprintf("0 of %d matching pod(s) are Ready (EndpointSlices could not be read)", len(s.Pods))))
	}
	return append(out, s.portMismatch(subject)...)
}

// endpointCounts counts ready and total endpoints over the service's
// EndpointSlices. An endpoint with no ready condition counts as ready, as the
// API defines.
func (s *snapshot) endpointCounts() (ready, total int, info string) {
	for i := range s.Slices {
		for _, e := range list(s.Slices[i].Object, "endpoints") {
			total++
			if r, ok := dict(asMap(e), "conditions")["ready"].(bool); !ok || r {
				ready++
			}
		}
	}
	return ready, total, fmt.Sprintf("%d ready endpoint(s) of %d in %d EndpointSlice(s)", ready, total, len(s.Slices))
}

// selectorMismatch names pods of the namespace that share a selector key with
// the service but differ in value or lack another key: the usual typo.
func (s *snapshot) selectorMismatch(subject string) []finding {
	sel := map[string]string{}
	for k, v := range dict(s.Target.Object, "spec", "selector") {
		sel[k], _ = v.(string)
	}
	type near struct {
		name    string
		matched int
		text    string
	}
	var cands []near
	for i := range s.NSPods {
		p := &s.NSPods[i]
		lbls := labels.Set(p.GetLabels())
		matched := 0
		var diffs []string
		for _, k := range sortedKeys(sel) {
			switch v, ok := lbls[k]; {
			case ok && v == sel[k]:
				matched++
			case ok:
				diffs = append(diffs, fmt.Sprintf("%s=%s (selector wants %s=%s)", k, v, k, sel[k]))
			}
		}
		if len(diffs) == 0 {
			continue // shares no key it differs on: unrelated, or a full match
		}
		cands = append(cands, near{p.GetName(), matched, fmt.Sprintf("pod %s has %s", p.GetName(), strings.Join(diffs, ", "))})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].matched != cands[j].matched {
			return cands[i].matched > cands[j].matched
		}
		return cands[i].name < cands[j].name
	})
	var ev []string
	for i := 0; i < len(cands) && i < maxMismatchPods; i++ {
		ev = append(ev, cands[i].text)
	}
	return []finding{newFinding("service-selector-mismatch", sevCritical, basisInferred, subject, "",
		"pods that look like the service's backends carry different labels than its selector", ev...)}
}

// portMismatch checks each service port's targetPort against the container
// ports the matching pods declare. Declaring ports is optional, so a number
// is only wrong when pods declare ports and none match; a name must exist.
func (s *snapshot) portMismatch(subject string) []finding {
	nums, names := map[int64]bool{}, map[string]bool{}
	for i := range s.Pods {
		for _, c := range list(s.Pods[i].Object, "spec", "containers") {
			for _, p := range list(asMap(c), "ports") {
				pm := asMap(p)
				nums[num(pm, "containerPort")] = true
				if n := str(pm, "name"); n != "" {
					names[n] = true
				}
			}
		}
	}
	var out []finding
	for _, p := range list(s.Target.Object, "spec", "ports") {
		pm := asMap(p)
		port := num(pm, "port")
		target := fmt.Sprint(pm["targetPort"])
		if pm["targetPort"] == nil {
			target = strconv.FormatInt(port, 10)
		}
		n, err := strconv.ParseInt(target, 10, 64)
		switch {
		case err != nil && !names[target]:
			out = append(out, newFinding("service-port-mismatch", sevWarning, basisInferred, subject, target,
				"a service port targets a named container port that the matching pods do not declare",
				fmt.Sprintf("service port %d -> targetPort %s; matching pods declare ports: %s", port, target, declared(nums, names))))
		case err == nil && len(nums) > 0 && !nums[n]:
			out = append(out, newFinding("service-port-mismatch", sevWarning, basisInferred, subject, target,
				"a service port targets a container port that the matching pods do not declare",
				fmt.Sprintf("service port %d -> targetPort %s; matching pods declare ports: %s", port, target, declared(nums, names))))
		}
	}
	return out
}

func declared(nums map[int64]bool, names map[string]bool) string {
	var parts []string
	var ns []int64
	for n := range nums {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i] < ns[j] })
	for _, n := range ns {
		parts = append(parts, fmt.Sprintf("containerPort %d", n))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
