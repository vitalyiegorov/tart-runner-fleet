package app

import "testing"

// The store key is how a binding's durable rows are addressed, so two scale
// sets that differ in either component must never share one, and the key must
// be stable across processes — it is written to SQLite and read back by a later
// daemon. It must also stay positive: zero is the store's "unset" and would
// make a real binding indistinguishable from an absent one.
func TestScopedStoreKeyIsStableDistinctAndNeverZero(t *testing.T) {
	const scope = "sudoku-repo"
	first := scopedStoreKey(scope, 13)
	if first != scopedStoreKey(scope, 13) {
		t.Fatal("the same scope and scale set produced two different keys")
	}
	if first <= 0 {
		t.Fatalf("key %d is not a usable store address", first)
	}
	if same := scopedStoreKey(scope, 14); same == first {
		t.Fatal("two scale sets in one scope collided")
	}
	if same := scopedStoreKey("knee-repo", 13); same == first {
		t.Fatal("two scopes sharing a scale-set id collided")
	}

	// Every key the fleet can produce stays inside the positive range, because
	// the sign bit is masked off rather than hoped about.
	seen := make(map[int64]struct{})
	for id := range 512 {
		key := scopedStoreKey("fleet-repo", id)
		if key <= 0 {
			t.Fatalf("scale set %d produced non-positive key %d", id, key)
		}
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("scale set %d collided with an earlier key", id)
		}
		seen[key] = struct{}{}
	}
}
