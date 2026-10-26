package simulation_test

import "testing"

// ---------------------------------------------------------------------------
// Issue #423 -- a node out of inodes kept admitting guests
// ---------------------------------------------------------------------------
//
// On 2026-10-25 ubuntu-vitalii's root filesystem had every one of its
// 60,850,176 inodes in use while 29 GiB of bytes stayed free, above its 20 GiB
// disk floor. Admission compared bytes only, so the node reported READY and kept
// starting guests, and every one failed with `No space left on device`.
//
// TestIssue423NoGuestIsAdmittedOntoAFilesystemOutOfInodes: work arrives while
// the filesystem is out of inodes. Property (u) proves nothing is spawned for
// the length of the fault, and the run's other properties prove the demand is
// still admitted once the inodes come back -- the guard defers, it never wedges.
func TestIssue423NoGuestIsAdmittedOntoAFilesystemOutOfInodes(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := simTrace{Seed: 423, Ticks: 60, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventInodeExhausted, Count: 6},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "small", Event: "push"},
		{Tick: 2, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "pull_request"},
	}}
	world := newWorld(t, cfg, trace)
	defer world.close()
	if findings := world.run(); len(findings) > 0 {
		t.Fatalf("property violation: %s", findings[0])
	}
	for _, job := range world.jobs {
		if job.startedAt.IsZero() {
			t.Fatalf("%s's job never ran once the inodes came back: %+v", job.repo, job)
		}
		if job.startedAt.Sub(simEpoch) < 6*simTick {
			t.Fatalf("%s's job started at %s, inside the inode exhaustion", job.repo, job.startedAt.Sub(simEpoch))
		}
	}
}
