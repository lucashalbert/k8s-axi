package kube

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func f(rule, sev, basis, subject string) finding {
	return finding{Rule: rule, Severity: sev, Basis: basis, Subject: subject, Summary: rule + " summary", Evidence: []string{"e-" + subject}}
}

func TestRankOrdersBySeverityBasisRuleSubject(t *testing.T) {
	in := []finding{
		f("probe-failed", sevWarning, basisObserved, "pod/b"),
		f("image-pull-cause", sevCritical, basisInferred, "pod/a"),
		f("image-pull", sevCritical, basisObserved, "pod/z"),
		f("scaled-to-zero", sevInfo, basisObserved, "deployment/x"),
		f("crash-loop", sevCritical, basisObserved, "pod/m"),
		f("probe-failed", sevWarning, basisObserved, "pod/a"),
	}
	got := rankFindings(in)
	var order []string
	for _, x := range got {
		order = append(order, x.Rule+"|"+x.Subject)
	}
	// merged: the two probe-failed findings share a rule and group, so they become one
	if len(got) != 5 {
		t.Fatalf("want 5 findings after merging, got %d: %v", len(got), order)
	}
	if got[0].Rule != "image-pull" || got[1].Rule != "crash-loop" || got[2].Rule != "image-pull-cause" || got[3].Rule != "probe-failed" || got[4].Rule != "scaled-to-zero" {
		t.Errorf("order = %v", order)
	}
	for i, x := range got {
		if x.Rank != i+1 {
			t.Errorf("rank[%d] = %d", i, x.Rank)
		}
	}
}

func TestRankIsDeterministicUnderShuffle(t *testing.T) {
	var base []finding
	for i := 0; i < 20; i++ {
		base = append(base, f(ruleOrder[i%len(ruleOrder)], []string{sevCritical, sevWarning, sevInfo}[i%3], []string{basisObserved, basisInferred}[i%2], fmt.Sprintf("pod/p%02d", i)))
	}
	want := rankFindings(append([]finding(nil), base...))
	for seed := int64(0); seed < 20; seed++ {
		in := append([]finding(nil), base...)
		rand.New(rand.NewSource(seed)).Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
		if got := rankFindings(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: order differs", seed)
		}
	}
}

func TestMergeSameRuleAndGroup(t *testing.T) {
	var in []finding
	for _, n := range []string{"pod/e", "pod/a", "pod/c", "pod/b", "pod/d", "pod/f", "pod/g"} {
		x := f("image-pull", sevCritical, basisObserved, n)
		x.Group = "app"
		in = append(in, x)
	}
	other := f("image-pull", sevCritical, basisObserved, "pod/h")
	other.Group = "sidecar"
	got := rankFindings(append(in, other))
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	m := got[0]
	if m.Count != 7 || m.Subject != "pod/a, pod/b, pod/c, pod/d, pod/e (+2 more)" {
		t.Errorf("merged = %d %q", m.Count, m.Subject)
	}
	if got[1].Count != 1 || got[1].Subject != "pod/h" {
		t.Errorf("single = %d %q", got[1].Count, got[1].Subject)
	}
	if len(m.Evidence) != 1 || m.Evidence[0] != "e-pod/a" {
		t.Errorf("evidence = %v (the first subject by name represents the group)", m.Evidence)
	}
}

func TestEveryRuleHasAnOrder(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range ruleOrder {
		if seen[r] {
			t.Errorf("duplicate rule %s", r)
		}
		seen[r] = true
	}
}

func TestStatusAndConclusion(t *testing.T) {
	cases := []struct {
		name     string
		in       []finding
		status   string
		contains string
	}{
		{"none", nil, "healthy", "no problems found"},
		{"info only", []finding{f("scaled-to-zero", sevInfo, basisObserved, "deployment/x")}, "healthy", "scaled-to-zero summary"},
		{"warning", []finding{f("probe-failed", sevWarning, basisObserved, "pod/a")}, "degraded", "probe-failed summary"},
		{"critical wins", []finding{f("probe-failed", sevWarning, basisObserved, "pod/a"), f("image-pull", sevCritical, basisObserved, "pod/b")}, "unhealthy", "image-pull summary"},
	}
	for _, c := range cases {
		got := rankFindings(c.in)
		if s := statusOf(got); s != c.status {
			t.Errorf("%s: status %s, want %s", c.name, s, c.status)
		}
		if con := conclusionOf(got); !strings.Contains(con, c.contains) {
			t.Errorf("%s: conclusion %q lacks %q", c.name, con, c.contains)
		}
	}
	two := rankFindings([]finding{f("image-pull", sevCritical, basisObserved, "pod/b"), f("probe-failed", sevWarning, basisObserved, "pod/a")})
	if con := conclusionOf(two); !strings.Contains(con, "1 more finding") {
		t.Errorf("conclusion %q should count the rest", con)
	}
}
