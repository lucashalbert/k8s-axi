package kube

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucashalbert/axi-sdk-go/cli"
)

const approvedSessionStartHook = "k8s-axi cluster info --timeout 5s"

type setupResult struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Message string `json:"message"`
}

type settingsDoc struct {
	Hooks struct {
		SessionStart []struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"SessionStart"`
	} `json:"hooks"`
	Theme string `json:"theme,omitempty"`
}

func runSetup(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	root, out, errb := testRoot(t, nil, nil)
	root.SetArgs(append([]string{"setup"}, args...))
	code = cli.Execute(root)
	stdout, stderr = splitErr(out.String(), errb.String())
	return code, stdout, stderr
}

func parseSetupResult(t *testing.T, out string) setupResult {
	t.Helper()
	var got setupResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("setup output is not JSON: %v\n%s", err, out)
	}
	return got
}

func parseSettings(t *testing.T, b []byte) settingsDoc {
	t.Helper()
	var doc settingsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("settings are not JSON: %v\n%s", err, b)
	}
	return doc
}

func requireSessionStartHook(t *testing.T, doc settingsDoc) {
	t.Helper()
	if len(doc.Hooks.SessionStart) != 1 {
		t.Fatalf("SessionStart hooks = %d, want 1", len(doc.Hooks.SessionStart))
	}
	hooks := doc.Hooks.SessionStart[0].Hooks
	if len(hooks) != 1 {
		t.Fatalf("SessionStart commands = %d, want 1", len(hooks))
	}
	if hooks[0].Type != "command" || hooks[0].Command != approvedSessionStartHook {
		t.Fatalf("hook = (%q, %q), want (%q, %q)", hooks[0].Type, hooks[0].Command, "command", approvedSessionStartHook)
	}
}

func parseSkillFrontMatter(t *testing.T, b []byte) map[string]string {
	t.Helper()
	fields := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		t.Fatalf("skill is missing front matter:\n%s", b)
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			return fields
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("invalid front matter line %q", line)
		}
		fields[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("skill front matter is not closed:\n%s", b)
	return nil
}

func TestSetupHooksIdempotent(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "claude", "settings.json")
	code, out, errOut := runSetup(t, "hooks", "--settings", settings, "--json")
	if code != 0 {
		t.Fatalf("first run: exit %d out=%s err=%s", code, out, errOut)
	}
	if got := parseSetupResult(t, out); got.Path != settings || !got.Changed || got.Message != "hook added" {
		t.Fatalf("first result = %+v, want path %q changed hook added", got, settings)
	}
	first, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ = runSetup(t, "hooks", "--settings", settings, "--json")
	if code != 0 {
		t.Fatalf("second run: exit %d out=%s", code, out)
	}
	if got := parseSetupResult(t, out); got.Path != settings || got.Changed || got.Message != "hook already present" {
		t.Fatalf("second result = %+v, want path %q unchanged hook already present", got, settings)
	}
	second, _ := os.ReadFile(settings)
	if string(first) != string(second) {
		t.Errorf("second run rewrote the file:\n%s\n---\n%s", first, second)
	}
	requireSessionStartHook(t, parseSettings(t, second))
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
	doc := parseSettings(t, b)
	if doc.Theme != "dark" {
		t.Errorf("theme = %q, want dark", doc.Theme)
	}
	requireSessionStartHook(t, doc)
}

func TestSetupHooksPrintWritesNothing(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	code, out, _ := runSetup(t, "hooks", "--print", "--settings", settings)
	if code != 0 {
		t.Fatalf("exit %d out=%s", code, out)
	}
	requireSessionStartHook(t, parseSettings(t, []byte(out)))
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
	if got := parseSkillFrontMatter(t, b)["name"]; got != "k8s-axi" {
		t.Errorf("skill name = %q, want k8s-axi", got)
	}
}
