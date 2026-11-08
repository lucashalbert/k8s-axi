package kube

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lucashalbert/axi-sdk-go/redact"
)

// Severity and basis values of a finding.
const (
	sevCritical = "critical"
	sevWarning  = "warning"
	sevInfo     = "info"

	basisObserved = "observed" // read directly from the cluster
	basisInferred = "inferred" // derived by a rule

	maxSubjects = 5 // subjects named on one merged finding
)

// finding is one diagnosis. Group separates findings of one rule that must
// not merge (for example two containers); findings with the same rule and
// group merge into one that names up to maxSubjects subjects.
type finding struct {
	Rank     int      `json:"rank"`
	Severity string   `json:"severity"`
	Basis    string   `json:"basis"`
	Rule     string   `json:"rule"`
	Subject  string   `json:"subject"`
	Count    int      `json:"count"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`

	Group string `json:"-"`
}

// ruleOrder breaks ties between findings of equal severity and basis: causes
// come before the symptoms they explain.
var ruleOrder = []string{
	"quota-exceeded", "replica-failure", "node-unhealthy",
	"image-pull", "image-pull-cause", "container-error", "init-failure",
	"unschedulable", "unschedulable-cause", "startup-failed", "evicted", "evicted-cause",
	"oom-killed", "oom-cause", "crash-loop", "container-failed", "restarts", "probe-failed", "not-ready", "terminating-stuck",
	"rollout-stalled", "rollout-blocked", "rollout-progressing", "replicas-unavailable", "daemonset-unavailable", "paused", "scaled-to-zero",
	"job-failed", "job-retrying", "job-suspended",
	"service-no-pods", "service-selector-mismatch", "service-no-ready-endpoints", "service-partial-endpoints", "service-port-mismatch", "service-lb-pending", "service-no-selector",
}

func ruleIndex(rule string) int {
	for i, r := range ruleOrder {
		if r == rule {
			return i
		}
	}
	return len(ruleOrder)
}

func sevRank(s string) int {
	switch s {
	case sevCritical:
		return 0
	case sevWarning:
		return 1
	}
	return 2
}

func basisRank(b string) int {
	if b == basisObserved {
		return 0
	}
	return 1
}

// rankFindings merges findings of one rule and group, sorts them
// deterministically (severity, observed before inferred, rule order,
// subject) and numbers them from 1.
func rankFindings(in []finding) []finding {
	sorted := append([]finding(nil), in...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Subject < sorted[j].Subject })

	type key struct{ rule, group, sev, basis string }
	merged := map[key]*finding{}
	subjects := map[key][]string{}
	var keys []key
	for _, x := range sorted {
		k := key{x.Rule, x.Group, x.Severity, x.Basis}
		if _, ok := merged[k]; !ok {
			c := x
			c.Evidence = append([]string(nil), x.Evidence...)
			merged[k] = &c
			keys = append(keys, k)
		}
		subjects[k] = append(subjects[k], x.Subject)
	}
	out := make([]finding, 0, len(keys))
	for _, k := range keys {
		m := *merged[k]
		m.Count = len(subjects[k])
		m.Subject = joinSubjects(subjects[k])
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case sevRank(a.Severity) != sevRank(b.Severity):
			return sevRank(a.Severity) < sevRank(b.Severity)
		case basisRank(a.Basis) != basisRank(b.Basis):
			return basisRank(a.Basis) < basisRank(b.Basis)
		case ruleIndex(a.Rule) != ruleIndex(b.Rule):
			return ruleIndex(a.Rule) < ruleIndex(b.Rule)
		case a.Group != b.Group:
			return a.Group < b.Group
		}
		return a.Subject < b.Subject
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func joinSubjects(s []string) string {
	if len(s) <= maxSubjects {
		return strings.Join(s, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(s[:maxSubjects], ", "), len(s)-maxSubjects)
}

// statusOf is unhealthy with any critical finding, degraded with a warning,
// else healthy.
func statusOf(fs []finding) string {
	status := "healthy"
	for _, x := range fs {
		switch x.Severity {
		case sevCritical:
			return "unhealthy"
		case sevWarning:
			status = "degraded"
		}
	}
	return status
}

// conclusionOf is one sentence built from the top finding.
func conclusionOf(fs []finding) string {
	if len(fs) == 0 {
		return "no problems found in the checked evidence"
	}
	c := fmt.Sprintf("%s (%s)", fs[0].Summary, fs[0].Subject)
	switch n := len(fs) - 1; {
	case n == 1:
		c += "; 1 more finding"
	case n > 1:
		c += fmt.Sprintf("; %d more findings", n)
	}
	return c
}

// snapshot is everything the rules read: the target and the objects gathered
// around it. Rules do no I/O, so a snapshot built from fixtures is a test.
type snapshot struct {
	Resource    Resource
	Target      *unstructured.Unstructured
	Pods        []unstructured.Unstructured // the target's pods; a Pod target is itself
	ReplicaSets []unstructured.Unstructured // Deployment only
	Events      []corev1.Event              // warnings about the target, its pods and replica sets
	Nodes       map[string]*unstructured.Unstructured
	Quotas      []unstructured.Unstructured
	Slices      []unstructured.Unstructured // Service only: its EndpointSlices
	SlicesRead  bool                        // false when the slices could not be read
	NSPods      []unstructured.Unstructured // Service only: pods of the namespace
	Full        bool
}

// text makes a message from the cluster safe for evidence: masked for
// credentials, on one line, and cut unless --full.
func (s *snapshot) text(m string) string {
	m = strings.Join(strings.Fields(redact.Mask(m)), " ")
	if !s.Full {
		m = truncate(m, messageWidth)
	}
	return m
}

// run applies every rule that fits the target's kind.
func (s *snapshot) run() []finding {
	out := s.podRules()
	switch s.Resource.GVR.Resource {
	case "deployments":
		out = append(out, s.deploymentRules()...)
	case "statefulsets":
		out = append(out, s.statefulSetRules()...)
	case "daemonsets":
		out = append(out, s.daemonSetRules()...)
	case "jobs":
		out = append(out, s.jobRules()...)
	case "services":
		out = append(out, s.serviceRules()...)
	}
	if s.Resource.GVR.Resource != "pods" && s.Resource.GVR.Resource != "services" {
		out = append(out, s.controllerEventRules()...)
		out = append(out, s.quotaRules()...)
	}
	return append(out, s.nodeRules()...)
}

// eventsAbout are the events whose involved object has the given kind and name.
func (s *snapshot) eventsAbout(kind, name string) []corev1.Event {
	var out []corev1.Event
	for _, e := range s.Events {
		if e.InvolvedObject.Kind == kind && e.InvolvedObject.Name == name {
			out = append(out, e)
		}
	}
	return out
}
