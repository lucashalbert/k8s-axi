package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/update"
)

// Bounds on resource use against large clusters. The budgets they enforce
// are documented in docs/performance.md. The benchmarks next to them print
// the numbers; they never fail.

// pagedList makes the fake clientset behave like an API server that honours
// limit and continue for a collection of total items, and returns the list
// requests it saw so a test can assert how much one command asked for.
type pagedList struct {
	mu     sync.Mutex
	limits []int64
	sent   int
}

func (p *pagedList) requests() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.limits...)
}

// servePaged serves total items of one resource, built by item, the way the
// API server does.
func servePaged(cs *kubefake.Clientset, resource string, total int, list func() runtime.Object, item func(i int) runtime.Object) *pagedList {
	p := &pagedList{}
	cs.PrependReactor("list", resource, func(a clienttesting.Action) (bool, runtime.Object, error) {
		o := a.(clienttesting.ListActionImpl).ListOptions
		start, _ := strconv.Atoi(o.Continue)
		end := total
		if o.Limit > 0 && start+int(o.Limit) < total {
			end = start + int(o.Limit)
		}
		p.mu.Lock()
		p.limits = append(p.limits, o.Limit)
		p.sent += end - start
		p.mu.Unlock()
		items := make([]runtime.Object, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, item(i))
		}
		l := list()
		if err := meta.SetList(l, items); err != nil {
			return true, nil, err
		}
		acc, _ := meta.ListAccessor(l)
		if end < total {
			acc.SetContinue(strconv.Itoa(end))
			rest := int64(total - end)
			acc.SetRemainingItemCount(&rest)
		}
		return true, l, nil
	})
	return p
}

func servePods(cs *kubefake.Clientset, total int) *pagedList {
	return servePaged(cs, "pods", total, func() runtime.Object { return &corev1.PodList{} },
		func(i int) runtime.Object { return runningPod("default", fmt.Sprintf("pod-%06d", i), nil) })
}

func TestGetListIsCutAtTheServer(t *testing.T) {
	freezeTime(t)
	c, cs := readFixture(t, nil)
	srv := servePods(cs, 20000)
	m := runJSON(t, c, "get", "pods", "-A")
	if got := len(items(m)); got != defaultLimit || m["truncated"] != true {
		t.Fatalf("rows %d truncated %v, want %d and true", got, m["truncated"], defaultLimit)
	}
	if req := srv.requests(); len(req) != 1 || req[0] != int64(defaultLimit) || srv.sent != defaultLimit {
		t.Errorf("list requests %v, server sent %d items; want one request of limit %d", req, srv.sent, defaultLimit)
	}
}

func TestGetLimitZeroPagesTheList(t *testing.T) {
	freezeTime(t)
	c, cs := readFixture(t, nil)
	srv := servePods(cs, 2300)
	m := runJSON(t, c, "get", "pods", "-A", "--limit", "0")
	if got := len(items(m)); got != 2300 || m["truncated"] != false {
		t.Fatalf("rows %d truncated %v, want 2300 and false", got, m["truncated"])
	}
	for _, l := range srv.requests() {
		if l <= 0 || l > listChunk {
			t.Errorf("list request with limit %d, want 1..%d: %v", l, listChunk, srv.requests())
		}
	}
	if n := len(srv.requests()); n != 5 {
		t.Errorf("%d list requests for 2300 items, want 5", n)
	}
}

func BenchmarkGetLargeList(b *testing.B) {
	for _, limit := range []string{"50", "0"} {
		b.Run("limit="+limit, func(b *testing.B) {
			c, cs := readFixture(b, nil)
			servePods(cs, 20000)
			b.ReportAllocs()
			for b.Loop() {
				root, out, _ := testRoot(b, c, nil)
				root.SetArgs([]string{"get", "pods", "-A", "--limit", limit, "--json"})
				if err := root.Execute(); err != nil {
					b.Fatal(err)
				}
				out.Reset()
			}
		})
	}
}

// wireAPI is a small API server on a real socket, for the bounds that hold
// between the client and the wire: it serves aggregated discovery for many
// API groups, each backed by one CRD, and the CRD list with limit and
// continue.
type wireAPI struct {
	URL      string
	mu       sync.Mutex
	paths    []string
	crdLimit []int
	inflight int
	peak     int
}

func newWireAPI(t testing.TB, groups int) *wireAPI {
	t.Helper()
	w := &wireAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.paths = append(w.paths, r.URL.Path)
		w.inflight++
		w.peak = max(w.peak, w.inflight)
		w.mu.Unlock()
		defer func() { w.mu.Lock(); w.inflight--; w.mu.Unlock() }()
		w.serve(rw, r, groups)
	}))
	t.Cleanup(srv.Close)
	w.URL = srv.URL
	return w
}

func (w *wireAPI) serve(rw http.ResponseWriter, r *http.Request, groups int) {
	const aggregated = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList"
	rw.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/version":
		_, _ = fmt.Fprint(rw, `{"major":"1","minor":"37","gitVersion":"v1.37.0"}`)
	case "/api":
		rw.Header().Set("Content-Type", aggregated)
		_, _ = fmt.Fprint(rw, `{"kind":"APIGroupDiscoveryList","apiVersion":"apidiscovery.k8s.io/v2","items":[{"versions":[{"version":"v1","resources":[
{"resource":"pods","responseKind":{"group":"","version":"v1","kind":"Pod"},"scope":"Namespaced","singularResource":"pod","verbs":["get","list"]}]}]}]}`)
	case "/apis":
		rw.Header().Set("Content-Type", aggregated)
		var items []string
		for i := range groups {
			items = append(items, fmt.Sprintf(`{"metadata":{"name":"g%d.example.com"},"versions":[{"version":"v1","resources":[
{"resource":"things%d","responseKind":{"group":"g%d.example.com","version":"v1","kind":"Thing%d"},"scope":"Namespaced","singularResource":"thing%d","verbs":["get","list"]}]}]}`, i, i, i, i, i))
		}
		_, _ = fmt.Fprintf(rw, `{"kind":"APIGroupDiscoveryList","apiVersion":"apidiscovery.k8s.io/v2","items":[%s]}`, strings.Join(items, ","))
	case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions":
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		start, _ := strconv.Atoi(r.URL.Query().Get("continue"))
		end := groups
		if limit > 0 && start+limit < groups {
			end = start + limit
		}
		w.mu.Lock()
		w.crdLimit = append(w.crdLimit, limit)
		w.mu.Unlock()
		var items []string
		for i := start; i < end; i++ {
			items = append(items, fmt.Sprintf(`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"things%d.g%d.example.com"},"spec":{"group":"g%d.example.com","names":{"plural":"things%d","kind":"Thing%d"}}}`, i, i, i, i, i))
		}
		cont := ""
		if end < groups {
			cont = strconv.Itoa(end)
		}
		_, _ = fmt.Fprintf(rw, `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinitionList","metadata":{"continue":%q},"items":[%s]}`, cont, strings.Join(items, ","))
	default:
		http.NotFound(rw, r)
	}
}

func wireKubeconfig(t testing.TB, url string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := "apiVersion: v1\nkind: Config\ncurrent-context: w\nclusters:\n- name: w\n  cluster: {server: " + url + "}\ncontexts:\n- name: w\n  context: {cluster: w, user: u}\nusers:\n- name: u\n  user: {token: x}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func wireRun(t testing.TB, kc string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	root := cli.New("k8s-axi", update.Info{Version: "0.0.0"}, cli.EnvPrefix("K8S_AXI_"), Module{})
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append(args, "--kubeconfig", kc))
	return cli.Execute(root), out.String(), errb.String()
}

// Discovery over many API groups is one aggregated request, and the CRD
// objects, which carry large schemas in a real cluster, are read in pages.
func TestDiscoverManyCRDsIsBounded(t *testing.T) {
	const groups = 1200
	w := newWireAPI(t, groups)
	start := time.Now()
	code, out, errOut := wireRun(t, wireKubeconfig(t, w.URL), "discover", "--json", "--limit", "0", "--timeout", "20s")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got struct {
		Matched   int    `json:"matched"`
		CRDSource string `json:"crd_source"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Matched != groups+1 || got.CRDSource != "apiextensions" {
		t.Errorf("matched %d source %s, want %d apiextensions", got.Matched, got.CRDSource, groups+1)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v for %d groups", d, groups)
	}
	disc := 0
	for _, p := range w.paths {
		if p == "/api" || p == "/apis" {
			disc++
		}
	}
	if disc > 2 {
		t.Errorf("%d discovery requests for %d groups, want 2: %v", disc, groups, w.paths[:min(len(w.paths), 8)])
	}
	if w.peak > maxWireConcurrency {
		t.Errorf("%d requests in flight at once, want at most %d", w.peak, maxWireConcurrency)
	}
	for _, l := range w.crdLimit {
		if l <= 0 || l > crdChunk {
			t.Errorf("CRD list with limit %d, want 1..%d: %v", l, crdChunk, w.crdLimit)
			break
		}
	}
}

// maxWireConcurrency is the most requests discover may have in flight.
const maxWireConcurrency = 4

func BenchmarkDiscoverManyCRDs(b *testing.B) {
	w := newWireAPI(b, 1200)
	kc := wireKubeconfig(b, w.URL)
	b.ReportAllocs()
	for b.Loop() {
		if code, _, errOut := wireRun(b, kc, "discover", "--json", "--limit", "0", "--timeout", "20s"); code != 0 {
			b.Fatal(errOut)
		}
	}
}

// Events are read as one page of at most maxEventScan, however many exist,
// and the command says the newest may be missing.
func TestEventsReadOneBoundedPage(t *testing.T) {
	freezeTime(t)
	c, cs := readFixture(t, nil)
	srv := servePaged(cs, "events", 5000, func() runtime.Object { return &corev1.EventList{} },
		func(i int) runtime.Object {
			return event("default", fmt.Sprintf("e%05d", i), "Warning", "BackOff", "Pod", "web", time.Duration(i)*time.Second)
		})
	for _, limit := range []string{"50", "0"} {
		m := runJSON(t, c, "events", "-n", "default", "--limit", limit)
		if got := len(m["events"].([]any)); (limit == "50" && got != 50) || (limit == "0" && got != maxEventScan) {
			t.Errorf("--limit %s printed %d events", limit, got)
		}
		if m["truncated"] != true {
			t.Errorf("--limit %s: not marked truncated: %v", limit, m["note"])
		}
	}
	if req := srv.requests(); len(req) != 2 || req[0] != maxEventScan || req[1] != maxEventScan {
		t.Errorf("event list requests %v, want two of limit %d", req, maxEventScan)
	}
}

func BenchmarkEventsLargeNamespace(b *testing.B) {
	c, cs := readFixture(b, nil)
	servePaged(cs, "events", 5000, func() runtime.Object { return &corev1.EventList{} },
		func(i int) runtime.Object {
			return event("default", fmt.Sprintf("e%05d", i), "Warning", "BackOff", "Pod", "web", time.Duration(i)*time.Second)
		})
	b.ReportAllocs()
	for b.Loop() {
		root, _, _ := testRoot(b, c, nil)
		root.SetArgs([]string{"events", "-n", "default", "--json"})
		if err := root.Execute(); err != nil {
			b.Fatal(err)
		}
	}
}

// diagnose on a workload with thousands of pods reads one page of pods, at
// most three log streams, and never more than diagConcurrency calls at once.
func TestDiagnoseManyPodsIsBounded(t *testing.T) {
	freezeTime(t)
	var inflight, peak, reads atomic.Int32
	stubDiagLogs(t, func(context.Context, string, string, bool) (string, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		reads.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		return "x\n", nil
	})
	ds := typed[appsv1.DaemonSet](t, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent, namespace: default, uid: uid-ds}
spec: {selector: {matchLabels: {app: crash}}}
status: {desiredNumberScheduled: 5000, numberReady: 0}`)
	c, cs := readFixture(t, []runtime.Object{ds})
	srv := servePaged(cs, "pods", 5000, func() runtime.Object { return &corev1.PodList{} },
		func(i int) runtime.Object { return crashLoopPod(t, fmt.Sprintf("p-%04d", i), fmt.Sprintf("c-%04d", i)) })
	start := time.Now()
	m := runJSON(t, c, "diagnose", "daemonset", "agent")
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	if srv.sent > diagPodLimit {
		t.Errorf("read %d pods, want at most %d: %v", srv.sent, diagPodLimit, srv.requests())
	}
	if reads.Load() > 3 || len(m["logs"].([]any)) > 3 {
		t.Errorf("%d log reads, %d streams, want at most 3", reads.Load(), len(m["logs"].([]any)))
	}
	if peak.Load() > diagConcurrency {
		t.Errorf("%d concurrent log reads, want at most %d", peak.Load(), diagConcurrency)
	}
	if pods := m["pods"].(map[string]any); pods["shown"].(float64) > diagPodLimit {
		t.Errorf("pods = %v", pods)
	}
}

// logs over a selector that matches thousands of pods reads one page of
// pods, at most maxLogPods streams, and prints at most the byte budget.
func TestLogsManyPodsIsBounded(t *testing.T) {
	freezeTime(t)
	c, cs := readFixture(t, nil)
	srv := servePaged(cs, "pods", 3000, func() runtime.Object { return &corev1.PodList{} },
		func(i int) runtime.Object {
			return runningPod("default", fmt.Sprintf("p-%04d", i), map[string]string{"app": "many"})
		})
	big := strings.Repeat("x", 512) + "\n"
	calls := stubLogs(t, func(string, string) (string, error) { return strings.Repeat(big, 1000), nil })
	_, out, errOut := run(t, c, "logs", "-l", "app=many", "--json")
	if len(*calls) > maxLogPods {
		t.Errorf("%d log streams, want at most %d", len(*calls), maxLogPods)
	}
	if srv.sent > logPodListLimit*logPodScanPages {
		t.Errorf("read %d pods, want at most %d: %v", srv.sent, logPodListLimit*logPodScanPages, srv.requests())
	}
	if !strings.Contains(out, "at least") {
		t.Errorf("the cut scan is not stated: %s", out)
	}
	if len(out) > 3*defaultMaxSize {
		t.Errorf("printed %d bytes for a %d byte budget (stderr %s)", len(out), defaultMaxSize, errOut)
	}
}

func BenchmarkDiagnoseManyPods(b *testing.B) {
	stubDiagLogs(b, func(context.Context, string, string, bool) (string, error) { return "x\n", nil })
	ds := typed[appsv1.DaemonSet](b, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent, namespace: default, uid: uid-ds}
spec: {selector: {matchLabels: {app: crash}}}
status: {desiredNumberScheduled: 5000, numberReady: 0}`)
	c, cs := readFixture(b, []runtime.Object{ds})
	servePaged(cs, "pods", 5000, func() runtime.Object { return &corev1.PodList{} },
		func(i int) runtime.Object { return crashLoopPod(b, fmt.Sprintf("p-%04d", i), fmt.Sprintf("c-%04d", i)) })
	b.ReportAllocs()
	for b.Loop() {
		root, _, _ := testRoot(b, c, nil)
		root.SetArgs([]string{"diagnose", "daemonset", "agent", "--json"})
		if err := root.Execute(); err != nil {
			b.Fatal(err)
		}
	}
}
