package credentials

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubAppKeyUsesSecureFileWithoutTouchingKeychain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(path, []byte("  PRIVATE-KEY-SENTINEL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keychain := &fakeRunner{err: os.ErrPermission}
	secret, err := (GitHubAppKey{Keychain: Keychain{Runner: keychain}}).Load(
		context.Background(), "ignored-service", "ignored-account", path,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer secret.Destroy()
	if secret.Reveal() != "PRIVATE-KEY-SENTINEL" {
		t.Fatal("file credential was not loaded")
	}
	if len(keychain.args) != 0 {
		t.Fatalf("Keychain was invoked despite privateKeyFile precedence: %q", keychain.args)
	}
}

func TestPrivateKeyFileRejectsUnsafeFilesWithoutLeakingContents(t *testing.T) {
	dir := t.TempDir()
	secure := filepath.Join(dir, "secure.pem")
	if err := os.WriteFile(secure, []byte("PRIVATE-KEY-SENTINEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link.pem")
	if err := os.Symlink(secure, symlink); err != nil {
		t.Fatal(err)
	}
	tooOpen := filepath.Join(dir, "open.pem")
	if err := os.WriteFile(tooOpen, []byte("PRIVATE-KEY-SENTINEL"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		file PrivateKeyFile
		want string
	}{
		{name: "blank path", path: " ", want: "required"},
		{name: "missing", path: filepath.Join(dir, "missing.pem"), want: "open private key file"},
		{name: "symlink", path: symlink, want: "open private key file"},
		{name: "directory", path: dir, want: "regular file"},
		{name: "permissions", path: tooOpen, want: "mode 0600"},
		{name: "owner", path: secure, file: PrivateKeyFile{EffectiveUID: func() int { return os.Geteuid() + 1 }}, want: "owned by the current user"},
		{name: "empty", path: empty, want: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.file.Load(context.Background(), tt.path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE-KEY-SENTINEL") {
				t.Fatal("credential contents leaked through error")
			}
		})
	}
}

func TestGitHubAppKeyFallsBackToKeychain(t *testing.T) {
	runner := &fakeRunner{out: []byte("keychain-key")}
	secret, err := (GitHubAppKey{Keychain: Keychain{Runner: runner}}).Load(context.Background(), "service", "account", "")
	if err != nil {
		t.Fatal(err)
	}
	defer secret.Destroy()
	if secret.Reveal() != "keychain-key" || len(runner.args) == 0 {
		t.Fatal("Keychain fallback did not run")
	}
}
