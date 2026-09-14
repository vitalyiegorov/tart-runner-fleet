package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
)

type fakeAuditor struct {
	listed  []githubscaleset.ScaleSetSummary
	listErr error
}

func (f *fakeAuditor) List(context.Context, string) ([]githubscaleset.ScaleSetSummary, error) {
	return f.listed, f.listErr
}

func (f *fakeAuditor) Statistics(context.Context, int) (githubscaleset.ScaleSetStatistics, error) {
	return githubscaleset.ScaleSetStatistics{}, nil
}

// auditDeps is a node configured exactly as the mac studio was on 2026-08-04:
// it binds the builder scale set and names nothing else.
func auditDeps(t *testing.T, auditor *fakeAuditor) dependencies {
	t.Helper()
	cfg := fleetProvisionConfig()
	cfg.GitHub.Scopes[0].ScaleSets = cfg.GitHub.Scopes[0].ScaleSets[:1]
	cfg.GitHub.Scopes[0].ScaleSets[0].ID = 1
	cfg.GitHub.Scopes[0].ScaleSets[0].Name = "trf-sudoku-builder"
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
	deps.openAudit = func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return auditor, nil }
	return deps
}

// TestTheAuditNamesAStrandingAndExitsFive is issue #164's Case A end to end at
// the command an operator runs. GitHub holds sets 1 and 7 for the scope; the
// configuration binds only 1; set 7 reports assigned 2 / busy 2.
func TestTheAuditNamesAStrandingAndExitsFive(t *testing.T) {
	deps := auditDeps(t, &fakeAuditor{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{}},
		{ID: 7, Name: "trf-sudoku-builder-studio", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
	}})
	var stdout, stderr bytes.Buffer

	code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json"}, &stdout, &stderr, deps)

	if code != exitDegraded {
		t.Fatalf("a stranding must exit 5: %d (%s)", code, stderr.String())
	}
	for _, want := range []string{"trf-sudoku-builder-studio", "2 assigned job(s)", "nothing is known to be"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("the finding must name %q: %q", want, stderr.String())
		}
	}
	if !strings.Contains(stdout.String(), "bound") || !strings.Contains(stdout.String(), "parked") {
		t.Fatalf("both sets are reported with their state: %q", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "PRIVATE-KEY-SENTINEL") {
		t.Fatal("the App key must never reach an operator surface")
	}
}

// A fleet whose parked sets hold nothing exits 0, and says what it saw. Under
// ADR 0034 a parked set is very often a sibling's, so this is the ordinary
// answer on a federated fleet rather than an unusual one.
func TestAnAuditWithNoStrandingExitsZeroAndStillReports(t *testing.T) {
	deps := auditDeps(t, &fakeAuditor{listed: []githubscaleset.ScaleSetSummary{
		{ID: 9, Name: "trf-sudoku-builder-mini", Statistics: &githubscaleset.ScaleSetStatistics{IdleRunners: 1}},
	}})
	var stdout, stderr bytes.Buffer

	code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json", "--output", "json"},
		&stdout, &stderr, deps)

	if code != exitSuccess || stderr.Len() != 0 {
		t.Fatalf("no stranding exits 0: %d %q", code, stderr.String())
	}
	var sets []scalesetaudit.ScaleSet
	if err := json.Unmarshal(stdout.Bytes(), &sets); err != nil {
		t.Fatalf("the JSON document must decode: %v (%q)", err, stdout.String())
	}
	if len(sets) != 1 || sets[0].State != scalesetaudit.Parked || sets[0].Stranding {
		t.Fatalf("a parked idle set is reported and is not a finding: %#v", sets)
	}
}

// An audit GitHub would not answer exits 4. It must never exit 0: "GitHub did
// not answer" and "no set is parked" are the two states issue #164 was lost
// between, and only one of them is a pass.
func TestAnUnanswerableAuditExitsUnavailable(t *testing.T) {
	deps := auditDeps(t, &fakeAuditor{listErr: errors.New("GitHub is unreachable")})
	var stdout, stderr bytes.Buffer

	if code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json"},
		&stdout, &stderr, deps); code != exitUnavailable {
		t.Fatalf("an unreachable GitHub exits 4: %d", code)
	}

	deps = auditDeps(t, &fakeAuditor{})
	deps.loadPrivateKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return nil, errors.New("keychain refused")
	}
	stdout.Reset()
	stderr.Reset()
	if code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json"},
		&stdout, &stderr, deps); code != exitUnavailable {
		t.Fatalf("a missing credential exits 4: %d", code)
	}
}

// The usual command-surface refusals, so a typo never reads as a clean audit.
func TestTheAuditCommandRefusesAnIllFormedInvocation(t *testing.T) {
	valid := auditDeps(t, &fakeAuditor{})
	tests := []struct {
		name string
		args []string
		edit func(*dependencies)
		code int
		text string
	}{
		{name: "no config", args: []string{"scale-sets", "audit"}, code: exitUsage},
		{name: "bad output", args: []string{"scale-sets", "audit", "--config", "fleet.json", "--output", "yaml"}, code: exitUsage},
		{name: "unknown flag", args: []string{"scale-sets", "audit", "--unknown"}, code: exitUsage},
		{name: "stray argument", args: []string{"scale-sets", "audit", "--config", "fleet.json", "extra"}, code: exitUsage},
		{name: "open config", args: []string{"scale-sets", "audit", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openConfig = func(string) (io.ReadCloser, error) { return nil, errors.New("permission denied") }
		}, code: exitFailure, text: "open config"},
		{name: "decode config", args: []string{"scale-sets", "audit", "--config", "fleet.json"}, edit: func(d *dependencies) {
			d.openConfig = func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("{")), nil }
		}, code: exitFailure, text: "invalid config"},
		{name: "close config", args: []string{"scale-sets", "audit", "--config", "fleet.json"}, edit: func(d *dependencies) {
			source := d.openConfig
			d.openConfig = func(path string) (io.ReadCloser, error) {
				reader, err := source(path)
				if err != nil {
					return nil, err
				}
				return fakeReadCloser{Reader: reader, err: errors.New("close failed")}, nil
			}
		}, code: exitFailure, text: "close config"},
		{name: "no scopes to audit", args: []string{"scale-sets", "audit", "--config", "fleet.json"}, edit: func(d *dependencies) {
			var encoded bytes.Buffer
			cfg := fleetProvisionConfig()
			cfg.GitHub = config.GitHub{}
			if err := config.Encode(&encoded, cfg); err != nil {
				t.Fatal(err)
			}
			d.openConfig = func(string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(encoded.String())), nil
			}
		}, code: exitUsage, text: "audit scale sets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := valid
			if tt.edit != nil {
				tt.edit(&deps)
			}
			var stdout, stderr bytes.Buffer
			code := executeWith(context.Background(), tt.args, &stdout, &stderr, deps)
			if code != tt.code {
				t.Fatalf("code = %d, want %d (%q)", code, tt.code, stdout.String()+stderr.String())
			}
			if tt.text != "" && !strings.Contains(stdout.String()+stderr.String(), tt.text) {
				t.Fatalf("output must say %q: %q", tt.text, stdout.String()+stderr.String())
			}
		})
	}
	var stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"scale-sets", "audit", "--config", "fleet.json", "--output", "json"},
		errorWriter{}, &stderr, valid); code != exitFailure {
		t.Fatalf("an unwritable document is a failure: %d", code)
	}
	if _, err := defaultDependencies().openAudit(githubscaleset.GitHubAppAdminConfig{}); err == nil {
		t.Fatal("the real auditor must refuse an empty credential")
	}
}

// TestTheParkedScaleSetLineDistinguishesThreeStates is what the doctor row must
// never collapse: a daemon that cannot report, a daemon that has not looked, and
// a daemon that looked and found nothing. Issue #164 ran for 4.5 hours under the
// third reading of the second state.
func TestTheParkedScaleSetLineDistinguishesThreeStates(t *testing.T) {
	status := healthyStatus()

	if detail := parkedScaleSetDetail(status.Data, status.Data.EffectiveParkedScaleSetCheck()); detail != "not reported by this daemon" {
		t.Fatalf("an absent check must say so: %q", detail)
	}
	if !status.Data.EffectiveParkedScaleSetCheck().OK {
		t.Fatal("silence must not fail every node during a rolling update")
	}

	passing := adminapi.Check{OK: true}
	status.Data.ParkedScaleSetCheck = &passing
	if detail := parkedScaleSetDetail(status.Data, passing); detail != "not audited" {
		t.Fatalf("a daemon that never audited must say so: %q", detail)
	}

	at := time.Date(2026, 8, 4, 18, 30, 0, 0, time.UTC)
	status.Data.ParkedScaleSetsAuditedAt = &at
	if detail := parkedScaleSetDetail(status.Data, passing); detail != "no parked scale set holds work" {
		t.Fatalf("an audited node says what it found: %q", detail)
	}

	stranded := adminapi.Check{OK: false, Reasons: []string{
		"suuudokuuu scale set 7 (trf-sudoku-builder-studio) is parked here and holds 2 assigned job(s)"}}
	status.Data.ParkedScaleSetCheck = &stranded
	if detail := parkedScaleSetDetail(status.Data, stranded); !strings.Contains(detail, "scale set 7") {
		t.Fatalf("the finding must reach the line: %q", detail)
	}
}
