package replay_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

const (
	phantomInstanceID  = "trf-maestro-096ffcb3a52d8624"
	phantomOperationID = "op-ea9b705d234ad29f14e79b6d"
)

// reaper stands in for Tart. The phantom's VM exists and is stopped, which is the
// only power state the discharge accepts.
type reaper struct {
	running bool
	deleted []string
}

func (r *reaper) Running(context.Context, string) (bool, error) { return r.running, nil }

func (r *reaper) Reap(_ context.Context, name string, _ operations.Ownership) error {
	r.deleted = append(r.deleted, name)
	return nil
}

// publish renders the real fleet.v1 status document from the store's own
// aggregates, exactly as the daemon's tick does.
func publish(t *testing.T, store *sqlite.Store, parked bool) adminapi.StatusEnvelope {
	t.Helper()
	ctx := context.Background()
	health, err := telemetry.NewHealth(clock{}, telemetry.HealthConfig{Profiles: []string{"macos-maestro"}})
	if err != nil {
		t.Fatal(err)
	}
	retrying, dead, err := store.OperationCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetOperations(retrying, dead); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetInstances("macos-maestro", len(live), 4*len(live), 7168*len(live)); err != nil {
		t.Fatal(err)
	}
	durable, err := store.DeadLetters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	letters := make([]telemetry.DeadLetter, 0, len(durable))
	for _, letter := range durable {
		letters = append(letters, telemetry.DeadLetter{OperationID: letter.OperationID, Kind: letter.Kind,
			Code: letter.Code, ResourceID: letter.ResourceID, Attempts: letter.Attempts,
			Parked: parked && !letter.ResourceProgressing})
	}
	if err := health.SetDeadLetters(letters); err != nil {
		t.Fatal(err)
	}
	// Serve the real handler on a loopback listener and read it back through the
	// real client, so the replay exercises the published document rather than an
	// in-process struct.
	server, err := telemetry.NewServer(health, telemetry.ServerConfig{ControllerVersion: "v0.1.281", ControllerMode: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-served
	})
	client, err := adminapi.NewClient("http://"+listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

type clock struct{}

func (clock) Now() time.Time { return time.Date(2026, 7, 25, 21, 0, 0, 0, time.UTC) }

// quiescent applies the release gate's own rule to a published document: only
// retrying operations, queued jobs, and instances the daemon cannot prove parked
// defer an update.
func quiescent(status adminapi.StatusEnvelope) bool {
	if status.Data.Operations.Retrying != 0 {
		return false
	}
	for _, queue := range status.Data.Queues {
		if queue.Jobs != 0 {
			return false
		}
	}
	live := 0
	for _, instance := range status.Data.Instances {
		live += instance.Count
	}
	parked := map[string]struct{}{}
	for _, letter := range status.Data.Operations.DeadLetters {
		if letter.Parked && letter.ResourceID != "" {
			parked[letter.ResourceID] = struct{}{}
		}
	}
	return live-len(parked) <= 0
}

// TestDeadLetterUpdateDeadlockIsObservableAndDischargeable replays the whole
// 2026-07-25 incident across the durable store, the operator API, and the guarded
// mutation.
//
// The registration behind this dead letter can never be deregistered: GitHub
// answered
//
//	DELETE /orgs/budgie-at/actions/runners/3175
//	422 {"message":"Bad request - Runner trf-maestro-096ffcb3a52d8624 is currently
//	     running a job and cannot be deleted."}
//
// for a runner reporting status=offline, busy=True, labels=[]; a token with
// admin:org failed identically, and no workflow run held it, so the documented
// remedy of cancelling the owning run did not exist. The fleet's part is therefore
// not to complete the cleanup but to stop the wedge from disabling its own repair.
func TestDeadLetterUpdateDeadlockIsObservableAndDischargeable(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	ownership := operations.Ownership{ControllerID: "tart-runner-fleet", ResourceID: phantomInstanceID, OperationID: "op-provision"}
	if err := store.CreateInstance(ctx, operations.Instance{ID: phantomInstanceID, State: operations.StatePlanned,
		Ownership: ownership, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Transition(ctx, operations.Transition{InstanceID: phantomInstanceID,
		ExpectedState: operations.StatePlanned, ExpectedVersion: 0, NextState: operations.StateDraining,
		Operation: operations.Operation{ID: phantomOperationID, IdempotencyKey: "drain:" + phantomInstanceID,
			EffectKey: "drain:" + phantomInstanceID, Kind: "drain", ResourceID: phantomInstanceID, AvailableAt: now}}); err != nil {
		t.Fatal(err)
	}
	// In production this took 835 attempts of the identical classified refusal
	// before ADR 0020's escalation ceiling dead-lettered it. The ceiling itself is
	// pinned in internal/operations; what matters here is the state it lands in.
	claimed, err := store.Claim(ctx, "worker", 1, now, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed=%#v err=%v", claimed, err)
	}
	if err := store.Retry(ctx, phantomOperationID, "worker", "runner lifecycle failed at deregister (runner_busy)",
		now.Add(30*time.Second), true); err != nil {
		t.Fatal(err)
	}

	// Step 1: the wedge is observable, named, and blocks the release gate.
	before := publish(t, store, true)
	if before.Data.Operations.Dead != 1 {
		t.Fatalf("dead operations=%d", before.Data.Operations.Dead)
	}
	letters := before.Data.Operations.DeadLetters
	if len(letters) != 1 || letters[0].OperationID != phantomOperationID ||
		letters[0].ResourceID != phantomInstanceID || letters[0].Code != "deregister:runner_busy" || !letters[0].Parked {
		t.Fatalf("published dead letters=%#v", letters)
	}
	if !quiescent(before) {
		t.Fatal("a parked dead letter and its stopped row must not block a release; that is the deadlock this replays")
	}

	// Step 1b: while the daemon cannot prove the row is parked, the gate still
	// defers. Real capacity is never discounted.
	if quiescent(publish(t, store, false)) {
		t.Fatal("an unproven row released the release gate")
	}

	// Step 2: the operator discharges it. A running VM is refused first.
	vm := &reaper{running: true}
	service := discharge.Service{Store: store, VM: vm, Authority: true, Now: func() time.Time { return now }}
	request := adminapi.DischargeRequest{OperationID: phantomOperationID, InstanceID: phantomInstanceID,
		ReapInstance: true, Confirm: adminapi.DischargeConfirmation,
		Reason: "GitHub 422 runner_busy: permanent registration leak, no owner run exists"}
	if _, err := service.DischargeDeadLetter(ctx, request); !errors.Is(err, adminapi.ErrRefused) {
		t.Fatalf("running VM discharge error=%v", err)
	}
	if len(vm.deleted) != 0 {
		t.Fatalf("a refused discharge deleted a VM: %#v", vm.deleted)
	}

	vm.running = false
	result, err := service.DischargeDeadLetter(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OperationDischarged || !result.InstanceReaped || !result.VMDeleted {
		t.Fatalf("result=%#v", result)
	}
	if len(vm.deleted) != 1 || vm.deleted[0] != phantomInstanceID {
		t.Fatalf("deleted=%#v", vm.deleted)
	}

	// Step 3: the fleet is genuinely quiescent, and the wedge is gone from every
	// published surface rather than merely discounted.
	after := publish(t, store, true)
	if after.Data.Operations.Retrying != 0 || after.Data.Operations.Dead != 0 || len(after.Data.Operations.DeadLetters) != 0 {
		t.Fatalf("post-discharge operations=%#v", after.Data.Operations)
	}
	for _, instance := range after.Data.Instances {
		if instance.Count != 0 {
			t.Fatalf("post-discharge instances=%#v", after.Data.Instances)
		}
	}
	if !quiescent(after) {
		t.Fatal("the discharged fleet is still not quiescent")
	}
}
