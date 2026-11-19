package cachegate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// IdentityPath is where a gate answers an identity challenge (ADR 0078). It is
// answered before Decide, so a challenge never reaches an upstream.
const IdentityPath = "/.trf/gate-identity"

const (
	nonceBytes = 32
	// identityMessage separates what this key signs from anything else, because
	// any guest can ask the gate to sign a nonce of its choosing.
	identityMessage = "trf-cache-gate-identity-v1:"
	// verifyTimeout bounds a host's check before each guest launch.
	verifyTimeout = 2 * time.Second
)

// LoadOrCreateIdentity returns the gate's signing key, creating it on first use.
// The key never leaves this file: only its public half is printed and pinned.
func LoadOrCreateIdentity(path string) (ed25519.PrivateKey, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- an operator's flag.
	if err == nil {
		defer func() { _ = file.Close() }()
		seed := make([]byte, ed25519.SeedSize)
		_, _ = rand.Read(seed) // Documented never to return an error.
		_, err = file.WriteString(base64.StdEncoding.EncodeToString(seed) + "\n")
		return ed25519.NewKeyFromSeed(seed), err
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- an operator's flag.
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be readable only by its owner", path)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s does not hold a key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKeyString is the form a node's cacheGate.publicKey pins.
func PublicKeyString(public ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(public)
}

// ParsePublicKey reads a pinned public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("not an ed25519 public key")
	}
	return raw, nil
}

func identityHandler(key ed25519.PrivateKey, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != IdentityPath {
			next.ServeHTTP(w, r)
			return
		}
		if key == nil {
			http.NotFound(w, r)
			return
		}
		nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
		if err != nil || len(nonce) != nonceBytes {
			http.Error(w, "bad challenge", http.StatusBadRequest)
			return
		}
		answer, _ := json.Marshal(identityAnswer{Signature: ed25519.Sign(key, challenge(nonce))})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(answer)
	})
}

// identityAnswer is the only thing the endpoint returns. encoding/json carries the
// signature as base64, and no byte of the request appears in it.
type identityAnswer struct {
	Signature []byte `json:"signature"`
}

func challenge(nonce []byte) []byte { return append([]byte(identityMessage), nonce...) }

// Fetch makes one GET to the node's pinned gate and returns the status and at most
// maxIdentityAnswer bytes of the body. It never follows a redirect or uses a proxy.
type Fetch func(ctx context.Context, target string) (status int, body []byte, err error)

// maxIdentityAnswer bounds what a proof reads. A real answer is about 100 bytes.
const maxIdentityAnswer = 1024

// Verifier asks a gate to sign a fresh nonce with the key a node pinned. An
// answer that does not verify, an error and a timeout all read as not our gate:
// the caller routes guests away from an address it cannot vouch for. A successful
// check says the gate holds the key now, and the traffic that follows is still
// plain HTTP on a LAN the check has just placed us on.
//
// Fetch is how the check reaches the gate; nil means Go's own client. A macOS node
// passes CurlFetch, because Local Network privacy refuses an ad-hoc signed launchd
// agent's own LAN connections (issue #537).
type Verifier struct {
	Gate    *url.URL
	Public  ed25519.PublicKey
	Timeout time.Duration
	Random  io.Reader
	Fetch   Fetch
}

// Verified reports whether the gate proved its identity.
func (v Verifier) Verified(ctx context.Context) bool { return v.Prove(ctx) == nil }

// Prove asks the gate for its proof and says why it was refused, so a node can log
// the cause instead of only that the gate did not prove itself.
func (v Verifier) Prove(ctx context.Context) error {
	if len(v.Public) != ed25519.PublicKeySize {
		return errors.New("no pinned public key")
	}
	random := v.Random
	if random == nil {
		random = rand.Reader
	}
	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = verifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := v.Gate.JoinPath(IdentityPath).String() + "?nonce=" + hex.EncodeToString(nonce)
	status, body, err := v.fetch()(ctx, target)
	if err != nil {
		return fmt.Errorf("identity request: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("identity request answered HTTP %d", status)
	}
	var answer identityAnswer
	if json.Unmarshal(body, &answer) != nil {
		return errors.New("identity answer is not a signature")
	}
	if !ed25519.Verify(v.Public, challenge(nonce), answer.Signature) {
		return errors.New("identity signature does not verify against the pinned key")
	}
	return nil
}

func (v Verifier) fetch() Fetch {
	if v.Fetch != nil {
		return v.Fetch
	}
	return directFetch
}
