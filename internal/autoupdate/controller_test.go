package autoupdate

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeHost struct {
	current    Generation
	events     []string
	validate   error
	activate   error
	ready      error
	rollback   error
	currentErr error
	prepareErr error
	commitErr  error
	committed  Generation
}

func (h *fakeHost) Current(context.Context) (Generation, error) {
	h.events = append(h.events, "current")
	return h.current, h.currentErr
}

func (h *fakeHost) Validate(_ context.Context, candidate Generation) error {
	h.events = append(h.events, "validate:"+candidate.Version)
	return h.validate
}

func (h *fakeHost) Prepare(_ context.Context, current, candidate Generation) error {
	h.events = append(h.events, "prepare:"+current.Version+"->"+candidate.Version)
	return h.prepareErr
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
	return h.commitErr
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

func TestApplyAtomicallyRollsOutConfigWithinTheInstalledRelease(t *testing.T) {
	current := generation("v1", "authority")
	candidate := current
	candidate.ConfigPath = "/state/variants/maestro-4x6.json"
	host := &fakeHost{current: current}
	if err := (Controller{Host: host}).Apply(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	want := []string{"current", "validate:v1", "prepare:v1->v1", "activate:v1", "ready:v1", "commit:v1"}
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

func TestApplyCoversEveryFailClosedBoundary(t *testing.T) {
	current := generation("v1", "authority")
	candidate := generation("v2", "authority")
	failure := errors.New("failure")
	tests := []struct {
		name string
		host *fakeHost
		want []string
	}{
		{name: "current", host: &fakeHost{current: current, currentErr: failure}, want: []string{"current"}},
		{name: "invalid current", host: &fakeHost{current: Generation{}}, want: []string{"current"}},
		{name: "prepare", host: &fakeHost{current: current, prepareErr: failure}, want: []string{"current", "validate:v2", "prepare:v1->v2"}},
		{name: "activate", host: &fakeHost{current: current, activate: failure}, want: []string{"current", "validate:v2", "prepare:v1->v2", "activate:v2", "rollback:v1"}},
		{name: "commit", host: &fakeHost{current: current, commitErr: failure}, want: []string{"current", "validate:v2", "prepare:v1->v2", "activate:v2", "ready:v2", "commit:v2", "rollback:v1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (Controller{Host: test.host}).Apply(context.Background(), candidate)
			if err == nil || !reflect.DeepEqual(test.host.events, test.want) {
				t.Fatalf("error=%v events=%v", err, test.host.events)
			}
		})
	}
	if err := (Controller{}).Apply(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("nil host error=%v", err)
	}
	for _, invalid := range []Generation{{}, {Version: "v2", Mode: "authority", ReleaseDir: "relative", ConfigPath: "/config", Endpoint: "unix:///x"},
		{Version: "v2", Mode: "other", ReleaseDir: "/release", ConfigPath: "/config", Endpoint: "unix:///x"}} {
		if err := (Controller{Host: &fakeHost{current: current}}).Apply(context.Background(), invalid); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("invalid=%+v error=%v", invalid, err)
		}
	}
}

func TestUpdatesAreStrictlyForwardOnly(t *testing.T) {
	for _, candidate := range []string{"v0.1.77+main.old", "v0.1.78+main.other", "not-a-version", "v0.1.999999999999999999999999999999"} {
		host := &fakeHost{current: generation("v0.1.78+main.current", "authority")}
		next := generation(candidate, "authority")
		if err := (Controller{Host: host}).Apply(context.Background(), next); !errors.Is(err, ErrDowngrade) {
			t.Fatalf("candidate=%s error=%v", candidate, err)
		}
		if !reflect.DeepEqual(host.events, []string{"current"}) {
			t.Fatalf("candidate=%s mutated host: %v", candidate, host.events)
		}
	}
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{left: "v1", right: "v1.0.1", want: -1},
		{left: "v2.1", right: "v2.0.9", want: 1},
		{left: "v1.2.3-a", right: "v1.2.3+b", want: 0},
	} {
		got, err := compareVersions(test.left, test.right)
		if err != nil || got != test.want {
			t.Fatalf("compare %s %s = %d, %v", test.left, test.right, got, err)
		}
	}
	if _, err := compareVersions("bad", "v1"); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("invalid current version error=%v", err)
	}
}
