package simulation_test

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Issue #400 -- a rerun's demand key moved under its bound VM
// ---------------------------------------------------------------------------
//
// On 2026-11-17 the Mac mini booted two maestro VMs for one pony runner request,
// 13 seconds apart. The run had been re-run, so GitHub queued the job again as a
// new runner request under run attempt 2. The broker announced it before REST
// had reported it, so the store did not know its attempt (#525's episode reset
// gives it 0) and its key read attempt 1. A VM was booted bound to that key.
// When REST then reported attempt 2, the row's key moved to attempt 2, nothing
// incarnated the new key, and the scheduler booted a second VM for the same
// request. ADR 0101 freezes the key's attempt at first projection.
//
// The harness could not reach it: every simulated job was attempt 1, REST
// always reported attempt 1, and no job shared a name with another. The rerun
// event closes all three, and property (d) now judges an incarnation by its
// runner request, so the second VM is a finding whatever attempt it names.

// TestIssue400RerunDeliveredBeforeRESTBootsOneVM: one maestro job runs and
// finishes. Its run is re-run while REST lags, so the broker delivers the rerun
// and a VM is bound to it several ticks before REST reports attempt 2.
func TestIssue400RerunDeliveredBeforeRESTBootsOneVM(t *testing.T) {
	t.Parallel()
	cfg := macOSOnlyNodeWorld()
	trace := simTrace{Seed: 400, Ticks: 60, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
		{Tick: 30, Kind: eventRESTLag, Count: 3},
		{Tick: 30, Kind: eventRerun},
	}}
	w := newWorld(t, cfg, trace)
	defer w.close()
	findings := w.run()
	if len(findings) > 0 {
		t.Fatalf("property violation: %s\n%s", findings[0], trace)
	}
	if len(w.jobs) != 2 {
		t.Fatalf("%d jobs, want the first attempt and its rerun", len(w.jobs))
	}
	first, rerun := w.jobs[0], w.jobs[1]
	if first.status != jobDone || rerun.attempt != 2 || rerun.runID != first.runID || rerun.displayName != first.displayName {
		t.Fatalf("the rerun is not the first attempt queued again: first %+v, rerun %+v", first, rerun)
	}
	// The premise: a VM was bound to the rerun before REST reported its attempt,
	// and REST did report it later in the run.
	record, err := w.store.DemandRecord(w.ctx, w.cfg.Bindings[rerun.binding].StoreKey, rerun.requestID)
	if err != nil {
		t.Fatal(err)
	}
	if record.RunAttempt != 2 {
		t.Fatalf("REST never reported the rerun's attempt: %+v", record)
	}
	bound := map[string]int{}
	for _, observation := range w.observations {
		for _, instance := range observation.Instances {
			if instance.Demand.JobID == rerun.requestID {
				bound[instance.ID] = instance.Demand.Attempt
			}
		}
	}
	if len(bound) != 1 {
		t.Fatalf("instances bound to the rerun's runner request: %v, want exactly one", bound)
	}
	for id, attempt := range bound {
		if attempt != 1 {
			t.Fatalf("instance %s was bound to attempt %d; the premise is a VM bound before REST reported attempt 2", id, attempt)
		}
	}
	if rerun.startedAt.IsZero() {
		t.Fatalf("the rerun never ran: %+v", rerun)
	}
}
