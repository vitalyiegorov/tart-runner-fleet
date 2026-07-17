package githubscaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

type InstallationTokenConfig struct {
	APIBaseURL     string
	ClientID       string
	InstallationID int64
	PrivateKey     *PrivateKeySecret
	HTTP           Doer
	Clock          Clock
	Timeout        time.Duration
}

// InstallationTokenSource reuses the daemon's GitHub App authority and keeps
// short-lived installation tokens in memory only. A token is refreshed before
// expiry; concurrent observers share one refresh.
type InstallationTokenSource struct {
	base           *url.URL
	clientID       string
	installationID int64
	privateKey     *PrivateKeySecret
	http           Doer
	clock          Clock
	timeout        time.Duration

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewInstallationTokenSource(config InstallationTokenConfig) (*InstallationTokenSource, error) {
	base, err := url.Parse(config.APIBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || strings.TrimSpace(config.ClientID) == "" ||
		config.InstallationID <= 0 || config.PrivateKey == nil || config.PrivateKey.reveal() == "" {
		return nil, errors.New("valid GitHub App installation token configuration is required")
	}
	if config.HTTP == nil {
		config.HTTP = http.DefaultClient
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	return &InstallationTokenSource{base: base, clientID: config.ClientID, installationID: config.InstallationID,
		privateKey: config.PrivateKey, http: config.HTTP, clock: config.Clock, timeout: config.Timeout}, nil
}

func (s *InstallationTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now().UTC()
	if s.token != "" && now.Add(time.Minute).Before(s.expires) {
		return s.token, nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(s.privateKey.reveal()))
	if err != nil {
		return "", errors.New("parse GitHub App private key")
	}
	claims := jwt.RegisteredClaims{Issuer: s.clientID, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))}
	appJWT, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", errors.New("sign GitHub App JWT")
	}
	endpoint := *s.base
	endpoint.Path = strings.TrimRight(s.base.Path, "/") + "/app/installations/" + strconv.FormatInt(s.installationID, 10) + "/access_tokens"
	endpoint.RawQuery = ""
	requestCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create installation token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("request installation token: GitHub status %d", resp.StatusCode)
	}
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode installation token: %w", err)
	}
	if payload.Token == "" || !payload.ExpiresAt.After(now) {
		return "", errors.New("invalid installation token response")
	}
	s.token, s.expires = payload.Token, payload.ExpiresAt.UTC()
	return s.token, nil
}

func APIBaseURL(configURL string) (string, error) {
	parsed, err := url.Parse(configURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("valid GitHub configuration URL is required")
	}
	if strings.EqualFold(parsed.Hostname(), "github.com") {
		return "https://api.github.com", nil
	}
	return parsed.Scheme + "://" + parsed.Host + "/api/v3", nil
}
