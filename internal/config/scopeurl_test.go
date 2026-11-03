package config

import "testing"

// TestScopeURLKeyNamesOneScopeHoweverItIsSpelled: `scale-sets retire` refuses a
// --from that is the scope's current configUrl, so two spellings of one scope
// must compare equal and anything that is not a scope URL must be refused.
func TestScopeURLKeyNamesOneScopeHoweverItIsSpelled(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/owner":       "https://github.com/owner",
		"https://GitHub.com/Owner/":      "https://github.com/owner",
		"https://github.com//owner":      "https://github.com/owner",
		"https://github.com/owner/repo":  "https://github.com/owner/repo",
		"https://github.com/Owner/Repo/": "https://github.com/owner/repo",
	} {
		if got, err := ScopeURLKey(raw); err != nil || got != want {
			t.Fatalf("ScopeURLKey(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "::", "http://github.com/owner", "https://github.com", "https://github.com/a/b/c",
		"https://github.com/a//b", "https://u@github.com/owner", "https://github.com/owner?x=1", "https://github.com/owner#f"} {
		if _, err := ScopeURLKey(raw); err == nil {
			t.Fatalf("ScopeURLKey(%q) must be refused", raw)
		}
	}
}
