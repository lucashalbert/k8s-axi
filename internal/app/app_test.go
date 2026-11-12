package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lucashalbert/axi-sdk-go/update"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("K8S_AXI_NO_UPDATE_CHECK", "1")
	var out, errb bytes.Buffer
	code = Run(update.Info{Version: "1.2.3", Commit: "abc1234", Date: "2026-11-07"}, args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := run(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	want := map[string]string{"version": "1.2.3", "commit": "abc1234", "date": "2026-11-07"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestUnknownFlagErrorIsOnStdout(t *testing.T) {
	code, out, stderr := run(t, "version", "--nope")
	if code != 2 || stderr != "" {
		t.Fatalf("exit %d (want 2), stderr %q (want empty)", code, stderr)
	}
	for _, want := range []string{"error:", "op: ", "message: ", "unknown flag: --nope", "hint: ", "exit: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestUsageErrorIsOnStdoutJSON(t *testing.T) {
	for name, args := range map[string][]string{
		"usage error":  {"scale", "deploy/web", "-n", "default", "--json"},
		"unknown flag": {"version", "--nope", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, stderr := run(t, args...)
			if code != 2 || stderr != "" {
				t.Fatalf("exit %d (want 2), stderr %q (want empty)", code, stderr)
			}
			var got struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			for _, k := range []string{"op", "message", "hint"} {
				if s, _ := got.Error[k].(string); s == "" {
					t.Errorf("error.%s missing: %v", k, got.Error)
				}
			}
			if got.Error["exit"] != float64(2) {
				t.Errorf("error.exit = %v, want 2", got.Error["exit"])
			}
		})
	}
}

func TestVersionFlags(t *testing.T) {
	for _, f := range []string{"--version", "-v", "-V"} {
		code, out, stderr := run(t, f)
		if code != 0 || out != "1.2.3\n" || stderr != "" {
			t.Errorf("%s: exit %d stdout %q stderr %q, want 0 \"1.2.3\\n\" \"\"", f, code, out, stderr)
		}
	}
}

func TestGlobalFlagsRegistered(t *testing.T) {
	for _, f := range []string{"context", "kubeconfig", "namespace", "json", "yaml", "full", "limit", "timeout"} {
		root := New(update.Info{})
		if root.PersistentFlags().Lookup(f) == nil {
			t.Errorf("missing global flag --%s", f)
		}
	}
	if New(update.Info{}).PersistentFlags().ShorthandLookup("n") == nil {
		t.Error("missing -n shorthand")
	}
}

func TestEnvBindsFlags(t *testing.T) {
	t.Setenv("K8S_AXI_LIMIT", "oops")
	code, _, stderr := run(t, "version")
	if code != 2 {
		t.Fatalf("invalid env value: exit %d, want 2; stderr=%s", code, stderr)
	}
}
