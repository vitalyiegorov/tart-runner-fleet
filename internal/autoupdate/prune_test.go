package autoupdate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// studioReleases is issue #287 exactly: 26 downloaded generations on the mac
// studio, every one of them NEWER than the running v0.1.461, because the
// quiescence gate refused all 26 while the updater kept downloading (#282).
func studioReleases() []string {
	names := []string{"v0.1.461"}
	for minor := 462; minor <= 487; minor++ {
		names = append(names, "v0.1."+itoa(minor))
	}
	return names
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// TestTheStudiosTwentySixReleasesCollapseToFour is the whole issue.
//
// The obvious rule — never delete anything newer than running, because the
// updater is waiting to adopt it — keeps all 26 and frees nothing, which is
// exactly the wrong answer on the only node that ever needed this. Only the
// NEWEST is a pending candidate; the updater resolves `releases/latest` every
// run, so the 25 intermediates were superseded before anything could adopt them.
func TestTheStudiosTwentySixReleasesCollapseToFour(t *testing.T) {
	prunable := PrunableReleases(studioReleases(), "v0.1.461", "v0.1.461", DefaultRetainedReleases)

	if len(prunable) != 25 {
		t.Fatalf("expected the 25 superseded downloads to go, got %d: %v", len(prunable), prunable)
	}
	for _, kept := range []string{"v0.1.461", "v0.1.487"} {
		for _, name := range prunable {
			if name == kept {
				t.Fatalf("%s must never be pruned: %v", kept, prunable)
			}
		}
	}
	if prunable[0] != "v0.1.462" {
		t.Fatalf("the oldest superseded download goes first, got %q", prunable[0])
	}
}

// The generation executing this code is never prunable; deleting it unlinks the
// running binary.
func TestTheRunningGenerationIsNeverPrunable(t *testing.T) {
	names := []string{"v0.1.480", "v0.1.481", "v0.1.482", "v0.1.483"}

	for _, name := range PrunableReleases(names, "v0.1.481", "v0.1.481", 0) {
		if name == "v0.1.481" {
			t.Fatal("the running generation was offered for deletion")
		}
	}
}

// Between Commit's symlink swap and the service restart, `current` and the
// running generation differ. That window is exactly when a prune would strand
// the boot path, so both are kept.
func TestTheCurrentSymlinkTargetIsKeptWhenItDiffersFromRunning(t *testing.T) {
	names := []string{"v0.1.480", "v0.1.481", "v0.1.482"}

	prunable := PrunableReleases(names, "v0.1.480", "v0.1.481", 0)

	if !reflect.DeepEqual(prunable, []string(nil)) && len(prunable) != 0 {
		for _, name := range prunable {
			if name == "v0.1.481" || name == "v0.1.480" {
				t.Fatalf("running and current must both survive: %v", prunable)
			}
		}
	}
}

// Rollback headroom is counted over generations OLDER than running. Rolling
// back to a version the node has never run is not a rollback.
func TestRollbackHeadroomIsCountedAmongOlderGenerationsOnly(t *testing.T) {
	names := []string{"v0.1.470", "v0.1.471", "v0.1.472", "v0.1.480", "v0.1.490", "v0.1.491"}

	prunable := PrunableReleases(names, "v0.1.480", "v0.1.480", 2)

	// Kept: 480 (running), 491 (newest), 472 and 471 (two newest older). Pruned:
	// 470 (beyond headroom) and 490 (a superseded intermediate download).
	if !reflect.DeepEqual(prunable, []string{"v0.1.470", "v0.1.490"}) {
		t.Fatalf("prunable = %v", prunable)
	}
}

// A name that is not a version is never prunable. The releases directory carries
// the updater's own `.generation-*` staging directories, and a concurrent
// download's staging directory is not this function's to remove.
func TestAStagingDirectoryIsNeverPrunable(t *testing.T) {
	names := []string{"v0.1.480", "v0.1.481", ".generation-12345", "downloads"}

	for _, name := range PrunableReleases(names, "v0.1.481", "v0.1.481", 0) {
		if name == ".generation-12345" || name == "downloads" {
			t.Fatalf("a non-version directory was offered for deletion: %v", name)
		}
	}
}

// Without a running generation nothing is safe to delete, because the one thing
// that must survive cannot be identified.
func TestNothingIsPrunableWithoutAKnownRunningGeneration(t *testing.T) {
	if got := PrunableReleases(studioReleases(), "", "", 2); got != nil {
		t.Fatalf("an unknown running generation must prune nothing, got %v", got)
	}
}

// A node holding only what it must keep prunes nothing.
func TestAnUpToDateNodePrunesNothing(t *testing.T) {
	if got := PrunableReleases([]string{"v0.1.487"}, "v0.1.487", "v0.1.487", 2); len(got) != 0 {
		t.Fatalf("an up-to-date node must prune nothing, got %v", got)
	}
}

// PruneReleases removes what the policy names and reports it, and leaves
// everything the policy kept.
func TestPruneReleasesRemovesOnlyWhatThePolicyNames(t *testing.T) {
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	for _, name := range []string{"v0.1.470", "v0.1.480", "v0.1.481", "v0.1.490"} {
		if err := os.MkdirAll(filepath.Join(releases, name), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join(releases, "v0.1.480"), filepath.Join(root, CurrentGenerationLink)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	removed := PruneReleases(root, "v0.1.480", 0)

	// 470 is beyond the (zero) rollback headroom; 481 is a superseded
	// intermediate download that no adoption will ever take, which is the studio's
	// case in miniature. 480 is running and current, 490 is the pending candidate.
	if !reflect.DeepEqual(removed, []string{"v0.1.470", "v0.1.481"}) {
		t.Fatalf("removed = %v", removed)
	}
	for _, kept := range []string{"v0.1.480", "v0.1.490"} {
		if _, err := os.Stat(filepath.Join(releases, kept)); err != nil {
			t.Fatalf("%s must survive: %v", kept, err)
		}
	}
	for _, gone := range []string{"v0.1.470", "v0.1.481"} {
		if _, err := os.Stat(filepath.Join(releases, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should have been removed: %v", gone, err)
		}
	}
}

// Reclaiming disk is an errand performed on the way to something else. A root
// with no releases directory, or none at all, reports nothing and fails nothing.
func TestPruningIsBestEffort(t *testing.T) {
	if got := PruneReleases(t.TempDir(), "v0.1.480", 2); len(got) != 0 {
		t.Fatalf("a root with no releases directory must prune nothing, got %v", got)
	}
	if got := PruneReleases("", "v0.1.480", 2); got != nil {
		t.Fatalf("an empty root must prune nothing, got %v", got)
	}
	if got := PruneReleases(t.TempDir(), "", 2); got != nil {
		t.Fatalf("an unknown running generation must prune nothing, got %v", got)
	}
}

// An unreadable `current` symlink still cannot cost a node the generation it is
// executing: running is excluded independently, and this exists only to protect
// the window in which the two differ.
func TestAnUnreadableCurrentSymlinkStillProtectsTheRunningGeneration(t *testing.T) {
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	for _, name := range []string{"v0.1.470", "v0.1.480"} {
		if err := os.MkdirAll(filepath.Join(releases, name), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	removed := PruneReleases(root, "v0.1.480", 0)

	if !reflect.DeepEqual(removed, []string{"v0.1.470"}) {
		t.Fatalf("removed = %v", removed)
	}
	if _, err := os.Stat(filepath.Join(releases, "v0.1.480")); err != nil {
		t.Fatalf("the running generation must survive an unreadable symlink: %v", err)
	}
}

// Version ordering falls back to a lexical comparison for names the version
// parser cannot rank against each other, so the sort stays total and the result
// stays deterministic.
func TestVersionOrderingIsTotal(t *testing.T) {
	names := []string{"v0.1.9", "v0.1.10", "v0.1.2"}

	sortVersionsNewestFirst(names)

	if !reflect.DeepEqual(names, []string{"v0.1.10", "v0.1.9", "v0.1.2"}) {
		t.Fatalf("newest-first order = %v", names)
	}
}
