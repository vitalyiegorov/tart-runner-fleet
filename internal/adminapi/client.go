package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const MaxResponseBytes = 1 << 20

var (
	ErrInvalidEndpoint  = errors.New("adminapi: invalid endpoint")
	ErrUnsafeEndpoint   = errors.New("adminapi: endpoint is not local")
	ErrResponse         = errors.New("adminapi: unsuccessful response")
	ErrResponseTooLarge = errors.New("adminapi: response exceeds limit")
	ErrInvalidResponse  = errors.New("adminapi: invalid response")
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(endpoint string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 || timeout > 30*time.Second {
		return nil, ErrInvalidEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, ErrInvalidEndpoint
	}
	switch parsed.Scheme {
	case "unix":
		if parsed.Host != "" || !filepath.IsAbs(parsed.Path) || parsed.Path == "/" {
			return nil, ErrInvalidEndpoint
		}
		socket := filepath.Clean(parsed.Path)
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", socket)
		}}
		return &Client{baseURL: "http://fleetd", http: &http.Client{Timeout: timeout, Transport: transport}}, nil
	case "http":
		if parsed.Path != "" && parsed.Path != "/" {
			return nil, ErrInvalidEndpoint
		}
		ip := net.ParseIP(parsed.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, ErrUnsafeEndpoint
		}
		return &Client{baseURL: strings.TrimSuffix(endpoint, "/"), http: &http.Client{Timeout: timeout}}, nil
	default:
		return nil, ErrInvalidEndpoint
	}
}

func (c *Client) Status(ctx context.Context) (StatusEnvelope, error) {
	body, err := c.get(ctx, StatusPath, "application/json")
	if err != nil {
		return StatusEnvelope{}, err
	}
	var status StatusEnvelope
	if err := json.Unmarshal(body, &status); err != nil || status.APIVersion != APIVersion || status.Kind != "Status" {
		return StatusEnvelope{}, ErrInvalidResponse
	}
	return status, nil
}

func (c *Client) Probe(ctx context.Context, ready bool) (Check, error) {
	path := HealthPath
	if ready {
		path = ReadyPath
	}
	body, err := c.getAllowUnavailable(ctx, path, "application/json")
	if err != nil {
		return Check{}, err
	}
	var wire struct {
		Status  string   `json:"status"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Status == "" {
		return Check{}, ErrInvalidResponse
	}
	ok := wire.Status == "live" || wire.Status == "ready"
	return Check{OK: ok, Reasons: nonNil(wire.Reasons)}, nil
}

func (c *Client) Metrics(ctx context.Context) (string, error) {
	body, err := c.get(ctx, MetricsPath, "text/plain")
	return string(body), err
}

// Discharge performs the one guarded mutation in fleet.v1. The request is
// validated locally first so an unconfirmed or unexplained mutation never leaves
// the operator's machine, and a bounded refusal is returned as a Refusal error so
// the caller can report the daemon's exact reason instead of an HTTP status.
func (c *Client) Discharge(ctx context.Context, request DischargeRequest) (DischargeResult, error) {
	if !request.Valid() {
		return DischargeResult{}, Refusal{Code: RefusalInvalidRequest}
	}
	// DischargeRequest is two strings, a bool, and two more strings, so encoding it
	// cannot fail; there is no error branch to pretend to handle.
	body, _ := json.Marshal(request)
	response, err := c.post(ctx, DischargePath, body)
	if err != nil {
		return DischargeResult{}, err
	}
	var result DischargeResult
	if err := json.Unmarshal(response, &result); err != nil || result.APIVersion != APIVersion || result.Kind != DischargeKind {
		return DischargeResult{}, ErrInvalidResponse
	}
	return result, nil
}

func (c *Client) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := readBounded(response)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, refusalFrom(payload, response.StatusCode)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return nil, ErrInvalidResponse
	}
	return payload, nil
}

// readBounded reads at most MaxResponseBytes. Both the read-only and the mutating
// path share it so neither can drift into an unbounded read.
func readBounded(response *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

// refusalFrom converts an unsuccessful mutation response into a bounded Refusal.
// A body that does not carry a known refusal code is reported as an unsuccessful
// response rather than being echoed, so an unexpected error page can never be
// printed to an operator as if it were a fleet decision.
func refusalFrom(payload []byte, status int) error {
	var wire struct {
		Kind string `json:"kind"`
		Code string `json:"code"`
	}
	if json.Unmarshal(payload, &wire) == nil && wire.Kind == RefusalKind && ValidRefusalCode(wire.Code) {
		return Refusal{Code: wire.Code}
	}
	return fmt.Errorf("%w: HTTP %d", ErrResponse, status)
}

func (c *Client) get(ctx context.Context, path, contentType string) ([]byte, error) {
	return c.do(ctx, path, contentType, false)
}

func (c *Client) getAllowUnavailable(ctx context.Context, path, contentType string) ([]byte, error) {
	return c.do(ctx, path, contentType, true)
}

func (c *Client) do(ctx context.Context, path, contentType string, allowUnavailable bool) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", contentType)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	allowed := response.StatusCode >= 200 && response.StatusCode < 300
	if allowUnavailable && response.StatusCode == http.StatusServiceUnavailable {
		allowed = true
	}
	if !allowed {
		return nil, fmt.Errorf("%w: HTTP %d", ErrResponse, response.StatusCode)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), strings.ToLower(contentType)) {
		return nil, ErrInvalidResponse
	}
	return readBounded(response)
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
