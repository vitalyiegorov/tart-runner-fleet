package config

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func exampleBurst(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../config/fleet.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func decodeDoc(t *testing.T, doc map[string]any) (Config, error) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return Decode(bytes.NewReader(raw))
}

// A node's file moves from `maestro` to `compact` after this release is live, so
// this release must read either spelling as the same profile (ADR 0091).
func TestCompactKeyDecodesLikeTheMaestroKey(t *testing.T) {
	doc := exampleBurst(t)
	want, err := decodeDoc(t, doc)
	if err != nil {
		t.Fatalf("decode the maestro spelling: %v", err)
	}
	burst := doc["macosBurst"].(map[string]any)
	burst["compact"] = burst["maestro"]
	delete(burst, "maestro")
	got, err := decodeDoc(t, doc)
	if err != nil {
		t.Fatalf("decode the compact spelling: %v", err)
	}
	if got.MacOS.Compact.ID != want.MacOS.Compact.ID || got.MacOS.Compact.Label != want.MacOS.Compact.Label ||
		got.MacOS.Compact.MaxActive != want.MacOS.Compact.MaxActive {
		t.Fatalf("compact decoded as %+v, want %+v", got.MacOS.Compact, want.MacOS.Compact)
	}
}

// Two spellings in one file could disagree, and neither may silently win.
func TestBothProfileKeysFailClosed(t *testing.T) {
	doc := exampleBurst(t)
	burst := doc["macosBurst"].(map[string]any)
	burst["compact"] = burst["maestro"]
	if _, err := decodeDoc(t, doc); err == nil || !strings.Contains(err.Error(), "compact") {
		t.Fatalf("a file naming both keys must fail closed, got %v", err)
	}
}

// `scale-sets provision --write` re-encodes a live file, and a rollback decodes it
// with DisallowUnknownFields. Until every release in rollback reach reads `compact`,
// a write keeps the key they read.
func TestEncodeKeepsTheKeyARollbackReads(t *testing.T) {
	cfg, err := decodeDoc(t, exampleBurst(t))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Encode(&out, cfg); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	burst := doc["macosBurst"].(map[string]any)
	if _, ok := burst["maestro"]; !ok {
		t.Error("encode dropped the maestro key an older release requires")
	}
	if _, ok := burst["compact"]; ok {
		t.Error("encode wrote the compact key an older release refuses")
	}
}
