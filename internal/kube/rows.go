package kube

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// now is replaceable so ages are testable.
var now = time.Now

// age renders the time since t as one unit: 45s, 5m, 3h or 12d.
func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now().Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func createdAge(u *unstructured.Unstructured) string {
	return age(u.GetCreationTimestamp().Time)
}

// field readers: absent or mistyped fields read as zero values.

func str(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

func num(obj map[string]any, path ...string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(obj, path...)
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func list(obj map[string]any, path ...string) []any {
	l, _, _ := unstructured.NestedSlice(obj, path...)
	return l
}

func dict(obj map[string]any, path ...string) map[string]any {
	m, _, _ := unstructured.NestedMap(obj, path...)
	return m
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// Row types. A list has one row type, so every row has the same columns and
// TOON prints it as a table. Namespace is set only for all-namespace lists.

type podRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Ready     string `json:"ready"`
	Status    string `json:"status"`
	Restarts  int64  `json:"restarts"`
	Node      string `json:"node"`
	Age       string `json:"age"`
}

type workloadRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Ready     string `json:"ready"`
	Age       string `json:"age"`
}

type jobRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Complete  string `json:"complete"`
	Age       string `json:"age"`
}

type nodeRow struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Roles   string `json:"roles"`
	Version string `json:"version"`
	Age     string `json:"age"`
}

type serviceRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	ClusterIP string `json:"cluster_ip"`
	Ports     string `json:"ports"`
	Age       string `json:"age"`
}

type configMapRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Keys      int    `json:"keys"`
	Age       string `json:"age"`
}

type secretRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Keys      int    `json:"keys"`
	Age       string `json:"age"`
}

type genericRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Age       string `json:"age"`
}

// rowFor builds the compact row for one object of resource r.
func rowFor(r Resource, u *unstructured.Unstructured, showNS bool) any {
	ns := ""
	if showNS {
		ns = u.GetNamespace()
	}
	name, a, o := u.GetName(), createdAge(u), u.Object
	switch {
	case r.GVR.Group == "" && r.GVR.Resource == "pods":
		return podRow{ns, name, podReady(o), podStatus(o), podRestarts(o), str(o, "spec", "nodeName"), a}
	case r.GVR.Group == "apps" && r.GVR.Resource == "daemonsets":
		return workloadRow{ns, name, fmt.Sprintf("%d/%d", num(o, "status", "numberReady"), num(o, "status", "desiredNumberScheduled")), a}
	case r.GVR.Group == "apps" && (r.GVR.Resource == "deployments" || r.GVR.Resource == "statefulsets" || r.GVR.Resource == "replicasets"):
		return workloadRow{ns, name, fmt.Sprintf("%d/%d", num(o, "status", "readyReplicas"), replicas(o)), a}
	case r.GVR.Group == "batch" && r.GVR.Resource == "jobs":
		return jobRow{ns, name, fmt.Sprintf("%d/%d", num(o, "status", "succeeded"), completions(o)), a}
	case r.GVR.Group == "" && r.GVR.Resource == "nodes":
		return nodeRow{name, nodeStatus(o), nodeRoles(u), str(o, "status", "nodeInfo", "kubeletVersion"), a}
	case r.GVR.Group == "" && r.GVR.Resource == "services":
		return serviceRow{ns, name, str(o, "spec", "type"), str(o, "spec", "clusterIP"), servicePorts(o), a}
	case r.GVR.Group == "" && r.GVR.Resource == "configmaps":
		return configMapRow{ns, name, len(dataKeys(o)), a}
	case r.GVR.Group == "" && r.GVR.Resource == "secrets":
		return secretRow{ns, name, str(o, "type"), len(dataKeys(o)), a}
	}
	return genericRow{ns, name, genericStatus(o), a}
}

// replicas is spec.replicas, which the API server defaults to 1.
func replicas(o map[string]any) int64 {
	if _, ok, _ := unstructured.NestedFieldNoCopy(o, "spec", "replicas"); !ok {
		return 1
	}
	return num(o, "spec", "replicas")
}

func completions(o map[string]any) int64 {
	if _, ok, _ := unstructured.NestedFieldNoCopy(o, "spec", "completions"); !ok {
		return 1
	}
	return num(o, "spec", "completions")
}

func podReady(o map[string]any) string {
	ready := 0
	for _, c := range list(o, "status", "containerStatuses") {
		if b, _ := asMap(c)["ready"].(bool); b {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(list(o, "spec", "containers")))
}

func podRestarts(o map[string]any) int64 {
	var n int64
	for _, c := range list(o, "status", "containerStatuses") {
		n += num(asMap(c), "restartCount")
	}
	return n
}

// podStatus follows the rules kubectl uses for its STATUS column: the phase,
// refined by init container progress and the reason a container waits or
// terminated, and Terminating once deletion started.
func podStatus(o map[string]any) string {
	reason := str(o, "status", "phase")
	if r := str(o, "status", "reason"); r != "" {
		reason = r
	}
	initializing := false
	inits := list(o, "spec", "initContainers")
	for i, c := range list(o, "status", "initContainerStatuses") {
		cs := asMap(c)
		term, wait := dict(cs, "state", "terminated"), dict(cs, "state", "waiting")
		switch {
		case term != nil && num(term, "exitCode") == 0:
			continue
		case term != nil:
			reason = "Init:" + exitReason(term)
		case wait != nil && str(wait, "reason") != "" && str(wait, "reason") != "PodInitializing":
			reason = "Init:" + str(wait, "reason")
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(inits))
		}
		initializing = true
		break
	}
	if !initializing {
		running := false
		statuses := list(o, "status", "containerStatuses")
		for i := len(statuses) - 1; i >= 0; i-- {
			cs := asMap(statuses[i])
			wait, term := dict(cs, "state", "waiting"), dict(cs, "state", "terminated")
			switch {
			case wait != nil && str(wait, "reason") != "":
				reason = str(wait, "reason")
			case term != nil:
				reason = exitReason(term)
			case dict(cs, "state", "running") != nil && cs["ready"] == true:
				running = true
			}
		}
		if reason == "Completed" && running {
			reason = "Running"
		}
	}
	if _, ok, _ := unstructured.NestedString(o, "metadata", "deletionTimestamp"); ok {
		if str(o, "status", "reason") == "NodeLost" {
			return "Unknown"
		}
		return "Terminating"
	}
	return reason
}

func exitReason(term map[string]any) string {
	if r := str(term, "reason"); r != "" {
		return r
	}
	if s := num(term, "signal"); s != 0 {
		return fmt.Sprintf("Signal:%d", s)
	}
	return fmt.Sprintf("ExitCode:%d", num(term, "exitCode"))
}

func nodeStatus(o map[string]any) string {
	s := "Unknown"
	for _, c := range list(o, "status", "conditions") {
		if cm := asMap(c); str(cm, "type") == "Ready" {
			switch str(cm, "status") {
			case "True":
				s = "Ready"
			case "False":
				s = "NotReady"
			}
		}
	}
	if b, _ := dict(o, "spec")["unschedulable"].(bool); b {
		s += ",SchedulingDisabled"
	}
	return s
}

func nodeRoles(u *unstructured.Unstructured) string {
	var roles []string
	for k := range u.GetLabels() {
		if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && r != "" {
			roles = append(roles, r)
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func servicePorts(o map[string]any) string {
	var ps []string
	for _, p := range list(o, "spec", "ports") {
		pm := asMap(p)
		proto := str(pm, "protocol")
		if proto == "" {
			proto = "TCP"
		}
		ps = append(ps, fmt.Sprintf("%d/%s", num(pm, "port"), proto))
	}
	if len(ps) == 0 {
		return "<none>"
	}
	return strings.Join(ps, ",")
}

// genericStatus is status.phase, else the first of the well-known condition
// types that is present, shown as the type when True and "Not<type>" when
// False. Empty when the kind reports neither.
func genericStatus(o map[string]any) string {
	if p := str(o, "status", "phase"); p != "" {
		return p
	}
	conds := list(o, "status", "conditions")
	for _, want := range []string{"Ready", "Available", "Established", "Succeeded", "Complete"} {
		for _, c := range conds {
			cm := asMap(c)
			if str(cm, "type") != want {
				continue
			}
			switch str(cm, "status") {
			case "True":
				return want
			case "False":
				return "Not" + want
			}
			return "Unknown"
		}
	}
	return ""
}
