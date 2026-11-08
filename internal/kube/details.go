package kube

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// Per-kind summaries shown by inspect. They are structs so the field order
// is stable, and they read only the scrubbed object (Secret and ConfigMap key
// sizes come from the original, which is read for lengths only).

type containerRow struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Ready    bool   `json:"ready"`
	State    string `json:"state"`
	Restarts int64  `json:"restarts"`
}

type podDetails struct {
	Status     string         `json:"status"`
	Node       string         `json:"node"`
	PodIP      string         `json:"pod_ip"`
	QoS        string         `json:"qos"`
	Containers []containerRow `json:"containers"`
}

type workloadDetails struct {
	Desired   int64  `json:"desired"`
	Ready     int64  `json:"ready"`
	Available int64  `json:"available"`
	Updated   int64  `json:"updated"`
	Strategy  string `json:"strategy,omitempty"`
	Selector  string `json:"selector"`
}

type jobDetails struct {
	Completions int64 `json:"completions"`
	Succeeded   int64 `json:"succeeded"`
	Failed      int64 `json:"failed"`
	Active      int64 `json:"active"`
}

type nodeDetails struct {
	Status      string   `json:"status"`
	Roles       string   `json:"roles"`
	Version     string   `json:"version"`
	OS          string   `json:"os,omitempty"`
	Capacity    string   `json:"capacity,omitempty"`
	Allocatable string   `json:"allocatable,omitempty"`
	Taints      []string `json:"taints"`
}

type serviceDetails struct {
	Type      string   `json:"type"`
	ClusterIP string   `json:"cluster_ip"`
	Ports     []string `json:"ports"`
	Selector  string   `json:"selector"`
}

type dataDetails struct {
	Type string    `json:"type,omitempty"`
	Keys []keySize `json:"keys"`
}

// genericDetails covers any kind without a dedicated summary: the scalar
// fields at the top of spec and status.
type genericDetails struct {
	Spec   map[string]any `json:"spec,omitempty"`
	Status map[string]any `json:"status,omitempty"`
	Note   string         `json:"note,omitempty"`
}

const maxGenericFields = 20

// detailsFor summarises obj (already scrubbed) for resource r. orig is the
// unscrubbed object, used only to measure Secret and ConfigMap values.
func detailsFor(r Resource, obj, orig map[string]any) any {
	g, res := r.GVR.Group, r.GVR.Resource
	switch {
	case g == "" && res == "pods":
		return podDetailsOf(obj)
	case g == "apps" && res == "daemonsets":
		return workloadDetails{num(obj, "status", "desiredNumberScheduled"), num(obj, "status", "numberReady"), num(obj, "status", "numberAvailable"),
			num(obj, "status", "updatedNumberScheduled"), str(obj, "spec", "updateStrategy", "type"), labelSelector(obj, "spec", "selector")}
	case g == "apps" && (res == "deployments" || res == "statefulsets" || res == "replicasets"):
		strategy := str(obj, "spec", "strategy", "type")
		if res == "statefulsets" {
			strategy = str(obj, "spec", "updateStrategy", "type")
		}
		return workloadDetails{replicas(obj), num(obj, "status", "readyReplicas"), num(obj, "status", "availableReplicas"),
			num(obj, "status", "updatedReplicas"), strategy, labelSelector(obj, "spec", "selector")}
	case g == "batch" && res == "jobs":
		return jobDetails{completions(obj), num(obj, "status", "succeeded"), num(obj, "status", "failed"), num(obj, "status", "active")}
	case g == "" && res == "nodes":
		return nodeDetails{nodeStatus(obj), nodeRoles(labelsOf(obj)), str(obj, "status", "nodeInfo", "kubeletVersion"), str(obj, "status", "nodeInfo", "osImage"),
			resourceList(obj, "status", "capacity"), resourceList(obj, "status", "allocatable"), taints(obj)}
	case g == "" && res == "services":
		ports := []string{}
		for _, p := range list(obj, "spec", "ports") {
			pm := asMap(p)
			s := fmt.Sprintf("%d/%s", num(pm, "port"), orDefault(str(pm, "protocol"), "TCP"))
			if n := str(pm, "name"); n != "" {
				s = n + ":" + s
			}
			ports = append(ports, s)
		}
		return serviceDetails{str(obj, "spec", "type"), str(obj, "spec", "clusterIP"), ports, mapSelector(obj)}
	case g == "" && res == "secrets":
		return dataDetails{str(obj, "type"), nonNil(dataKeys(orig))}
	case g == "" && res == "configmaps":
		return dataDetails{"", nonNil(dataKeys(orig))}
	}
	return genericDetailsOf(obj)
}

func podDetailsOf(o map[string]any) podDetails {
	statuses := map[string]map[string]any{}
	for _, c := range list(o, "status", "containerStatuses") {
		statuses[str(asMap(c), "name")] = asMap(c)
	}
	rows := []containerRow{}
	for _, c := range list(o, "spec", "containers") {
		cm := asMap(c)
		cs := statuses[str(cm, "name")]
		ready, _ := cs["ready"].(bool)
		rows = append(rows, containerRow{str(cm, "name"), str(cm, "image"), ready, containerState(cs), num(cs, "restartCount")})
	}
	return podDetails{podStatus(o), str(o, "spec", "nodeName"), str(o, "status", "podIP"), str(o, "status", "qosClass"), rows}
}

func containerState(cs map[string]any) string {
	st := dict(cs, "state")
	switch {
	case st["running"] != nil:
		return "running"
	case st["waiting"] != nil:
		return "waiting: " + orDefault(str(st, "waiting", "reason"), "unknown")
	case st["terminated"] != nil:
		t := asMap(st["terminated"])
		return fmt.Sprintf("terminated: %s (exit %d)", orDefault(str(t, "reason"), "unknown"), num(t, "exitCode"))
	}
	return "waiting"
}

func genericDetailsOf(o map[string]any) genericDetails {
	d := genericDetails{Spec: map[string]any{}, Status: map[string]any{}}
	left, skipped := maxGenericFields, 0
	for _, part := range []struct {
		name string
		dst  map[string]any
	}{{"spec", d.Spec}, {"status", d.Status}} {
		m := dict(o, part.name)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch m[k].(type) {
			case string, bool, int64, int32, int, float64:
				if left > 0 {
					part.dst[k] = m[k]
					left--
					continue
				}
			}
			skipped++
		}
	}
	if len(d.Spec) == 0 {
		d.Spec = nil
	}
	if len(d.Status) == 0 {
		d.Status = nil
	}
	if skipped > 0 {
		d.Note = fmt.Sprintf("%d nested or surplus fields omitted; use --full to see them", skipped)
	}
	return d
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// labelSelector renders the metav1.LabelSelector at path, or "" when absent.
func labelSelector(o map[string]any, path ...string) string {
	m := dict(o, path...)
	if m == nil {
		return ""
	}
	var ls metav1.LabelSelector
	if runtime.DefaultUnstructuredConverter.FromUnstructured(m, &ls) != nil {
		return ""
	}
	return metav1.FormatLabelSelector(&ls)
}

// mapSelector renders a Service's plain spec.selector map.
func mapSelector(o map[string]any) string {
	set := labels.Set{}
	for k, v := range dict(o, "spec", "selector") {
		set[k], _ = v.(string)
	}
	return set.String()
}

func resourceList(o map[string]any, path ...string) string {
	m := dict(o, path...)
	var out []string
	for _, k := range []string{"cpu", "memory", "pods"} {
		if v, ok := m[k].(string); ok {
			out = append(out, k+"="+v)
		}
	}
	return strings.Join(out, ",")
}

func taints(o map[string]any) []string {
	out := []string{}
	for _, t := range list(o, "spec", "taints") {
		tm := asMap(t)
		s := str(tm, "key")
		if v := str(tm, "value"); v != "" {
			s += "=" + v
		}
		out = append(out, s+":"+str(tm, "effect"))
	}
	return out
}

type conditionRow struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Age     string `json:"age"`
	Message string `json:"message"`
}

// conditions reads status.conditions, which most kinds share.
func conditions(o map[string]any, full bool) []conditionRow {
	rows := []conditionRow{}
	for _, c := range list(o, "status", "conditions") {
		cm := asMap(c)
		var since string
		if t, err := time.Parse(time.RFC3339, str(cm, "lastTransitionTime")); err == nil {
			since = age(t)
		}
		msg := strings.Join(strings.Fields(str(cm, "message")), " ")
		if !full {
			msg = truncate(msg, messageWidth)
		}
		rows = append(rows, conditionRow{str(cm, "type"), str(cm, "status"), str(cm, "reason"), since, msg})
	}
	return rows
}

func labelsOf(o map[string]any) map[string]string {
	m, _, _ := unstructured.NestedStringMap(o, "metadata", "labels")
	return m
}
