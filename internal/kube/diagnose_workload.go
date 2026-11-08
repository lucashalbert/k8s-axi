package kube

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// targetSubject is the finding subject for the target itself, for example
// "deployment/web".
func (s *snapshot) targetSubject() string {
	return strings.ToLower(s.Resource.Kind) + "/" + s.Target.GetName()
}

func (s *snapshot) condition(typ string) map[string]any {
	for _, c := range list(s.Target.Object, "status", "conditions") {
		if cm := asMap(c); str(cm, "type") == typ {
			return cm
		}
	}
	return nil
}

func (s *snapshot) conditionIs(typ, status string) bool {
	c := s.condition(typ)
	return c != nil && str(c, "status") == status
}

func (s *snapshot) zeroFinding(what string) finding {
	return newFinding("scaled-to-zero", sevInfo, basisObserved, s.targetSubject(), "", what, "desired replicas is 0")
}

func (s *snapshot) deploymentRules() []finding {
	o, subject := s.Target.Object, s.targetSubject()
	desired := replicas(o)
	if desired == 0 {
		return []finding{s.zeroFinding("the deployment is scaled to zero, so it runs no pods")}
	}
	ready, avail, updated := num(o, "status", "readyReplicas"), num(o, "status", "availableReplicas"), num(o, "status", "updatedReplicas")
	var out []finding
	stalled := false
	if c := s.condition("Progressing"); c != nil && str(c, "status") == "False" && str(c, "reason") == "ProgressDeadlineExceeded" {
		stalled = true
		out = append(out, newFinding("rollout-stalled", sevCritical, basisObserved, subject, "", "the rollout is stalled past its progress deadline",
			s.text("Progressing=False: ProgressDeadlineExceeded: "+str(c, "message"))))
	}
	if b, _ := o["spec"].(map[string]any)["paused"].(bool); b {
		out = append(out, newFinding("paused", sevWarning, basisObserved, subject, "", "the rollout is paused, so spec changes are not applied", "spec.paused is true"))
	}
	if avail < desired {
		sev := sevWarning
		if avail == 0 {
			sev = sevCritical
		}
		out = append(out, newFinding("replicas-unavailable", sev, basisObserved, subject, "", "fewer replicas are available than desired",
			fmt.Sprintf("desired %d, ready %d, available %d, updated %d", desired, ready, avail, updated)))
	}
	if !stalled {
		var ev []string
		if updated < desired {
			ev = append(ev, fmt.Sprintf("updated %d of %d", updated, desired))
		}
		if og, g := num(o, "status", "observedGeneration"), num(o, "metadata", "generation"); og < g {
			ev = append(ev, fmt.Sprintf("observedGeneration %d is behind generation %d", og, g))
		}
		if len(ev) > 0 {
			out = append(out, newFinding("rollout-progressing", sevInfo, basisObserved, subject, "", "a rollout is in progress", ev...))
		}
	}
	return out
}

func (s *snapshot) statefulSetRules() []finding {
	o, subject := s.Target.Object, s.targetSubject()
	desired := replicas(o)
	if desired == 0 {
		return []finding{s.zeroFinding("the statefulset is scaled to zero, so it runs no pods")}
	}
	ready, updated := num(o, "status", "readyReplicas"), num(o, "status", "updatedReplicas")
	var out []finding
	if ready < desired {
		sev := sevWarning
		if ready == 0 {
			sev = sevCritical
		}
		out = append(out, newFinding("replicas-unavailable", sev, basisObserved, subject, "", "fewer replicas are ready than desired",
			fmt.Sprintf("desired %d, ready %d, updated %d", desired, ready, updated)))
	}
	cur, upd := str(o, "status", "currentRevision"), str(o, "status", "updateRevision")
	if cur != "" && upd != "" && cur != upd && updated < desired && ready < desired {
		ev := []string{fmt.Sprintf("revision %s -> %s, updated %d of %d", cur, upd, updated, desired)}
		var unready []string
		for i := range s.Pods {
			if !podIsReady(s.Pods[i].Object) {
				unready = append(unready, s.Pods[i].GetName())
			}
		}
		if len(unready) > 0 {
			ev = append(ev, "pods not ready: "+strings.Join(unready, ", "))
		}
		out = append(out, newFinding("rollout-blocked", sevWarning, basisInferred, subject, "",
			"the ordered update waits for a pod that is not ready, so later pods are not replaced", ev...))
	}
	return out
}

func podIsReady(o map[string]any) bool {
	for _, c := range list(o, "status", "conditions") {
		if cm := asMap(c); str(cm, "type") == "Ready" {
			return str(cm, "status") == "True"
		}
	}
	return false
}

func (s *snapshot) daemonSetRules() []finding {
	o, subject := s.Target.Object, s.targetSubject()
	desired, ready := num(o, "status", "desiredNumberScheduled"), num(o, "status", "numberReady")
	unavail, mis := num(o, "status", "numberUnavailable"), num(o, "status", "numberMisscheduled")
	if desired == 0 && mis == 0 {
		return []finding{newFinding("scaled-to-zero", sevInfo, basisObserved, subject, "", "the daemonset is scheduled on no nodes",
			"desiredNumberScheduled is 0; no node matches its nodeSelector or tolerations")}
	}
	if ready >= desired && unavail == 0 && mis == 0 {
		return nil
	}
	return []finding{newFinding("daemonset-unavailable", sevWarning, basisObserved, subject, "", "the daemonset is not ready on every node",
		fmt.Sprintf("desired %d, ready %d, unavailable %d, misscheduled %d", desired, ready, unavail, mis))}
}

func (s *snapshot) jobRules() []finding {
	o, subject := s.Target.Object, s.targetSubject()
	failed, active := num(o, "status", "failed"), num(o, "status", "active")
	if b, _ := dict(o, "spec")["suspend"].(bool); b {
		return []finding{newFinding("job-suspended", sevInfo, basisObserved, subject, "", "the job is suspended and creates no pods", "spec.suspend is true")}
	}
	if c := s.condition("Failed"); c != nil && str(c, "status") == "True" {
		return []finding{newFinding("job-failed", sevCritical, basisObserved, subject, "", "the job has failed",
			s.text(fmt.Sprintf("Failed=True: %s: %s", str(c, "reason"), str(c, "message"))), fmt.Sprintf("%d failed pod(s)", failed))}
	}
	if s.conditionIs("Complete", "True") || failed == 0 {
		return nil
	}
	limit := int64(6)
	if _, ok, _ := unstructured.NestedFieldNoCopy(o, "spec", "backoffLimit"); ok {
		limit = num(o, "spec", "backoffLimit")
	}
	return []finding{newFinding("job-retrying", sevWarning, basisObserved, subject, "", "the job's pods are failing and being retried",
		fmt.Sprintf("%d failed pod(s) so far, backoffLimit %d, %d active", failed, limit, active))}
}

// missingReplicas reports whether the workload lacks pods it wants.
func (s *snapshot) missingReplicas() bool {
	o := s.Target.Object
	switch s.Resource.GVR.Resource {
	case "deployments":
		return num(o, "status", "availableReplicas") < replicas(o)
	case "statefulsets":
		return num(o, "status", "readyReplicas") < replicas(o)
	case "daemonsets":
		return num(o, "status", "numberReady") < num(o, "status", "desiredNumberScheduled")
	case "jobs":
		return num(o, "status", "active") == 0 && num(o, "status", "succeeded") < completions(o)
	}
	return false
}

// podQuotaKeys are the quota resources that stop pods from being created.
var podQuotaKeys = map[string]bool{
	"pods": true, "count/pods": true, "cpu": true, "memory": true, "ephemeral-storage": true,
	"requests.cpu": true, "requests.memory": true, "requests.ephemeral-storage": true,
	"limits.cpu": true, "limits.memory": true, "limits.ephemeral-storage": true,
}

// exhaustedQuotas lists "ResourceQuota NAME: KEY used/hard" for pod-related
// quota resources that are fully used.
func (s *snapshot) exhaustedQuotas() []string {
	var out []string
	for i := range s.Quotas {
		q := &s.Quotas[i]
		hard, used := dict(q.Object, "status", "hard"), dict(q.Object, "status", "used")
		for _, k := range sortedKeys(hard) {
			if !podQuotaKeys[k] {
				continue
			}
			h, errH := resource.ParseQuantity(fmt.Sprint(hard[k]))
			u, errU := resource.ParseQuantity(fmt.Sprint(used[k]))
			if errH != nil || errU != nil || h.IsZero() || u.Cmp(h) < 0 {
				continue
			}
			out = append(out, fmt.Sprintf("ResourceQuota %s: %s %s/%s", q.GetName(), k, u.String(), h.String()))
		}
	}
	return out
}

var controllerKinds = map[string]bool{"ReplicaSet": true, "StatefulSet": true, "DaemonSet": true, "Job": true, "Deployment": true}

// controllerEventRules reads the controllers' failures to create pods
// (FailedCreate events, or the ReplicaFailure condition when no event is
// left) and the quotas that explain them.
func (s *snapshot) controllerEventRules() []finding {
	exhausted := s.exhaustedQuotas()
	var out []finding
	seen := false
	emit := func(subject, msg, seenAt string) {
		seen = true
		ev := []string{s.text(msg)}
		if seenAt != "" {
			ev = append(ev, seenAt)
		}
		if strings.Contains(strings.ToLower(msg), "exceeded quota") {
			out = append(out, newFinding("quota-exceeded", sevCritical, basisObserved, subject, "", "pods cannot be created because a resource quota is exceeded", append(ev, exhausted...)...))
			return
		}
		out = append(out, newFinding("replica-failure", sevCritical, basisObserved, subject, "", "the controller cannot create pods", ev...))
	}
	for _, e := range s.Events {
		if e.Reason == "FailedCreate" && controllerKinds[e.InvolvedObject.Kind] {
			emit(strings.ToLower(e.InvolvedObject.Kind)+"/"+e.InvolvedObject.Name, e.Message, eventSeen(&e))
		}
	}
	if c := s.condition("ReplicaFailure"); !seen && c != nil && str(c, "status") == "True" {
		emit(s.targetSubject(), fmt.Sprintf("ReplicaFailure=True: %s: %s", str(c, "reason"), str(c, "message")), "")
	}
	if !seen && len(exhausted) > 0 && s.missingReplicas() {
		out = append(out, newFinding("quota-exceeded", sevCritical, basisInferred, s.targetSubject(), "",
			"a resource quota is fully used while the workload is missing pods", exhausted...))
	}
	return out
}

func (s *snapshot) serviceRules() []finding { return nil }
