package autoupdate

import (
	"os"
	"path/filepath"
	"sort"
)

// DefaultRetainedReleases is how many superseded generations a node keeps once
// the ones it must never delete are set aside.
//
// Two, because rollback needs one and the operator arguing about a regression
// needs the one before it. A third has never been read on this fleet, and every
// one costs ~40 MiB of a disk the scheduler reserves capacity from.
const DefaultRetainedReleases = 2

// PrunableReleases names the release directories a node may delete, oldest
// first.
//
// The mac studio held 26 downloaded releases — v0.1.461 through v0.1.487,
// several GiB — on a node that was at that moment refusing every job for a 2 GiB
// disk-reserve shortfall (issue #287). Nothing had ever deleted one, because
// nothing was ever asked to.
//
// How they accumulated decides the whole policy, and the obvious rule gets it
// exactly backwards. The updater downloads and verifies every release BEFORE the
// quiescence gate decides whether to adopt it, and the studio never reached
// quiescence, so the gate refused all 26 (issue #282/#230). Every one of those
// directories was therefore NEWER than the running generation — so a collector
// that protected "anything newer than running, because the updater is waiting to
// adopt it" would have kept all 26 and freed nothing on the one node that needed
// it.
//
// Only the NEWEST is a pending candidate. The updater resolves
// `releases/latest` every run, so v0.1.462 through v0.1.486 were never going to
// be adopted by anything; they are downloads that were already superseded when
// the next hour's timer fired.
//
// Four things are kept, and each is a way this could take a node down:
//
//   - `running` is the generation executing this code. Deleting it unlinks the
//     binary of the live process and every helper it re-execs.
//   - `current` is what the `current` symlink resolves to. It is usually the
//     same as running and deliberately not assumed to be: between Commit's
//     symlink swap and the service restart they differ, and that window is
//     exactly when a prune would strand the boot path.
//   - The newest version present, which is what `PendingCandidate` reports and
//     what the next adoption will take. Deleting it would make a node fight its
//     own update.
//   - The `retain` newest generations OLDER than running. That is rollback
//     headroom, and it is deliberately measured among older versions only:
//     rolling back to a version the node has never run is not a rollback.
//
// A name that is not a parseable version is never prunable. The releases
// directory carries the updater's own `.generation-*` staging directories, and a
// concurrent download's staging directory is not this function's to remove.
func PrunableReleases(names []string, running, current string, retain int) []string {
	if running == "" {
		return nil
	}
	if retain < 0 {
		retain = 0
	}
	var versions, older []string
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, err := compareVersions(name, running); err != nil {
			continue
		}
		versions = append(versions, name)
	}
	newest := ""
	for _, name := range versions {
		if newest == "" {
			newest = name
			continue
		}
		if later, err := compareVersions(name, newest); err == nil && later > 0 {
			newest = name
		}
	}
	for _, name := range versions {
		if order, err := compareVersions(name, running); err == nil && order < 0 {
			older = append(older, name)
		}
	}
	sortVersionsNewestFirst(older)
	kept := map[string]bool{running: true, newest: true}
	if current != "" {
		kept[current] = true
	}
	// The rollback headroom is counted over older generations alone, so a node
	// carrying a pending candidate keeps exactly as many previous versions as one
	// that is up to date.
	retained := 0
	for _, name := range older {
		if retained == retain {
			break
		}
		if !kept[name] {
			kept[name] = true
		}
		retained++
	}
	prunable := make([]string, 0, len(versions))
	for _, name := range versions {
		if !kept[name] {
			prunable = append(prunable, name)
		}
	}
	sortVersionsNewestFirst(prunable)
	// Oldest first: a prune interrupted halfway has removed the least useful
	// generations rather than a random subset.
	for left, right := 0, len(prunable)-1; left < right; left, right = left+1, right-1 {
		prunable[left], prunable[right] = prunable[right], prunable[left]
	}
	return prunable
}

func sortVersionsNewestFirst(names []string) {
	sort.Slice(names, func(i, j int) bool {
		order, err := compareVersions(names[i], names[j])
		if err != nil {
			return names[i] > names[j]
		}
		return order > 0
	})
}

// PruneReleases deletes the superseded generations beneath rootDir and reports
// which ones went.
//
// It is best-effort by construction and returns no error for a directory it
// could not remove. Reclaiming disk is a courtesy the caller performs on the way
// to doing something else — downloading a release, committing one — and failing
// that errand because a stale directory is busy would convert a disk-space
// problem into an update failure. What it must never do is delete the wrong
// thing, and that judgement lives in PrunableReleases where it is testable
// without a filesystem.
func PruneReleases(rootDir, running string, retain int) []string {
	if rootDir == "" || running == "" {
		return nil
	}
	releases := filepath.Join(rootDir, "releases")
	names, err := ReleaseDirNames(releases)
	if err != nil {
		return nil
	}
	removed := make([]string, 0, len(names))
	for _, name := range PrunableReleases(names, running, currentGenerationName(rootDir), retain) {
		if os.RemoveAll(filepath.Join(releases, name)) == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

// currentGenerationName is the version the `current` symlink resolves to, or
// empty when it cannot be read.
//
// Empty is the safe answer only because `running` is always excluded too: this
// exists to protect the window in which the two differ, and a node whose symlink
// is unreadable still cannot lose the generation it is executing.
func currentGenerationName(rootDir string) string {
	target, err := os.Readlink(filepath.Join(rootDir, CurrentGenerationLink))
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Clean(target))
}
