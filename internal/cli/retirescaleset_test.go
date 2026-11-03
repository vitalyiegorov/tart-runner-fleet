package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/provision"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
)

// fakeRetireClient is one GitHub tenant: what it lists and counts, and what it
// was asked to delete.
type fakeRetireClient struct {
	sets    []githubscaleset.ScaleSetSummary
	stats   githubscaleset.ScaleSetStatistics
	deleted []int
}

func (f *fakeRetireClient) List(context.Context, string) ([]githubscaleset.ScaleSetSummary, error) {
	return f.sets, nil
}

func (f *fakeRetireClient) Statistics(context.Context, int) (githubscaleset.ScaleSetStatistics, error) {
	return f.stats, nil
}

func (f *fakeRetireClient) Delete(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// retireDeps is a node whose `repo` scope has moved from owner/repo to the owner
// organization, with the org-level `repo-large` provisioned as id 31; the old
// repo-level set, id 23, is still registered at owner/repo.
func retireDeps(t *testing.T, current, from *fakeRetireClient) dependencies {
	t.Helper()
	cfg := fleetProvisionConfig()
	cfg.GitHub.Scopes[0].Kind = config.ScopeOrganization
	cfg.GitHub.Scopes[0].ConfigURL = "https://github.com/owner"
	for index, set := range cfg.GitHub.Scopes[0].ScaleSets {
		if set.Name == "repo-large" {
			cfg.GitHub.Scopes[0].ScaleSets[index].ID = 31
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
	deps.openAudit = func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return current, nil }
	deps.openRetire = func(githubscaleset.GitHubAppAdminConfig) (provision.Retirer, error) { return from, nil }
	deps.writeConfig = func(string, config.Config) error {
		t.Fatal("retire must never write the configuration")
		return nil
	}
	return deps
}

func retireClients() (*fakeRetireClient, *fakeRetireClient) {
	return &fakeRetireClient{sets: []githubscaleset.ScaleSetSummary{{ID: 31, Name: "repo-large"}}},
		&fakeRetireClient{sets: []githubscaleset.ScaleSetSummary{{ID: 23, Name: "repo-large"}}}
}

func retireArgs(extra ...string) []string {
	return append([]string{"scale-sets", "retire", "repo-large", "--config", "fleet.json", "--scope", "repo",
		"--from", "https://github.com/owner/repo"}, extra...)
}

// TestRetireDeletesTheOldSetAndReportsIt is issue #475: once the org-level
// replacement is bound and the old repo-level set holds nothing, one guarded
// command removes the old one and says exactly what it removed.
func TestRetireDeletesTheOldSetAndReportsIt(t *testing.T) {
	current, from := retireClients()
	var stdout, stderr bytes.Buffer
	code := executeWith(context.Background(), retireArgs("--confirm", "retire-scale-set", "--reason", "repo to org, #475"),
		&stdout, &stderr, retireDeps(t, current, from))
	if code != exitSuccess {
		t.Fatalf("code = %d (%s)", code, stderr.String())
	}
	if len(from.deleted) != 1 || from.deleted[0] != 23 || len(current.deleted) != 0 {
		t.Fatalf("deleted from=%v current=%v, want only 23 at the old scope", from.deleted, current.deleted)
	}
	if got, want := stdout.String(), "repo\trepo-large\thttps://github.com/owner/repo\t23 retired\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String()+stderr.String(), "PRIVATE-KEY-SENTINEL") {
		t.Fatal("the App key must never reach an operator surface")
	}
}

func TestRetireRefusesEachUnsafeRequestWithItsOwnExit(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		edit func(current, from *fakeRetireClient)
		code int
		text string
	}{
		{name: "no confirmation", args: retireArgs("--reason", "r"), code: exitUnsafe, text: "retire-scale-set"},
		{name: "wrong confirmation", args: retireArgs("--confirm", "recreate-scale-set", "--reason", "r"), code: exitUnsafe},
		{name: "blank reason", args: retireArgs("--confirm", "retire-scale-set", "--reason", " "), code: exitUnsafe},
		{name: "no set named", args: []string{"scale-sets", "retire", "--config", "fleet.json"}, code: exitUsage},
		{name: "stray argument", args: retireArgs("--confirm", "retire-scale-set", "--reason", "r", "extra"), code: exitUsage},
		{name: "no scope", args: []string{"scale-sets", "retire", "repo-large", "--config", "fleet.json",
			"--from", "https://github.com/owner/repo", "--confirm", "retire-scale-set", "--reason", "r"}, code: exitUsage},
		{name: "invalid from", args: []string{"scale-sets", "retire", "repo-large", "--config", "fleet.json", "--scope", "repo",
			"--from", "http://github.com/owner/repo", "--confirm", "retire-scale-set", "--reason", "r"}, code: exitUsage},
		{name: "from is the bound scope", args: []string{"scale-sets", "retire", "repo-large", "--config", "fleet.json",
			"--scope", "repo", "--from", "https://github.com/owner", "--confirm", "retire-scale-set", "--reason", "r"},
			code: exitUnsafe},
		{name: "old set absent", args: retireArgs("--confirm", "retire-scale-set", "--reason", "r"),
			edit: func(_, from *fakeRetireClient) { from.sets = nil }, code: exitNotFound},
		{name: "old set busy", args: retireArgs("--confirm", "retire-scale-set", "--reason", "r"),
			edit: func(_, from *fakeRetireClient) { from.stats.AssignedJobs = 1 }, code: exitUnsafe},
		{name: "replacement missing", args: retireArgs("--confirm", "retire-scale-set", "--reason", "r"),
			edit: func(current, _ *fakeRetireClient) { current.sets = nil }, code: exitUnsafe},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current, from := retireClients()
			if tt.edit != nil {
				tt.edit(current, from)
			}
			var stdout, stderr bytes.Buffer
			if code := executeWith(context.Background(), tt.args, &stdout, &stderr, retireDeps(t, current, from)); code != tt.code {
				t.Fatalf("code = %d, want %d (%s)", code, tt.code, stdout.String()+stderr.String())
			}
			if tt.text != "" && !strings.Contains(stderr.String(), tt.text) {
				t.Fatalf("stderr must say %q: %q", tt.text, stderr.String())
			}
			if len(from.deleted)+len(current.deleted) != 0 {
				t.Fatalf("a refusal must delete nothing: %v %v", from.deleted, current.deleted)
			}
		})
	}
}

func TestRetireReportsAnUnreadableConfigAndAFailedOpen(t *testing.T) {
	current, from := retireClients()
	deps := retireDeps(t, current, from)
	deps.openConfig = func(string) (io.ReadCloser, error) { return nil, errors.New("permission denied") }
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), retireArgs("--confirm", "retire-scale-set", "--reason", "r"),
		&stdout, &stderr, deps); code != exitFailure {
		t.Fatalf("code = %d, want failure", code)
	}
	deps = retireDeps(t, current, from)
	deps.openRetire = func(githubscaleset.GitHubAppAdminConfig) (provision.Retirer, error) {
		return nil, errors.New("handshake refused")
	}
	if code := executeWith(context.Background(), retireArgs("--confirm", "retire-scale-set", "--reason", "r"),
		&stdout, &stderr, deps); code != exitFailure || !strings.Contains(stderr.String(), "retire scale set") {
		t.Fatalf("code = %d (%s), want failure", code, stderr.String())
	}
	if _, err := defaultDependencies().openRetire(githubscaleset.GitHubAppAdminConfig{}); err == nil {
		t.Fatal("the real retirer must refuse an empty credential")
	}
}
