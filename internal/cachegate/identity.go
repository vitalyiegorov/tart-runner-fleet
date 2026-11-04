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

// Verifier asks a gate to sign a fresh nonce with the key a node pinned. An
// answer that does not verify, an error and a timeout all read as not our gate:
// the caller routes guests away from an address it cannot vouch for. A successful
// check says the gate holds the key now, and the traffic that follows is still
// plain HTTP on a LAN the check has just placed us on.
type Verifier struct {
	Gate    *url.URL
	Public  ed25519.PublicKey
	Timeout time.Duration
	Random  io.Reader
}

// Verified reports whether the gate proved its identity.
func (v Verifier) Verified(ctx context.Context) bool {
	if len(v.Public) != ed25519.PublicKeySize {
		return false
	}
	random := v.Random
	if random == nil {
		random = rand.Reader
	}
	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return false
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = verifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := v.Gate.JoinPath(IdentityPath).String() + "?nonce=" + hex.EncodeToString(nonce)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	// No proxy and no redirect: only the address the node pinned may answer.
	client := http.Client{Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request) // #nosec G704 -- the URL is the node's pinned gate.
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	var answer identityAnswer
	return json.Unmarshal(body, &answer) == nil && response.StatusCode == http.StatusOK &&
		ed25519.Verify(v.Public, challenge(nonce), answer.Signature)
}
