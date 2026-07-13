package githubscaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions/scaleset"
)

type PrivateKeySecret struct{ secret *JITSecret }

func NewPrivateKeySecret(pem string) *PrivateKeySecret {
	return &PrivateKeySecret{secret: NewJITSecret(pem)}
}
func (s *PrivateKeySecret) reveal() string {
	if s == nil || s.secret == nil {
		return ""
	}
	return s.secret.Reveal()
}
func (s *PrivateKeySecret) Destroy() {
	if s != nil && s.secret != nil {
		s.secret.Destroy()
	}
}
func (*PrivateKeySecret) String() string       { return "[REDACTED]" }
func (*PrivateKeySecret) GoString() string     { return "[REDACTED]" }
func (*PrivateKeySecret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }
func (*PrivateKeySecret) MarshalJSON() ([]byte, error) {
	return nil, errors.New("private key must not be persisted")
}
func (*PrivateKeySecret) MarshalText() ([]byte, error) {
	return nil, errors.New("private key must not be persisted")
}
func (*PrivateKeySecret) MarshalBinary() ([]byte, error) {
	return nil, errors.New("private key must not be persisted")
}

type GitHubAppScaleSetConfig struct {
	GitHubConfigURL                       string
	ClientID                              string
	InstallationID                        int64
	PrivateKey                            *PrivateKeySecret
	ScaleSetID                            int
	Owner                                 string
	MaxCapacity                           int
	InitialCursor                         int
	PollTimeout                           time.Duration
	RequestTimeout                        time.Duration
	System, Version, CommitSHA, Subsystem string
}

var openOfficial = func(ctx context.Context, c GitHubAppScaleSetConfig) (officialMessages, officialJIT, error) {
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: c.GitHubConfigURL,
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: c.ClientID, InstallationID: c.InstallationID, PrivateKey: c.PrivateKey.reveal()},
		SystemInfo:      scaleset.SystemInfo{System: c.System, Version: c.Version, CommitSHA: c.CommitSHA, ScaleSetID: c.ScaleSetID, Subsystem: c.Subsystem},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create official GitHub App client: %w", err)
	}
	session, err := client.MessageSessionClient(ctx, c.ScaleSetID, c.Owner)
	if err != nil {
		return nil, nil, fmt.Errorf("create official message session: %w", err)
	}
	return session, client, nil
}

// NewGitHubAppScaleSet is the only production construction path touching the
// preview client. The private key remains behind a redacting wrapper.
func NewGitHubAppScaleSet(ctx context.Context, c GitHubAppScaleSetConfig) (*ScaleSet, error) {
	if c.PrivateKey == nil || c.PrivateKey.reveal() == "" {
		return nil, errors.New("GitHub App private key is required")
	}
	timeout := c.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	messages, jit, err := openOfficial(ctx, c)
	if err != nil {
		return nil, err
	}
	runners, _ := jit.(officialRunners)
	s, err := NewScaleSet(ScaleSetConfig{Messages: messages, JIT: jit, Runners: runners, ScaleSetID: c.ScaleSetID, MaxCapacity: c.MaxCapacity, InitialCursor: c.InitialCursor, PollTimeout: c.PollTimeout, RequestTimeout: c.RequestTimeout})
	if err != nil {
		if closer, ok := messages.(interface{ Close(context.Context) error }); ok {
			_ = closer.Close(ctx)
		}
		return nil, err
	}
	return s, nil
}
