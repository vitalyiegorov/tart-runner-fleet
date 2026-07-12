package telemetry

import "testing"

func TestRedact(t *testing.T) {
	in := map[string]any{"repo": "owner/repo", "token": "secret", "jit_config": "opaque", "nested": map[string]any{"authorization": "Bearer abc", "ok": 7}}
	out := Redact(in)
	if out["repo"] != "owner/repo" || out["token"] != redacted || out["jit_config"] != redacted {
		t.Fatalf("Redact() = %#v", out)
	}
	nested := out["nested"].(map[string]any)
	if nested["authorization"] != redacted || nested["ok"] != 7 {
		t.Fatalf("nested = %#v", nested)
	}
}

func TestRedactDoesNotAliasInput(t *testing.T) {
	in := map[string]any{"safe": []any{map[string]any{"password": "x"}}}
	out := Redact(in)
	out["new"] = true
	if _, ok := in["new"]; ok {
		t.Fatal("output aliases input")
	}
}
