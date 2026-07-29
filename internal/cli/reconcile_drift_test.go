package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/provision"
)

// writeProvisionConfig persists the shared provisioning fixture to a temp file.
func writeProvisionConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Encode(file, fleetProvisionConfig()); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// testDependenciesForProvision supplies a loadable private key so the command
// reaches the provisioner instead of failing on credentials.
func testDependenciesForProvision(t *testing.T) dependencies {
	t.Helper()
	deps := defaultDependencies()
	deps.loadPrivateKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return githubscaleset.NewPrivateKeySecret("pem"), nil
	}
	return deps
}

// Repairing an existing scale set is a strictly larger authority than creating a
// missing one, so it rides on its own flag rather than on the provision
// confirmation. These cases pin that separation: which provisioner the command
// selects, and that the default one is still selected without the flag.

// TestProvisionSelectsAReconcilingProvisionerOnlyWhenAsked proves the opt-in
// actually changes which client is opened. Without it an operator provisioning
// missing scale sets can never mutate an existing one, which is the whole point
// of keeping drift repair behind a separate flag.
func TestProvisionSelectsAReconcilingProvisionerOnlyWhenAsked(t *testing.T) {
	configPath := writeProvisionConfig(t)

	for _, test := range []struct {
		name            string
		args            []string
		wantReconciling bool
	}{
		{name: "default", args: []string{"scale-sets", "provision", "--config", configPath}},
		{name: "opted in", args: []string{"scale-sets", "provision", "--config", configPath, "--reconcile-drift"},
			wantReconciling: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var usedDefault, usedReconciling bool
			deps := testDependenciesForProvision(t)
			deps.openProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
				usedDefault = true
				return &fakeProvisioner{plan: githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetReuse, ID: 3}}, nil
			}
			deps.openReconcilingProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
				usedReconciling = true
				return &fakeProvisioner{plan: githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetUpdate, ID: 3}}, nil
			}

			var stdout, stderr bytes.Buffer
			if code := executeWith(context.Background(), test.args, &stdout, &stderr, deps); code != exitSuccess {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if usedReconciling != test.wantReconciling || usedDefault == test.wantReconciling {
				t.Fatalf("reconciling=%v default=%v, want reconciling=%v", usedReconciling, usedDefault, test.wantReconciling)
			}
		})
	}
}

// TestProvisionReportsAnUpdatePlan proves a drift repair is visible in the plan
// before anything is applied, so an operator sees what would change.
func TestProvisionReportsAnUpdatePlan(t *testing.T) {
	configPath := writeProvisionConfig(t)
	deps := testDependenciesForProvision(t)
	deps.openReconcilingProvision = func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
		return &fakeProvisioner{plan: githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetUpdate, ID: 7}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := executeWith(context.Background(),
		[]string{"scale-sets", "provision", "--config", configPath, "--reconcile-drift"}, &stdout, &stderr, deps)
	if code != exitSuccess {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), string(githubscaleset.ScaleSetUpdate)) {
		t.Fatalf("plan did not report the update action:\n%s", stdout.String())
	}
}

// TestDefaultDependenciesBuildBothProvisioners proves the production wiring
// supplies a reconciling constructor as well as the default one, and that both
// refuse an unusable admin configuration rather than returning a client that
// would fail later against GitHub.
func TestDefaultDependenciesBuildBothProvisioners(t *testing.T) {
	deps := defaultDependencies()
	if deps.openProvision == nil || deps.openReconcilingProvision == nil {
		t.Fatal("production wiring is missing a provisioner constructor")
	}
	for name, open := range map[string]func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error){
		"default": deps.openProvision, "reconciling": deps.openReconcilingProvision,
	} {
		if _, err := open(githubscaleset.GitHubAppAdminConfig{}); err == nil {
			t.Fatalf("%s provisioner accepted an empty admin configuration", name)
		}
	}
}
