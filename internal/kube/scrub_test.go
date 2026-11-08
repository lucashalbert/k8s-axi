package kube

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func secretObj() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{
			"name": "db", "namespace": "default",
			"annotations": map[string]any{
				lastApplied: `{"data":{"password":"hunter2-plain"}}`,
				"owner":     "team-a",
			},
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
		},
		"data":       map[string]any{"password": b64("hunter2-plain"), "user": b64("admin")},
		"stringData": map[string]any{"token": "tok-plain-value"},
	}
}

func TestScrubSecretKeepsOnlyKeysAndSizes(t *testing.T) {
	in := secretObj()
	out := scrubObject(in)
	raw, _ := json.Marshal(out)
	for _, leak := range []string{"hunter2-plain", b64("hunter2-plain"), b64("admin"), "admin", "tok-plain-value", "managedFields", "kubectl"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("scrubbed Secret still contains %q: %s", leak, raw)
		}
	}
	data := out["data"].(map[string]any)
	if data["password"] != "<13 bytes>" || data["user"] != "<5 bytes>" {
		t.Errorf("data = %v", data)
	}
	if out["stringData"].(map[string]any)["token"] != "<15 bytes>" {
		t.Errorf("stringData = %v", out["stringData"])
	}
	ann := out["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["owner"] != "team-a" || len(ann) != 1 {
		t.Errorf("annotations = %v", ann)
	}
	// The input is left alone.
	if in["data"].(map[string]any)["password"] != b64("hunter2-plain") {
		t.Error("scrubObject mutated its input")
	}
}

func TestScrubDropsLastAppliedEverywhere(t *testing.T) {
	obj := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{
		"name": "web", "annotations": map[string]any{lastApplied: `{"env":"x"}`}}}
	out := scrubObject(obj)
	if _, ok := out["metadata"].(map[string]any)["annotations"]; ok {
		t.Errorf("empty annotations kept: %v", out["metadata"])
	}
}

func TestScrubMasksCredentialEnvLiterals(t *testing.T) {
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "p"},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name": "app",
			"env": []any{
				map[string]any{"name": "DB_PASSWORD", "value": "env-literal-pw"},
				map[string]any{"name": "LOG_LEVEL", "value": "debug"},
				map[string]any{"name": "API_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}}},
			},
		}}, "volumes": []any{map[string]any{"name": "v", "secret": map[string]any{"secretName": "db-creds"}}}}}
	raw, _ := json.Marshal(scrubObject(pod))
	s := string(raw)
	if strings.Contains(s, "env-literal-pw") {
		t.Errorf("env literal leaked: %s", s)
	}
	for _, keep := range []string{"debug", "db-creds", `"secretKeyRef"`} {
		if !strings.Contains(s, keep) {
			t.Errorf("lost %q: %s", keep, s)
		}
	}
}

func TestScrubMasksConfigMapCredentialsAndTokens(t *testing.T) {
	cm := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"},
		"data": map[string]any{
			"db_password": "cm-pw-value",
			"curl":        "curl -H 'Authorization: Bearer abcdefghijklmnop12345' https://x",
			"dsn":         "postgres://user:dsn-pw-value@db:5432/app",
			"mode":        "fast",
		}}
	raw, _ := json.Marshal(scrubObject(cm))
	s := string(raw)
	for _, leak := range []string{"cm-pw-value", "abcdefghijklmnop12345", "dsn-pw-value"} {
		if strings.Contains(s, leak) {
			t.Errorf("leaked %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, "fast") {
		t.Errorf("lost benign value: %s", s)
	}
}

func TestDataKeysSizes(t *testing.T) {
	got := dataKeys(secretObj())
	want := []keySize{{"password", 13}, {"token", 15}, {"user", 5}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %v, want %v", i, got[i], want[i])
		}
	}
	cm := map[string]any{"kind": "ConfigMap", "data": map[string]any{"a": "hello"}, "binaryData": map[string]any{"b": b64("xyz")}}
	got = dataKeys(cm)
	if len(got) != 2 || got[0] != (keySize{"a", 5}) || got[1] != (keySize{"b", 3}) {
		t.Errorf("configmap keys = %v", got)
	}
}
