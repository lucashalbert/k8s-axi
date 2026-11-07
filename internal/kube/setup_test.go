package kube

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucashalbert/axi-sdk-go/cli"
)

func runSetup(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	root, out, errb := testRoot(t, nil, nil)
	root.SetArgs(append([]string{"setup"}, args...))
	code = cli.Execute(root)
	return code, out.String(), errb.String()
}

func TestSetupHooksIdempotent(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "claude", "settings.json")
	if code, out, errOut := runSetup(t, "hooks", "--settings", settings, "--json"); code != 0 || !strings.Contains(out, `"changed":true`) {
		t.Fatalf("first run: exit %d out=%s err=%s", code, out, errOut)
	}
	first, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runSetup(t, "hooks", "--settings", settings, "--json"); code != 0 || !strings.Contains(out, `"changed":false`) {
		t.Fatalf("second run: exit %d out=%s", code, out)
	}
	second, _ := os.ReadFile(settings)
	if string(first) != string(second) {
		t.Errorf("second run rewrote the file:\n%s\n---\n%s", first, second)
	}
	var doc struct {
		Hooks struct {
			SessionStart []struct {
				Hooks []struct{ Type, Command string }
			}
		}
	}
	if err := json.Unmarshal(second, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Hooks.SessionStart) != 1 || doc.Hooks.SessionStart[0].Hooks[0].Command != hookCommand {
		t.Errorf("unexpected hooks: %s", second)
	}
	if !strings.HasPrefix(hookCommand, "k8s-axi cluster info") {
		t.Errorf("hook command %q must run cluster info", hookCommand)
	}
}

func TestSetupHooksPreservesSettings(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runSetup(t, "hooks", "--settings", settings); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	b, _ := os.ReadFile(settings)
	if !strings.Contains(string(b), `"theme": "dark"`) {
		t.Errorf("unrelated key lost: %s", b)
	}
}

func TestSetupHooksPrintWritesNothing(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	code, out, _ := runSetup(t, "hooks", "--print", "--settings", settings)
	if code != 0 || !strings.Contains(out, "SessionStart") || !strings.Contains(out, hookCommand) {
		t.Fatalf("exit %d out=%s", code, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("--print wrote files: %v", entries)
	}
}

func TestSetupHooksHonoursClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if code, _, errOut := runSetup(t, "hooks"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Error(err)
	}
}

func TestSetupSkillInstallsAndIsIdempotent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "skills", "k8s-axi")
	for i := 0; i < 2; i++ {
		if code, _, errOut := runSetup(t, "skill", "--dest", dest); code != 0 {
			t.Fatalf("run %d: exit %d: %s", i, code, errOut)
		}
	}
	b, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "---\nname: k8s-axi\n") {
		t.Errorf("unexpected skill:\n%s", b)
	}
}
