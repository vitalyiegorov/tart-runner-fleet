package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/provision"
)

// strandedStatus is what the daemon published on 2026-09-21 once it had watched
// the same reading stand for longer than a boot takes: the set it BINDS holds
// work, no runner is registered, it holds no instance for it, and the ingest
// check fails on the detector's own sentence (#336).
func strandedStatus() adminapi.StatusEnvelope {
	status := healthyStatus()
	reason := "repo scale set 1 (trf-sudoku-builder) is bound here and has held 3 assigned job(s) and " +
		"3 busy runner(s) for 4h0m0s with no runner registered and no instance on this node: GitHub is " +
		"delivering nothing for this set -- recreate the set"
	status.Data.IngestCheck = &adminapi.Check{OK: false, Reasons: []string{reason}}
	status.Data.StrandedScaleSets = []adminapi.StrandedScaleSet{{Scope: "repo", ScaleSetID: 1,
		Name: "trf-sudoku-builder", Profile: "builder", Assigned: 3, Busy: 3,
		HoldingSince: time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC),
		ObservedAt:   time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), Reason: reason}}
	return status
}

// TestTheIngestDeliveryRowFailsOnAStrandedBoundSet is the doctor line that
// PASSED three times on 2026-09-21 while nothing was being delivered. The
// parked row stays informational (ADR 0054's amendment); a set this node
// SERVES is a different case, and it fails.
func TestTheIngestDeliveryRowFailsOnAStrandedBoundSet(t *testing.T) {
	client := &fakeClient{status: strandedStatus(), metrics: "fleet_up 1"}
	var stdout, stderr bytes.Buffer

	code := runDoctor(context.Background(), client, "", &stdout, &stderr)

	if code != exitDegraded {
		t.Fatalf("a node delivering nothing for a set it binds is degraded: %d\n%s", code, stdout.String())
	}
	line := ""
	for _, row := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(row, "ingest delivery") {
			line = row
		}
	}
	if !strings.HasPrefix(line, "FAIL") {
		t.Fatalf("the ingest-delivery row must FAIL:\n%s", stdout.String())
	}
	for _, want := range []string{"scale set 1", "trf-sudoku-builder", "recreate the set"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the row must say %q: %q", want, line)
		}
	}
	if !strings.Contains(stdout.String(), "PASS   parked scale sets") {
		t.Fatalf("the parked row stays informational (ADR 0054 amendment):\n%s", stdout.String())
	}
}

// boundStrandingDeps is node-b's chair: GitHub still answers assigned=3 busy=3
// registered=0 for the set the node binds, and the daemon beside it has been
// watching that reading since before the boot timeout.
func boundStrandingDeps(t *testing.T, status adminapi.StatusEnvelope, dialErr error) dependencies {
	t.Helper()
	deps := auditDeps(t, &fakeAuditor{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
	}})
	deps.newClient = func(string, time.Duration) (apiClient, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return fakeClient{status: status}, nil
	}
	return deps
}

// The command an operator runs reaches the same verdict as the daemon, and
// unlike the parked evidence it exits 5 on its own: every term in it is this
// node's own reading of the object this node polls.
func TestTheAuditFailsOnASetThisNodeServesAndGitHubStoppedDeliveringFor(t *testing.T) {
	deps := boundStrandingDeps(t, strandedStatus(), nil)
	var stdout, stderr bytes.Buffer

	code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json"},
		&stdout, &stderr, deps)

	if code != exitDegraded {
		t.Fatalf("a bound set nothing is delivered for is a fault: %d (%s)", code, stderr.String())
	}
	for _, want := range []string{"trf-sudoku-builder", "3 assigned job(s)", "recreate the set"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("the finding must say %q: %q", want, stderr.String())
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), "PRIVATE-KEY-SENTINEL") {
		t.Fatal("the App key must never reach an operator surface")
	}

	// --strict, which an operator scripts against, reports it the same way.
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json", "--strict"},
		&stdout, &stderr, deps); code != exitDegraded {
		t.Fatalf("--strict on the same reading is 5: %d", code)
	}
}

// Without the daemon beside it the command cannot tell a wedged set from a
// booting runner, and it says so instead of guessing either way.
func TestAnAuditWithoutTheDaemonCannotJudgeABoundSet(t *testing.T) {
	deps := boundStrandingDeps(t, adminapi.StatusEnvelope{}, errors.New("no such file or directory"))
	var stdout, stderr bytes.Buffer

	code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json", "--strict"},
		&stdout, &stderr, deps)

	if code != exitSuccess {
		t.Fatalf("an unobserved node makes no verdict: %d (%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "bound sets were not judged") {
		t.Fatalf("the missing observation must be named, not assumed: %q", stderr.String())
	}
}

// And a daemon that reports no stranding leaves the reading unjudged too: the
// clock the verdict needs lives in the daemon, not in one command run.
func TestABoundSetTheDaemonHasNotFlaggedIsNotAFinding(t *testing.T) {
	deps := boundStrandingDeps(t, healthyStatus(), nil)
	var stdout, stderr bytes.Buffer

	if code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json", "--strict"},
		&stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("a set the daemon has not flagged is not a finding: %d (%s)", code, stderr.String())
	}
}

type fakeRecreateClient struct {
	deleted []int
	created []string
	newID   int
	err     error
}

func (f *fakeRecreateClient) Inspect(context.Context, githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) {
	return githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetCreate}, nil
}

func (f *fakeRecreateClient) Ensure(_ context.Context, spec githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
	f.created = append(f.created, spec.Name)
	return scaleset.RunnerScaleSet{ID: f.newID, Name: spec.Name}, nil
}

func (f *fakeRecreateClient) Delete(_ context.Context, id int) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

// recreateDeps is a node whose `repo-large` set is stranded on id 17.
func recreateDeps(t *testing.T, client *fakeRecreateClient, written *config.Config) dependencies {
	t.Helper()
	cfg := fleetProvisionConfig()
	for index, set := range cfg.GitHub.Scopes[0].ScaleSets {
		if set.Name == "repo-large" {
			cfg.GitHub.Scopes[0].ScaleSets[index].ID = 17
		}
	}
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	deps := defaultDependencies()
	deps.openConfig = func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(encoded.String())), nil
	}
	deps.loadPrivateKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return githubscaleset.NewPrivateKeySecret("PRIVATE-KEY-SENTINEL"), nil
	}
	deps.openRecreate = func(githubscaleset.GitHubAppAdminConfig) (provision.Recreater, error) { return client, nil }
	deps.writeConfig = func(_ string, cfg config.Config) error {
		if written != nil {
			*written = cfg
		}
		return nil
	}
	return deps
}

// TestRecreateIsGuardedAndReportsTheSubstitution is the remedy an operator had
// to perform by hand three times on 2026-09-21, with a program compiled in
// /tmp. It is now one guarded command, and it says exactly what changed.
func TestRecreateIsGuardedAndReportsTheSubstitution(t *testing.T) {
	client := &fakeRecreateClient{newID: 19}
	var written config.Config
	deps := recreateDeps(t, client, &written)
	var stdout, stderr bytes.Buffer

	code := executeWith(context.Background(), []string{"scale-sets", "recreate", "repo-large",
		"--config", "fleet.json", "--confirm", "recreate-scale-set", "--reason", "stranded, #336"},
		&stdout, &stderr, deps)

	if code != exitSuccess {
		t.Fatalf("a confirmed recreate must succeed: %d (%s)", code, stderr.String())
	}
	if len(client.deleted) != 1 || client.deleted[0] != 17 || len(client.created) != 1 {
		t.Fatalf("the old object is deleted and a replacement created: %v %v", client.deleted, client.created)
	}
	for _, want := range []string{"repo-large", "17", "19", "restart"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("the operator must be told %q: %q", want, stdout.String())
		}
	}
	var persisted int
	for _, set := range written.GitHub.Scopes[0].ScaleSets {
		if set.Name == "repo-large" {
			persisted = set.ID
		}
	}
	if persisted != 19 {
		t.Fatalf("the new id must be persisted: %d", persisted)
	}
	if strings.Contains(stdout.String()+stderr.String(), "PRIVATE-KEY-SENTINEL") {
		t.Fatal("the App key must never reach an operator surface")
	}
}

// The guard is the point: a destructive call needs the exact confirmation and
// a reason, exactly as `provision --apply` and `operations discharge` do.
func TestRecreateRefusesWithoutTheExactConfirmation(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		code int
	}{
		{name: "no confirmation", args: []string{"scale-sets", "recreate", "repo-large", "--config", "fleet.json",
			"--reason", "stranded"}, code: exitUnsafe},
		{name: "wrong confirmation", args: []string{"scale-sets", "recreate", "repo-large", "--config", "fleet.json",
			"--confirm", "yes", "--reason", "stranded"}, code: exitUnsafe},
		{name: "no reason", args: []string{"scale-sets", "recreate", "repo-large", "--config", "fleet.json",
			"--confirm", "recreate-scale-set"}, code: exitUnsafe},
		{name: "no set named", args: []string{"scale-sets", "recreate", "--config", "fleet.json",
			"--confirm", "recreate-scale-set", "--reason", "stranded"}, code: exitUsage},
		{name: "no config", args: []string{"scale-sets", "recreate", "repo-large",
			"--confirm", "recreate-scale-set", "--reason", "stranded"}, code: exitUsage},
		{name: "unknown set", args: []string{"scale-sets", "recreate", "repo-absent", "--config", "fleet.json",
			"--confirm", "recreate-scale-set", "--reason", "stranded"}, code: exitNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeRecreateClient{newID: 19}
			var stdout, stderr bytes.Buffer
			if code := executeWith(context.Background(), tt.args, &stdout, &stderr,
				recreateDeps(t, client, nil)); code != tt.code {
				t.Fatalf("code = %d, want %d (%s)", code, tt.code, stdout.String()+stderr.String())
			}
			if len(client.deleted) != 0 {
				t.Fatalf("a refusal must delete nothing: %v", client.deleted)
			}
		})
	}
}

// A failed recreation must not leave a configuration naming an object that was
// never created.
func TestAFailedRecreateWritesNothing(t *testing.T) {
	client := &fakeRecreateClient{newID: 19, err: errors.New("GitHub refused")}
	written := config.Config{}
	deps := recreateDeps(t, client, &written)
	deps.writeConfig = func(string, config.Config) error {
		t.Fatal("a failed recreate must not write the configuration")
		return nil
	}
	var stdout, stderr bytes.Buffer

	if code := executeWith(context.Background(), []string{"scale-sets", "recreate", "repo-large",
		"--config", "fleet.json", "--confirm", "recreate-scale-set", "--reason", "stranded"},
		&stdout, &stderr, deps); code != exitFailure {
		t.Fatalf("a refused delete is a failure: %d", code)
	}
}

// A daemon that answers the socket but not the question leaves the bound sets
// unjudged, exactly as an unreachable one does.
func TestAnUnanswerableDaemonLeavesBoundSetsUnjudged(t *testing.T) {
	deps := boundStrandingDeps(t, adminapi.StatusEnvelope{}, nil)
	deps.newClient = func(string, time.Duration) (apiClient, error) {
		return fakeClient{err: errors.New("daemon is not listening")}, nil
	}
	var stdout, stderr bytes.Buffer

	if code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json",
		"--endpoint", ""}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("an unanswered status is not a verdict: %d (%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "bound sets were not judged") {
		t.Fatalf("the missing observation must be named: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), adminapi.DefaultEndpoint()) {
		t.Fatalf("an empty endpoint falls back to the default one: %q", stderr.String())
	}
}

// Every way a recreation can fail reports itself, and none of them may leave a
// configuration naming an object that is not there.
func TestRecreateSurfacesEachFailureWithItsOwnExit(t *testing.T) {
	ambiguous := fleetProvisionConfig()
	second := ambiguous.GitHub.Scopes[0]
	second.Name = "other"
	second.ConfigURL = "https://github.com/owner/other"
	second.Targets = []string{"owner/other"}
	second.ScaleSets = append([]config.ScaleSet(nil), second.ScaleSets...)
	ambiguous.GitHub.Scopes = append(ambiguous.GitHub.Scopes, second)
	ambiguous.Targets = append(ambiguous.Targets, config.Target{Type: "repo", Slug: "owner/other", MaxActive: 4})
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, ambiguous); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		edit func(*dependencies)
		code int
		text string
	}{
		"ambiguous name": {edit: func(d *dependencies) {
			d.openConfig = func(string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(encoded.String())), nil
			}
		}, code: exitUnsafe, text: "--scope"},
		"the same id back": {edit: func(d *dependencies) {
			d.openRecreate = func(githubscaleset.GitHubAppAdminConfig) (provision.Recreater, error) {
				return &fakeRecreateClient{newID: 17}, nil
			}
		}, code: exitUnsafe},
		"nothing to recreate": {edit: func(d *dependencies) {
			cfg := fleetProvisionConfig()
			cfg.GitHub = config.GitHub{}
			var empty bytes.Buffer
			if err := config.Encode(&empty, cfg); err != nil {
				t.Fatal(err)
			}
			d.openConfig = func(string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(empty.String())), nil
			}
		}, code: exitUsage},
		"unreadable config": {edit: func(d *dependencies) {
			d.openConfig = func(string) (io.ReadCloser, error) { return nil, errors.New("permission denied") }
		}, code: exitFailure, text: "open config"},
		"unwritable config": {edit: func(d *dependencies) {
			d.writeConfig = func(string, config.Config) error { return errors.New("read-only file system") }
		}, code: exitFailure, text: "persist config"},
		"stray argument": {edit: func(*dependencies) {}, code: exitUsage},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			deps := recreateDeps(t, &fakeRecreateClient{newID: 19}, nil)
			tt.edit(&deps)
			args := []string{"scale-sets", "recreate", "repo-large", "--config", "fleet.json",
				"--confirm", "recreate-scale-set", "--reason", "stranded"}
			if name == "stray argument" {
				args = append(args, "extra")
			}
			var stdout, stderr bytes.Buffer
			if code := executeWith(context.Background(), args, &stdout, &stderr, deps); code != tt.code {
				t.Fatalf("code = %d, want %d (%s)", code, tt.code, stdout.String()+stderr.String())
			}
			if tt.text != "" && !strings.Contains(stdout.String()+stderr.String(), tt.text) {
				t.Fatalf("the failure must say %q: %q", tt.text, stdout.String()+stderr.String())
			}
		})
	}

	// The real port is wired, and it refuses an empty credential like the others.
	if _, err := defaultDependencies().openRecreate(githubscaleset.GitHubAppAdminConfig{}); err == nil {
		t.Fatal("the real recreater must refuse an empty credential")
	}
}
