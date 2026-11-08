package kube

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"context"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// stubLogs replaces the log stream with fn for one test and records the
// options each read used.
type logCall struct {
	pod  string
	opts corev1.PodLogOptions
}

func stubLogs(t *testing.T, fn func(pod, container string) (string, error)) *[]logCall {
	t.Helper()
	var calls []logCall
	old := podLog
	podLog = func(_ context.Context, _ *Clients, _ string, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
		calls = append(calls, logCall{pod, *o})
		s, err := fn(pod, o.Container)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(s)), nil
	}
	t.Cleanup(func() { podLog = old })
	return &calls
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func logLines(s map[string]any) []string {
	var out []string
	for _, l := range s["log"].([]any) {
		out = append(out, l.(map[string]any)["line"].(string))
	}
	return out
}

func twoContainerPod(name string, labels map[string]string) *corev1.Pod {
	p := runningPod("default", name, labels)
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "side:1"})
	p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "init:1"}}
	return p
}

func logsFixture(t *testing.T) *Clients {
	t.Helper()
	freezeTime(t)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mig"},
		Spec: batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"controller-uid": "abc"}}}}
	objs := []runtime.Object{
		dep, job,
		runningPod("default", "solo", nil),
		twoContainerPod("multi", nil),
		runningPod("default", "web-1", map[string]string{"app": "web"}),
		runningPod("default", "web-2", map[string]string{"app": "web"}),
		runningPod("default", "web-3", map[string]string{"app": "web"}),
		runningPod("default", "mig-x", map[string]string{"controller-uid": "abc"}),
		runningPod("other", "elsewhere", map[string]string{"app": "web"}),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cfg"}},
	}
	c, _ := readFixture(t, objs)
	return c
}

func TestLogsOnePod(t *testing.T) {
	c := logsFixture(t)
	calls := stubLogs(t, func(string, string) (string, error) { return "hello\nworld\n", nil })
	m := runJSON(t, c, "logs", "solo")
	if m["target"] != "pod/solo" || m["namespace"] != "default" || m["pods"] != float64(1) || m["truncated"] != false || m["redacted"] != float64(0) {
		t.Fatalf("envelope: %v", m)
	}
	s := m["streams"].([]any)[0].(map[string]any)
	if s["pod"] != "solo" || s["container"] != "app" || s["lines"] != float64(2) || strings.Join(logLines(s), "|") != "hello|world" {
		t.Errorf("stream: %v", s)
	}
	o := (*calls)[0].opts
	if *o.TailLines != 101 || o.Previous || o.SinceSeconds != nil || o.LimitBytes == nil {
		t.Errorf("options: %+v", o)
	}
}

func TestLogsTOONPrintsOneLinePerRow(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return "a, b\nc\n", nil })
	code, out, errOut := run(t, c, "logs", "pod/solo")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "log[2]{line}:") || !strings.Contains(out, `"a, b"`) {
		t.Errorf("TOON:\n%s", out)
	}
}

func TestLogsContainerSelection(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(_, container string) (string, error) { return "from " + container + "\n", nil })
	all := runJSON(t, c, "logs", "multi")["streams"].([]any)
	if len(all) != 2 || all[0].(map[string]any)["container"] != "app" || all[1].(map[string]any)["container"] != "sidecar" {
		t.Errorf("every regular container, init excluded: %v", all)
	}
	one := runJSON(t, c, "logs", "multi", "-c", "init")["streams"].([]any)
	if len(one) != 1 || logLines(one[0].(map[string]any))[0] != "from init" {
		t.Errorf("init container by name: %v", one)
	}
	code, _, errOut := run(t, c, "logs", "multi", "-c", "nope")
	if code != 2 || !strings.Contains(errOut, "app, sidecar, init") {
		t.Errorf("unknown container: exit %d: %s", code, errOut)
	}
}

func TestLogsWorkloadTargets(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(pod, _ string) (string, error) { return "from " + pod + "\n", nil })
	for _, args := range [][]string{{"logs", "deploy/web"}, {"logs", "deployment", "web"}, {"logs", "-l", "app=web"}} {
		m := runJSON(t, c, args...)
		streams := m["streams"].([]any)
		if m["pods"] != float64(3) || len(streams) != 3 || streams[0].(map[string]any)["pod"] != "web-1" || streams[2].(map[string]any)["pod"] != "web-3" {
			t.Errorf("%v: %v", args, m)
		}
	}
	if m := runJSON(t, c, "logs", "job/mig"); m["pods"] != float64(1) || m["target"] != "job/mig" {
		t.Errorf("job: %v", m)
	}
}

func TestLogsWorkloadInAnotherNamespaceIsNotFound(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return "x", nil })
	if code, _, _ := run(t, c, "logs", "deploy/web", "-n", "other"); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestLogsStatesWhenNoPodsMatch(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return "x", nil })
	m := runJSON(t, c, "logs", "-l", "app=none")
	if m["pods"] != float64(0) || !strings.Contains(m["note"].(string), "no pods found for selector app=none") || len(m["streams"].([]any)) != 0 {
		t.Errorf("envelope: %v", m)
	}
}

func TestLogsCapsThePodsRead(t *testing.T) {
	freezeTime(t)
	var objs []runtime.Object
	for i := 1; i <= 7; i++ {
		objs = append(objs, runningPod("default", fmt.Sprintf("p%d", i), map[string]string{"app": "many"}))
	}
	c, _ := readFixture(t, objs)
	stubLogs(t, func(string, string) (string, error) { return "x\n", nil })
	m := runJSON(t, c, "logs", "-l", "app=many")
	if m["pods"] != float64(7) || len(m["streams"].([]any)) != 5 || !strings.Contains(m["note"].(string), "showing 5 of 7 pods") {
		t.Errorf("envelope: %v", m)
	}
}

func TestLogsKeepsTheNewestLinesAndSaysSo(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return numbered(150), nil })
	m := runJSON(t, c, "logs", "solo", "--tail", "100")
	s := m["streams"].([]any)[0].(map[string]any)
	lines := logLines(s)
	if len(lines) != 100 || lines[0] != "line 51" || lines[99] != "line 150" || s["truncated"] != true || m["truncated"] != true {
		t.Errorf("stream: lines=%d first=%q last=%q truncated=%v", len(lines), lines[0], lines[len(lines)-1], s["truncated"])
	}
	if !strings.Contains(m["note"].(string), "older lines were left out") {
		t.Errorf("note = %q", m["note"])
	}
}

func TestLogsSmallLogIsNotTruncated(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return numbered(100), nil })
	m := runJSON(t, c, "logs", "solo", "--tail", "100")
	if m["truncated"] != false || len(logLines(m["streams"].([]any)[0].(map[string]any))) != 100 {
		t.Errorf("envelope: %v", m)
	}
}

func TestShapeLogByteBudget(t *testing.T) {
	long := strings.Repeat("x", 100)
	raw := strings.Repeat(long+"\n", 50)
	lines, truncated, _ := shapeLog(raw, 1000, 1024)
	if !truncated || len(lines) != 10 { // 10 lines of 101 bytes fit in 1024
		t.Errorf("lines=%d truncated=%v", len(lines), truncated)
	}
	lines, truncated, _ = shapeLog(strings.Repeat("y", 5000)+"\n", 1000, 1024)
	if !truncated || len(lines) != 1 || len(lines[0]) > 1024 || !strings.HasSuffix(lines[0], "…") {
		t.Errorf("one oversized line: %d lines, %d bytes", len(lines), len(lines[0]))
	}
	lines, truncated, _ = shapeLog(strings.Repeat("界", 2000)+"\n", 1000, 1024)
	if !truncated || len(lines) != 1 || len(lines[0]) > 1024 || !strings.HasSuffix(lines[0], "…") {
		t.Errorf("one oversized multibyte line: %d lines, %d bytes", len(lines), len(lines[0]))
	}
	if !utf8.ValidString(lines[0]) {
		t.Errorf("one oversized multibyte line is not valid UTF-8")
	}
	if lines, truncated, _ := shapeLog("", 10, 1024); len(lines) != 0 || truncated {
		t.Errorf("empty: %v %v", lines, truncated)
	}
	if lines, _, _ := shapeLog("a\r\nb\r\n", 10, 1024); strings.Join(lines, "|") != "a|b" {
		t.Errorf("CRLF: %q", lines)
	}
}

func TestLogsByteBudgetIsSharedAcrossStreams(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return strings.Repeat(strings.Repeat("z", 99)+"\n", 100), nil })
	m := runJSON(t, c, "logs", "multi", "--max-bytes", "2048")
	total := 0
	for _, s := range m["streams"].([]any) {
		total += len(strings.Join(logLines(s.(map[string]any)), "\n"))
	}
	if total > 2048 || m["truncated"] != true {
		t.Errorf("total log bytes %d, truncated=%v", total, m["truncated"])
	}
}

func TestLogsDropsStreamsWhenMinimumWouldExceedTotalBudget(t *testing.T) {
	freezeTime(t)
	var objs []runtime.Object
	for i := 1; i <= 5; i++ {
		objs = append(objs, runningPod("default", fmt.Sprintf("tight-%d", i), map[string]string{"app": "tight"}))
	}
	c, _ := readFixture(t, objs)
	calls := stubLogs(t, func(string, string) (string, error) { return "ok\n", nil })
	m := runJSON(t, c, "logs", "-l", "app=tight", "--max-bytes", "1024")
	if len(m["streams"].([]any)) != 1 || len(*calls) != 1 || m["truncated"] != true {
		t.Fatalf("budget was not enforced before opening streams: %v calls=%d", m, len(*calls))
	}
	note := m["note"].(string)
	if !strings.Contains(note, "4 stream(s) left unread") || strings.Contains(note, "older lines were left out") {
		t.Errorf("note = %q", note)
	}
}

func TestLogsRedactsCredentials(t *testing.T) {
	c := logsFixture(t)
	key := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAsecretkeymaterial\nmore+key+lines\n-----END RSA PRIVATE KEY-----"
	text := "starting\nAuthorization: Bearer abcdefghijklmnop12345\ntoken=plainlysecret99\n" + key + "\ndone\n"
	stubLogs(t, func(string, string) (string, error) { return text, nil })
	code, out, _ := run(t, c, "logs", "solo", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, leak := range []string{"abcdefghijklmnop12345", "plainlysecret99", "secretkeymaterial", "more+key+lines"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q:\n%s", leak, out)
		}
	}
	m := runJSON(t, c, "logs", "solo")
	if m["redacted"].(float64) < 3 || !strings.Contains(m["note"].(string), "redacted") {
		t.Errorf("redaction not reported: %v", m)
	}
	if got := logLines(m["streams"].([]any)[0].(map[string]any)); got[0] != "starting" || got[len(got)-1] != "done" {
		t.Errorf("benign lines lost: %v", got)
	}
}

func TestLogsPassesPreviousAndSince(t *testing.T) {
	c := logsFixture(t)
	calls := stubLogs(t, func(string, string) (string, error) { return "x\n", nil })
	runJSON(t, c, "logs", "solo", "--previous", "--since", "10m", "--tail", "0")
	o := (*calls)[0].opts
	if !o.Previous || *o.SinceSeconds != 600 || *o.TailLines != maxTailLines+1 {
		t.Errorf("options: %+v", o)
	}
}

func TestLogsOneUnreadableStreamIsReportedOnTheStream(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(_, container string) (string, error) {
		if container == "sidecar" {
			return "", errors.New(`previous terminated container "sidecar" in pod "multi" not found`)
		}
		return "ok\n", nil
	})
	m := runJSON(t, c, "logs", "multi", "--previous")
	streams := m["streams"].([]any)
	bad := streams[1].(map[string]any)
	if !strings.Contains(bad["error"].(string), "not found") || bad["lines"] != float64(0) || streams[0].(map[string]any)["error"] != nil {
		t.Errorf("streams: %v", streams)
	}
	if !strings.Contains(m["note"].(string), "multi/sidecar could not be read") {
		t.Errorf("note = %q", m["note"])
	}
}

func TestLogsFailsWhenNothingCanBeRead(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) {
		return "", errors.New(`Get "https://u:pw@h:1/log": previous terminated container "app" not found`)
	})
	code, out, errOut := run(t, c, "logs", "solo", "--previous")
	if code != 1 || out != "" || !strings.Contains(errOut, "drop --previous") || strings.Contains(errOut, "pw@") {
		t.Errorf("exit %d stdout=%q stderr=%s", code, out, errOut)
	}
}

func TestLogsStatesEmptyOutput(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return "", nil })
	m := runJSON(t, c, "logs", "solo", "--since", "1m")
	if !strings.Contains(m["note"].(string), "no log lines") || m["streams"].([]any)[0].(map[string]any)["lines"] != float64(0) {
		t.Errorf("envelope: %v", m)
	}
}

func TestLogsUsageErrors(t *testing.T) {
	c := logsFixture(t)
	stubLogs(t, func(string, string) (string, error) { return "x", nil })
	for _, args := range [][]string{
		{"logs"}, {"logs", "solo", "-l", "app=x"}, {"logs", "configmap/cfg"}, {"logs", "frobnicators/x"},
		{"logs", "solo", "--tail", "-1"}, {"logs", "solo", "--tail", "20000"}, {"logs", "solo", "--max-bytes", "10"}, {"logs", "a/b/c"},
	} {
		if code, _, _ := run(t, c, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _, _ := run(t, c, "logs", "missing-pod"); code != 1 {
		t.Errorf("missing pod: exit %d, want 1", code)
	}
}
