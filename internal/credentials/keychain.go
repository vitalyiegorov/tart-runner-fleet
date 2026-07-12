package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const redactedSecret = "[REDACTED]"

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, binary string, args ...string) ([]byte, error) {
	// #nosec G204 -- the trusted keychain executable is injected without shell expansion.
	return exec.CommandContext(ctx, binary, args...).Output()
}

type Keychain struct {
	Runner  Runner
	Timeout time.Duration
}

func (k Keychain) Load(ctx context.Context, service, account string) (*Secret, error) {
	if strings.TrimSpace(service) == "" || strings.TrimSpace(account) == "" {
		return nil, errors.New("keychain service and account are required")
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner := k.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	output, err := runner.Run(ctx, "/usr/bin/security", "find-generic-password", "-w", "-s", service, "-a", account)
	if err != nil {
		return nil, fmt.Errorf("read keychain credential: %w", err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return nil, errors.New("keychain credential is empty")
	}
	return &Secret{value: value}, nil
}

type Secret struct {
	mu    sync.Mutex
	value string
}

func (s *Secret) Reveal() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}

func (s *Secret) Destroy() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.value = ""
	s.mu.Unlock()
}

func (*Secret) String() string       { return redactedSecret }
func (*Secret) GoString() string     { return redactedSecret }
func (*Secret) LogValue() slog.Value { return slog.StringValue(redactedSecret) }
func (*Secret) MarshalJSON() ([]byte, error) {
	return nil, errors.New("secret serialization is forbidden")
}
func (*Secret) MarshalText() ([]byte, error) {
	return nil, errors.New("secret serialization is forbidden")
}
func (*Secret) MarshalBinary() ([]byte, error) {
	return nil, errors.New("secret serialization is forbidden")
}
