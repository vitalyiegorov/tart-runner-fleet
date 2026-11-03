package githubscaleset

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestNewRetirerSharesTheAuditorCredentialPath: the retire port is the audit
// client plus a delete by id, built from the same App credential checks.
func TestNewRetirerSharesTheAuditorCredentialPath(t *testing.T) {
	valid := GitHubAppAdminConfig{GitHubConfigURL: "https://github.com/owner/repo", ClientID: "client",
		InstallationID: 7, PrivateKey: NewPrivateKeySecret("pem")}
	retirer, err := NewRetirer(valid)
	if err != nil || retirer.Auditor == nil || retirer.provisioner.Client == nil {
		t.Fatalf("NewRetirer() = %#v, %v", retirer, err)
	}
	invalid := valid
	invalid.InstallationID = 0
	if _, err := NewRetirer(invalid); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("NewRetirer(no installation) error = %v, want invalid", err)
	}
}

func TestRetirerDeletesByIDOnly(t *testing.T) {
	admin := &fakeScaleSetAdmin{}
	retirer := Retirer{provisioner: Provisioner{Client: admin}}
	if err := retirer.Delete(context.Background(), 23); err != nil {
		t.Fatal(err)
	}
	if len(admin.deleted) != 1 || admin.deleted[0] != 23 {
		t.Fatalf("deleted = %v, want [23]", admin.deleted)
	}
	if err := retirer.Delete(context.Background(), 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("Delete(0) error = %v, want invalid", err)
	}
}
