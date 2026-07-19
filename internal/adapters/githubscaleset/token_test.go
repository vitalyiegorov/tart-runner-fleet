package githubscaleset

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func testPrivateKey(t *testing.T) *PrivateKeySecret {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return NewPrivateKeySecret(string(encoded))
}

func TestInstallationTokenSourceSignsCachesAndRefreshes(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	clock := fixedClock(now)
	requests := 0
	doer := doerFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/42/access_tokens" ||
			request.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
			t.Fatalf("request = %s %s headers=%v", request.Method, request.URL, request.Header)
		}
		raw := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		claims := &jwt.RegisteredClaims{}
		if _, _, err := jwt.NewParser().ParseUnverified(raw, claims); err != nil || claims.Issuer != "Iv1.client" {
			t.Fatalf("JWT claims = %#v, %v", claims, err)
		}
		body := `{"token":"installation-token","expires_at":"2026-07-17T13:00:00Z"}`
		return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	key := testPrivateKey(t)
	defer key.Destroy()
	source, err := NewInstallationTokenSource(InstallationTokenConfig{APIBaseURL: "https://api.github.test", ClientID: "Iv1.client",
		InstallationID: 42, PrivateKey: key, HTTP: doer, Clock: clock, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		token, err := source.Token(context.Background())
		if err != nil || token != "installation-token" {
			t.Fatalf("Token() = %q, %v", token, err)
		}
	}
	if requests != 1 {
		t.Fatalf("token refreshes = %d", requests)
	}
}

func TestInstallationTokenSourceFailsClosedAndAPIBaseURL(t *testing.T) {
	if _, err := NewInstallationTokenSource(InstallationTokenConfig{}); err == nil {
		t.Fatal("invalid token source accepted")
	}
	badKey := NewPrivateKeySecret("not-pem")
	source, err := NewInstallationTokenSource(InstallationTokenConfig{APIBaseURL: "https://api.github.test", ClientID: "client",
		InstallationID: 1, PrivateKey: badKey})
	if err != nil {
		t.Fatal(err)
	}
	if token, err := source.Token(context.Background()); err == nil || token != "" {
		t.Fatalf("invalid key token = %q, %v", token, err)
	}
	if got, err := APIBaseURL("https://github.com/o/r"); err != nil || got != "https://api.github.com" {
		t.Fatalf("github.com API = %q, %v", got, err)
	}
	if got, err := APIBaseURL("https://github.example/o/r"); err != nil || got != "https://github.example/api/v3" {
		t.Fatalf("GHES API = %q, %v", got, err)
	}
	if _, err := APIBaseURL("://"); err == nil {
		t.Fatal("invalid configuration URL accepted")
	}
}

func TestInstallationTokenSourcePreservesEnterpriseAPIBasePath(t *testing.T) {
	key := testPrivateKey(t)
	defer key.Destroy()
	doer := doerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v3/app/installations/7/access_tokens" {
			t.Fatalf("enterprise token path = %q", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"token":"t","expires_at":"2026-07-17T13:00:00Z"}`))}, nil
	})
	source, err := NewInstallationTokenSource(InstallationTokenConfig{APIBaseURL: "https://github.example/api/v3",
		ClientID: "client", InstallationID: 7, PrivateKey: key, HTTP: doer,
		Clock: fixedClock(time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationTokenSourceFailureResponsesAndEarlyRefresh(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	key := testPrivateKey(t)
	defer key.Destroy()
	tests := []struct {
		name string
		doer Doer
	}{
		{"transport", doerFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })},
		{"status", doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("denied"))}, nil
		})},
		{"status close", doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: closeErrorBody{Reader: strings.NewReader("denied")}}, nil
		})},
		{"decode", doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{"))}, nil
		})},
		{"invalid payload", doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"token":"","expires_at":"2026-07-17T11:00:00Z"}`))}, nil
		})},
		{"close", doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: closeErrorBody{Reader: strings.NewReader(
				`{"token":"token","expires_at":"2026-07-17T13:00:00Z"}`)}}, nil
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, err := NewInstallationTokenSource(InstallationTokenConfig{APIBaseURL: "https://api.test", ClientID: "client",
				InstallationID: 1, PrivateKey: key, HTTP: test.doer, Clock: fixedClock(now)})
			if err != nil {
				t.Fatal(err)
			}
			if token, err := source.Token(context.Background()); err == nil || token != "" {
				t.Fatalf("Token() = %q, %v", token, err)
			}
		})
	}

	requests := 0
	source, err := NewInstallationTokenSource(InstallationTokenConfig{APIBaseURL: "https://api.test", ClientID: "client",
		InstallationID: 1, PrivateKey: key, Clock: fixedClock(now), HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			requests++
			expires := "2026-07-17T12:00:30Z"
			if requests > 1 {
				expires = "2026-07-17T13:00:00Z"
			}
			return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
				`{"token":"token","expires_at":"` + expires + `"}`))}, nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := source.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 2 {
		t.Fatalf("early refresh requests = %d", requests)
	}
}
