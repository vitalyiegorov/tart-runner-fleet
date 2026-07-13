package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

const maxPrivateKeyBytes = 1024 * 1024

// PrivateKeyFile loads a GitHub App key from a local, user-owned mode-0600
// regular file. Open and EffectiveUID are injectable only for deterministic
// failure-path tests; production uses O_NOFOLLOW and the process effective UID.
type PrivateKeyFile struct {
	Open         func(string) (*os.File, error)
	EffectiveUID func() int
}

func (f PrivateKeyFile) Load(ctx context.Context, path string) (*Secret, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("private key file path is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read private key file: %w", err)
	}
	open := f.Open
	if open == nil {
		open = func(name string) (*os.File, error) {
			return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- operator-configured credential path, constrained below.
		}
	}
	file, err := open(path)
	if err != nil {
		return nil, fmt.Errorf("open private key file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect private key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("private key must be a regular file")
	}
	if info.Mode() != 0o600 {
		return nil, errors.New("private key file must use mode 0600")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("private key file ownership is unavailable")
	}
	euid := os.Geteuid()
	if f.EffectiveUID != nil {
		euid = f.EffectiveUID()
	}
	if int(stat.Uid) != euid {
		return nil, errors.New("private key file must be owned by the current user")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPrivateKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private key file: %w", err)
	}
	defer clear(data)
	if len(data) > maxPrivateKeyBytes {
		return nil, errors.New("private key file exceeds size limit")
	}
	value := bytes.TrimSpace(data)
	if len(value) == 0 {
		return nil, errors.New("private key file is empty")
	}
	return &Secret{value: string(value)}, nil
}

// GitHubAppKey selects the non-interactive file source when configured and
// otherwise preserves the existing Keychain behavior. File precedence is
// deliberate: stale Keychain metadata cannot trigger an interactive prompt in
// unattended launchd once privateKeyFile is present.
type GitHubAppKey struct {
	Keychain Keychain
	File     PrivateKeyFile
}

func (l GitHubAppKey) Load(ctx context.Context, service, account, path string) (*Secret, error) {
	if strings.TrimSpace(path) != "" {
		return l.File.Load(ctx, path)
	}
	return l.Keychain.Load(ctx, service, account)
}
