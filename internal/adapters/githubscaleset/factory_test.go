package githubscaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPrivateKeyRedactionAndFactory(t *testing.T) {
	key := NewPrivateKeySecret("PRIVATE-KEY-MATERIAL")
	if strings.Contains(fmt.Sprintf("%v %#v %+v", key, key, key), "PRIVATE-KEY-MATERIAL") || key.LogValue().String() != "[REDACTED]" {
		t.Fatal("private key formatted")
	}
	if _, err := json.Marshal(key); err == nil {
		t.Fatal("private key persisted")
	}
	if _, err := key.MarshalText(); err == nil {
		t.Fatal("private key text persisted")
	}
	if _, err := key.MarshalBinary(); err == nil {
		t.Fatal("private key binary persisted")
	}
	if (*PrivateKeySecret)(nil).reveal() != "" {
		t.Fatal("nil key")
	}
	(*PrivateKeySecret)(nil).Destroy()
	if _, err := NewGitHubAppScaleSet(context.Background(), GitHubAppScaleSetConfig{}); err == nil {
		t.Fatal("missing key accepted")
	}

	original := openOfficial
	defer func() { openOfficial = original }()
	want := errors.New("open")
	openOfficial = func(context.Context, GitHubAppScaleSetConfig) (officialMessages, officialJIT, error) {
		return nil, nil, want
	}
	if _, err := NewGitHubAppScaleSet(context.Background(), GitHubAppScaleSetConfig{PrivateKey: key}); !errors.Is(err, want) {
		t.Fatalf("open error: %v", err)
	}

	f := &fakeScaleSet{}
	openOfficial = func(ctx context.Context, c GitHubAppScaleSetConfig) (officialMessages, officialJIT, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("factory deadline")
		}
		return f, f, nil
	}
	config := GitHubAppScaleSetConfig{PrivateKey: key, ScaleSetID: 3, MaxCapacity: 2, InitialCursor: 9}
	s, err := NewGitHubAppScaleSet(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if s.LastMessageID() != 9 {
		t.Fatal("initial cursor")
	}
	config.ScaleSetID = 0
	if _, err = NewGitHubAppScaleSet(context.Background(), config); err == nil || f.closed != 1 {
		t.Fatalf("invalid scale set cleanup: %v/%d", err, f.closed)
	}

	openOfficial = original
	_, _, err = original(context.Background(), GitHubAppScaleSetConfig{GitHubConfigURL: "://", PrivateKey: key})
	if err == nil {
		t.Fatal("official client validation")
	}
	_, _, err = original(context.Background(), GitHubAppScaleSetConfig{GitHubConfigURL: "https://github.com/o/r", ClientID: "1", InstallationID: 1, PrivateKey: key, ScaleSetID: 1, Owner: "owner"})
	if err == nil {
		t.Fatal("official session auth validation")
	}
	key.Destroy()
	if key.reveal() != "" {
		t.Fatal("private key destroy failed")
	}
}
