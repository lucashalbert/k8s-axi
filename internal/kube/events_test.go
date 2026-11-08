package kube

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lucashalbert/axi-sdk-go/redact"
)

func event(ns, name, typ, reason, kind, obj string, ago time.Duration) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: created(ago)},
		Type:           typ,
		Reason:         reason,
		Message:        reason + " message",
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: obj, Namespace: ns},
		LastTimestamp:  created(ago),
		FirstTimestamp: created(ago),
		Count:          1,
	}
}

func eventsFixture(t *testing.T, extra ...runtime.Object) *Clients {
	t.Helper()
	freezeTime(t)
	objs := []runtime.Object{
		event("default", "e1", "Normal", "Scheduled", "Pod", "a", 50*time.Minute),
		event("default", "e2", "Warning", "BackOff", "Pod", "b", 2*time.Minute),
		event("default", "e3", "Normal", "Pulled", "Pod", "b", 10*time.Minute),
		event("default", "e4", "Warning", "FailedMount", "Pod", "a", 20*time.Minute),
		event("other", "e5", "Normal", "Started", "Pod", "c", 5*time.Minute),
		event("default", "e6", "Normal", "NodeReady", "Node", "n1", 30*time.Minute),
	}
	c, _ := readFixture(t, append(objs, extra...))
	return c
}

func reasons(m map[string]any) []string {
	var out []string
	for _, e := range m["events"].([]any) {
		out = append(out, e.(map[string]any)["reason"].(string))
	}
	return out
}

func TestEventsNamespaceNewestFirstWithWarnings(t *testing.T) {
	c := eventsFixture(t)
	m := runJSON(t, c, "events")
	if got := strings.Join(reasons(m), ","); got != "BackOff,Pulled,FailedMount,NodeReady,Scheduled" {
		t.Errorf("order = %s", got)
	}
	if m["scope"] != "namespace/default" || m["count"] != float64(5) || m["total"] != float64(5) || m["warnings"] != float64(2) || m["truncated"] != false {
		t.Errorf("envelope: %v", m)
	}
	row := m["events"].([]any)[0].(map[string]any)
	if row["age"] != "2m" || row["type"] != "Warning" || row["object"] != "Pod/b" || row["count"] != float64(1) || row["message"] != "BackOff message" {
		t.Errorf("row: %v", row)
	}
	if _, ok := row["namespace"]; ok {
		t.Errorf("namespace column without -A: %v", row)
	}
	next := m["next"].([]any)[0].(string)
	if !strings.Contains(next, "k8s-axi inspect pod b -n default") {
		t.Errorf("next = %v", m["next"])
	}
}

func TestEventsTOONIsATable(t *testing.T) {
	c := eventsFixture(t)
	code, out, errOut := run(t, c, "events")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"scope: namespace/default", "warnings: 2", "events[5]{age,type,reason,object,count,message}:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestEventsForAnObject(t *testing.T) {
	c := eventsFixture(t)
	for _, args := range [][]string{{"events", "pod", "b"}, {"events", "pod/b"}} {
		m := runJSON(t, c, args...)
		if got := strings.Join(reasons(m), ","); got != "BackOff,Pulled" || m["scope"] != "pod/b in default" {
			t.Errorf("%v: scope=%v events=%s", args, m["scope"], got)
		}
	}
}

func TestEventsForAnObjectMatchByUIDWhenItExists(t *testing.T) {
	p := runningPod("default", "a", nil)
	p.UID = types.UID("uid-new")
	old := event("default", "old", "Warning", "OldIncarnation", "Pod", "a", 40*time.Minute)
	old.InvolvedObject.UID = "uid-old"
	cur := event("default", "cur", "Normal", "Created", "Pod", "a", 5*time.Minute)
	cur.InvolvedObject.UID = "uid-new"
	c := eventsFixture(t, p, old, cur)
	m := runJSON(t, c, "events", "pod/a")
	if got := strings.Join(reasons(m), ","); got != "Created" {
		t.Errorf("events = %s, want only the current incarnation", got)
	}
}

func TestEventsForADeletedObjectFallBackToKindAndName(t *testing.T) {
	c := eventsFixture(t)
	m := runJSON(t, c, "events", "pod/b") // pod b is not in the fixture's objects
	if m["count"] != float64(2) {
		t.Errorf("envelope: %v", m)
	}
}

func TestEventsForClusterScopedObjectSearchAllNamespaces(t *testing.T) {
	c := eventsFixture(t)
	m := runJSON(t, c, "events", "node/n1")
	if got := strings.Join(reasons(m), ","); got != "NodeReady" {
		t.Errorf("events = %s", got)
	}
	m = runJSON(t, c, "events", "node/n1", "-n", "default", "-A")
	if got := strings.Join(reasons(m), ","); got != "NodeReady" || m["scope"] != "node/n1" {
		t.Errorf("events with ignored namespace flags: scope=%v events=%s", m["scope"], got)
	}
}

func TestEventsFilters(t *testing.T) {
	c := eventsFixture(t)
	if got := strings.Join(reasons(runJSON(t, c, "events", "--type", "Warning")), ","); got != "BackOff,FailedMount" {
		t.Errorf("--type Warning = %s", got)
	}
	if got := strings.Join(reasons(runJSON(t, c, "events", "--since", "15m")), ","); got != "BackOff,Pulled" {
		t.Errorf("--since 15m = %s", got)
	}
	if code, _, _ := run(t, c, "events", "--type", "Bogus"); code != 2 {
		t.Errorf("bad --type exit %d, want 2", code)
	}
	if code, _, _ := run(t, c, "events", "--since", "soon"); code != 2 {
		t.Errorf("bad --since exit %d, want 2", code)
	}
}

func TestEventsAllNamespaces(t *testing.T) {
	c := eventsFixture(t)
	m := runJSON(t, c, "events", "-A")
	if m["scope"] != "all namespaces" || m["count"] != float64(6) {
		t.Fatalf("envelope: %v", m)
	}
	row := m["events"].([]any)[1].(map[string]any) // second newest is other/Started (5m)
	if row["namespace"] != "other" {
		t.Errorf("row: %v", row)
	}
}

func TestEventsAreBoundedAndSayWhenCut(t *testing.T) {
	c := eventsFixture(t)
	m := runJSON(t, c, "events", "--limit", "2")
	if m["count"] != float64(2) || m["total"] != float64(5) || m["truncated"] != true {
		t.Fatalf("envelope: %v", m)
	}
	if note, _ := m["note"].(string); !strings.Contains(note, "showing 2 of 5") || !strings.Contains(note, "--limit 0") {
		t.Errorf("note = %q", note)
	}
}

func TestEventsSayWhenTheScanWasCut(t *testing.T) {
	c := eventsFixture(t)
	c.Typed.(interface {
		PrependReactor(verb, resource string, reaction clienttesting.ReactionFunc)
	}).PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.EventList{ListMeta: metav1.ListMeta{Continue: "more"}, Items: []corev1.Event{*event("default", "x", "Normal", "R", "Pod", "a", time.Minute)}}, nil
	})
	m := runJSON(t, c, "events")
	if m["truncated"] != true {
		t.Fatalf("envelope: %v", m)
	}
	if note, _ := m["note"].(string); !strings.Contains(note, "newest events may be missing") {
		t.Errorf("note = %q", note)
	}
}

func TestEventsStateEmptyResult(t *testing.T) {
	c := eventsFixture(t)
	m := runJSON(t, c, "events", "-n", "quiet")
	if m["count"] != float64(0) || m["total"] != float64(0) || m["truncated"] != false {
		t.Fatalf("envelope: %v", m)
	}
	if note, _ := m["note"].(string); !strings.Contains(note, "no events in namespace/quiet") || !strings.Contains(note, "about an hour") {
		t.Errorf("note = %q", note)
	}
}

func TestEventsMessageIsCutInCompactAndWholeInFull(t *testing.T) {
	long := event("default", "long", "Warning", "Long", "Pod", "a", time.Minute)
	long.Message = strings.Repeat("x", 300) + "\nsecond line"
	c := eventsFixture(t, long)
	msg := func(args ...string) string {
		m := runJSON(t, c, args...)
		return m["events"].([]any)[0].(map[string]any)["message"].(string)
	}
	if got := msg("events"); len([]rune(got)) != 200 || !strings.HasSuffix(got, "…") || strings.Contains(got, "\n") {
		t.Errorf("compact message = %q", got)
	}
	if got := msg("events", "--full"); !strings.Contains(got, "second line") {
		t.Errorf("full message = %q", got)
	}
	m := runJSON(t, c, "events", "--full")
	row := m["events"].([]any)[0].(map[string]any)
	for _, k := range []string{"first_seen", "last_seen", "component", "uid"} {
		if _, ok := row[k]; !ok {
			t.Errorf("--full row lacks %s: %v", k, row)
		}
	}
}

func TestEventsFullFirstSeenFallsBackToEventTime(t *testing.T) {
	ev := event("default", "eventtime", "Warning", "EventTimeOnly", "Pod", "a", time.Hour)
	ev.FirstTimestamp = metav1.Time{}
	ev.LastTimestamp = metav1.Time{}
	ev.EventTime = metav1.MicroTime{Time: fixedNow.Add(-3 * time.Minute)}
	c := eventsFixture(t, ev)
	m := runJSON(t, c, "events", "pod/a", "--full", "--type", "Warning", "--since", "5m")
	row := m["events"].([]any)[0].(map[string]any)
	if row["first_seen"] != "2026-11-07T11:57:00Z" || row["last_seen"] != "2026-11-07T11:57:00Z" {
		t.Errorf("eventTime fallback row: %v", row)
	}
}

func TestEventsMessagesAreRedacted(t *testing.T) {
	leak := "sentinelTok123abc"
	secret := event("default", "secret", "Warning", "Secret", "Pod", "a", time.Minute)
	secret.Message = "first token=" + leak + "\nsecond"
	c := eventsFixture(t, secret)
	for _, args := range [][]string{{"events"}, {"events", "--full"}} {
		m := runJSON(t, c, args...)
		msg := m["events"].([]any)[0].(map[string]any)["message"].(string)
		if strings.Contains(msg, leak) || !strings.Contains(msg, redact.Placeholder) {
			t.Errorf("%v message leaked or was not redacted: %q", args, msg)
		}
		if strings.Contains(msg, "\n") || !strings.Contains(msg, "second") {
			t.Errorf("%v message was not normalized: %q", args, msg)
		}
	}
}

func TestEventsUsageErrors(t *testing.T) {
	c := eventsFixture(t)
	for _, args := range [][]string{{"events", "pods"}, {"events", "a", "b", "c"}, {"events", "frobnicators/x"}, {"events", "-n", "x", "-A"}} {
		if code, _, _ := run(t, c, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
