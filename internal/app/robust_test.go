package app

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run the real binary the way an agent does: a fresh process, a
// kubeconfig file and the environment, nothing injected.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "k8s-axi-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "k8s-axi")
		if runtime.GOOS == "windows" {
			binPath += ".exe"
		}
		out, err := exec.Command("go", "build", "-ldflags", "-X main.version=0.0.1", "-o", binPath, "../../cmd/k8s-axi").CombinedOutput()
		if err != nil {
			buildErr = &buildError{err, string(out)}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	// Each test gets its own copy, because update replaces the executable.
	cp := filepath.Join(t.TempDir(), filepath.Base(binPath))
	b, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return cp
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

// runBin executes the binary with an isolated home and cache, and returns the
// exit code and both streams.
func runBin(t *testing.T, bin string, env []string, args ...string) (int, string, string) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(bin, args...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CACHE_HOME=" + home, "LocalAppData=" + home, "USERPROFILE=" + home}, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func kubeconfig(t *testing.T, current, server string) string {
	t.Helper()
	body := "apiVersion: v1\nkind: Config\ncurrent-context: " + current + "\nclusters:\n- name: c\n  cluster: {server: \"" + server + "\"}\n" +
		"contexts:\n- name: prod\n  context: {cluster: c, user: u}\n- name: staging\n  context: {cluster: c, user: u}\nusers:\n- name: u\n  user: {token: x}\n"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNoCurrentContextIsAUsageErrorListingContexts(t *testing.T) {
	bin := binary(t)
	kc := kubeconfig(t, "", "https://127.0.0.1:1")
	code, out, errOut := runBin(t, bin, []string{"K8S_AXI_NO_UPDATE_CHECK=1"}, "get", "pods", "--kubeconfig", kc)
	if code != 2 || out != "" || !strings.Contains(errOut, "prod") || !strings.Contains(errOut, "staging") || !strings.Contains(errOut, "--context") {
		t.Errorf("exit %d stdout=%q stderr=%s", code, out, errOut)
	}
	// --context rescues it without touching the file.
	before, _ := os.ReadFile(kc)
	runBin(t, bin, []string{"K8S_AXI_NO_UPDATE_CHECK=1"}, "cluster", "info", "--kubeconfig", kc, "--context", "prod", "--timeout", "1s")
	if after, _ := os.ReadFile(kc); !bytes.Equal(before, after) {
		t.Error("the kubeconfig changed")
	}
}

func TestHangingAPIServerIsBoundedByTimeout(t *testing.T) {
	bin := binary(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	kc := kubeconfig(t, "prod", srv.URL)
	for _, args := range [][]string{{"cluster", "info"}, {"get", "pods", "-n", "default"}, {"discover"}, {"diagnose", "deploy/web", "-n", "default"}} {
		start := time.Now()
		code, out, errOut := runBin(t, bin, []string{"K8S_AXI_NO_UPDATE_CHECK=1"}, append(args, "--kubeconfig", kc, "--timeout", "1s")...)
		took := time.Since(start)
		// diagnose may stop at three times the timeout; the others at one.
		if limit := 8 * time.Second; took > limit {
			t.Errorf("%v took %v", args, took)
		}
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "error:") || !strings.Contains(errOut, "hint:") || strings.Contains(errOut, "goroutine ") {
			t.Errorf("%v: exit %d stdout=%q stderr=%s", args, code, out, errOut)
		}
	}
}

// deadProxy accepts connections and drops them, like a network that fails
// halfway through a download.
func deadProxy(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return "http://" + l.Addr().String()
}

func TestUpdateFailureLeavesTheBinaryUntouched(t *testing.T) {
	for name, proxy := range map[string]string{"unreachable": "http://127.0.0.1:1", "interrupted": ""} {
		t.Run(name, func(t *testing.T) {
			bin := binary(t)
			if proxy == "" {
				proxy = deadProxy(t)
			}
			before, _ := os.ReadFile(bin)
			code, _, errOut := runBin(t, bin, []string{"HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy}, "update")
			if code != 1 || strings.Contains(errOut, "goroutine ") || errOut == "" {
				t.Errorf("exit %d stderr=%s", code, errOut)
			}
			if after, _ := os.ReadFile(bin); !bytes.Equal(before, after) {
				t.Error("the binary changed")
			}
			if entries, _ := os.ReadDir(filepath.Dir(bin)); len(entries) != 1 {
				t.Errorf("leftover files next to the binary: %v", entries)
			}
		})
	}
}

// With the network gone the daily update check must not change what a command
// prints or how it exits, and must not add a notice.
func TestOfflineUpdateCheckChangesNothing(t *testing.T) {
	bin := binary(t)
	offline := []string{"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1"}
	wantCode, wantOut, _ := runBin(t, bin, []string{"K8S_AXI_NO_UPDATE_CHECK=1"}, "version", "--json")
	start := time.Now()
	code, out, errOut := runBin(t, bin, offline, "version", "--json")
	if code != wantCode || out != wantOut || errOut != "" {
		t.Errorf("exit %d (want %d) stdout=%q (want %q) stderr=%q", code, wantCode, out, wantOut, errOut)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the check delayed the command by %v", d)
	}
}
