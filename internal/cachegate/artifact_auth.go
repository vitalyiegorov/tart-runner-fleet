package cachegate

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

const artifactIssuer = "https://token.actions.githubusercontent.com"

var errArtifactAuth = errors.New("artifact authorization refused")

// artifactPrincipal is derived only from a verified runtime token, never RPC IDs.
type artifactPrincipal struct{ repo, run, job string }
type artifactClaims struct {
	jwt.RegisteredClaims
	Repository string `json:"repository_id"`
	Scope      string `json:"scp"`
}

// The parser's global clock is deliberately not used; the verifier owns time.
func (*artifactClaims) Valid() error { return nil }

type artifactVerifier struct {
	mu              sync.Mutex
	url             string
	client          *http.Client
	clock           func() time.Time
	keys            map[string]*rsa.PublicKey
	fetched         time.Time
	loaded          time.Time
	rotationRefresh bool
}

func (v *artifactVerifier) verify(ctx context.Context, auth string) (artifactPrincipal, error) {
	raw, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || len(raw) > 32768 {
		return artifactPrincipal{}, errArtifactAuth
	}
	claims := &artifactClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}))
	token, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	})
	at := v.clock()
	if err != nil || !token.Valid || claims.Issuer != artifactIssuer || claims.ExpiresAt == nil || !at.Before(claims.ExpiresAt.Time) || claims.NotBefore != nil && at.Before(claims.NotBefore.Time) || claims.Repository == "" {
		return artifactPrincipal{}, errArtifactAuth
	}
	p := artifactPrincipal{repo: claims.Repository}
	count := 0
	for _, scope := range strings.Fields(claims.Scope) {
		if !strings.HasPrefix(scope, "Actions.Results:") {
			continue
		}
		parts := strings.Split(scope, ":")
		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			return artifactPrincipal{}, errArtifactAuth
		}
		p.run, p.job = parts[1], parts[2]
		count++
	}
	if count != 1 {
		return artifactPrincipal{}, errArtifactAuth
	}
	return p, nil
}

func (v *artifactVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	at := v.clock()
	if key := v.keys[kid]; key != nil && at.Before(v.loaded.Add(10*time.Minute)) {
		return key, nil
	}
	if kid == "" {
		return nil, errArtifactAuth
	}
	if !v.fetched.IsZero() && at.Before(v.fetched.Add(30*time.Second)) {
		if len(v.keys) == 0 || v.keys[kid] != nil || v.rotationRefresh {
			return nil, errArtifactAuth
		}
		v.rotationRefresh = true // one extra fetch per window, never one per attacker-selected kid
	} else {
		v.rotationRefresh = false
	}
	v.fetched = at // failed refreshes are bounded too; old keys expire, never extend on failure
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return nil, errArtifactAuth
	}
	res, err := v.client.Do(req)
	if err != nil {
		return nil, errArtifactAuth
	}
	defer func() { _ = res.Body.Close() }()
	var doc struct {
		Keys []struct{ Kty, Kid, Alg, Use, N, E string }
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc) != nil || len(doc.Keys) > 100 {
		return nil, errArtifactAuth
	}
	keys := map[string]*rsa.PublicKey{}
	for _, j := range doc.Keys {
		n, en := base64.RawURLEncoding.DecodeString(j.N)
		e, ee := base64.RawURLEncoding.DecodeString(j.E)
		if en != nil || ee != nil || len(n) < 256 || len(n) > 1024 || len(e) > 4 || j.Kty != "RSA" || j.Alg != "" && j.Alg != "RS256" || j.Use != "" && j.Use != "sig" {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent%2 == 0 {
			continue
		}
		keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
	}
	v.keys = keys
	v.loaded = at
	key := keys[kid]
	if key == nil {
		return nil, errArtifactAuth
	}
	return key, nil
}
