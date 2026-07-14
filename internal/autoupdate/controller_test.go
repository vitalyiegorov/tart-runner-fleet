package autoupdate

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeHost struct {
	current   Generation
	events    []string
	validate  error
	activate  error
	ready     error
	rollback  error
	committed Generation
}

func (h *fakeHost) Current(context.Context) (Generation, error) {
	h.events = append(h.events, "current")
	return h.current, nil
}

func (h *fakeHost) Validate(_ context.Context, candidate Generation) error {
	h.events = append(h.events, "validate:"+candidate.Version)
	return h.validate
}

func (h *fakeHost) Prepare(_ context.Context, current, candidate Generation) error {
	h.events = append(h.events, "prepare:"+current.Version+"->"+candidate.Version)
	return nil
}

func (h *fakeHost) Activate(_ context.Context, candidate Generation) error {
	h.events = append(h.events, "activate:"+candidate.Version)
	return h.activate
}

func (h *fakeHost) Ready(_ context.Context, candidate Generation) error {
	h.events = append(h.events, "ready:"+candidate.Version)
	return h.ready
}

func (h *fakeHost) Commit(_ context.Context, candidate Generation) error {
	h.events = append(h.events, "commit:"+candidate.Version)
	h.committed = candidate
	return nil
}

func (h *fakeHost) Rollback(_ context.Context, current Generation) error {
	h.events = append(h.events, "rollback:"+current.Version)
	return h.rollback
}

func generation(version, mode string) Generation {
	return Generation{Version: version, Mode: mode, ReleaseDir: "/releases/" + version,
		ConfigPath: "/state/fleet.json", Endpoint: "unix:///state/fleetd.sock"}
}

func TestApplyPersistsEveryVerifiedUpdateInTheSameMode(t *testing.T) {
	host := &fakeHost{current: generation("v1", "authority")}
	candidate := generation("v2", "authority")
	if err := (Controller{Host: host}).Apply(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	want := []string{"current", "validate:v2", "prepare:v1->v2", "activate:v2", "ready:v2", "commit:v2"}
	if !reflect.DeepEqual(host.events, want) || host.committed != candidate {
		t.Fatalf("events=%v committed=%+v", host.events, host.committed)
	}
}

func TestApplyRollsBackBinaryConfigModeAndBootPlistWhenReadinessFails(t *testing.T) {
	probeFailure := errors.New("candidate never became ready")
	host := &fakeHost{current: generation("v1", "authority"), ready: probeFailure}
	err := (Controller{Host: host}).Apply(context.Background(), generation("v2", "authority"))
	if !errors.Is(err, probeFailure) {
		t.Fatalf("error=%v", err)
	}
	want := []string{"current", "validate:v2", "prepare:v1->v2", "activate:v2", "ready:v2", "rollback:v1"}
	if !reflect.DeepEqual(host.events, want) {
		t.Fatalf("events=%v", host.events)
	}
}

func TestApplyRejectsSchemaAndModeDriftBeforeTouchingLaunchd(t *testing.T) {
	validationFailure := errors.New("candidate rejects persisted config")
	host := &fakeHost{current: generation("v1", "authority"), validate: validationFailure}
	err := (Controller{Host: host}).Apply(context.Background(), generation("v2", "observe"))
	if !errors.Is(err, ErrModeChange) {
		t.Fatalf("mode error=%v", err)
	}
	if !reflect.DeepEqual(host.events, []string{"current"}) {
		t.Fatalf("mode drift touched host: %v", host.events)
	}

	host = &fakeHost{current: generation("v1", "authority"), validate: validationFailure}
	err = (Controller{Host: host}).Apply(context.Background(), generation("v2", "authority"))
	if !errors.Is(err, validationFailure) || !reflect.DeepEqual(host.events, []string{"current", "validate:v2"}) {
		t.Fatalf("validation error=%v events=%v", err, host.events)
	}
}

func TestApplyIsIdempotentAndRollbackFailureIsVisible(t *testing.T) {
	current := generation("v1", "authority")
	host := &fakeHost{current: current}
	if err := (Controller{Host: host}).Apply(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(host.events, []string{"current"}) {
		t.Fatalf("idempotent update mutated host: %v", host.events)
	}

	readyFailure := errors.New("not ready")
	rollbackFailure := errors.New("rollback failed")
	host = &fakeHost{current: current, ready: readyFailure, rollback: rollbackFailure}
	err := (Controller{Host: host}).Apply(context.Background(), generation("v2", "authority"))
	if !errors.Is(err, readyFailure) || !errors.Is(err, rollbackFailure) {
		t.Fatalf("joined error=%v", err)
	}
}
