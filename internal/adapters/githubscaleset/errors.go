package githubscaleset

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ErrorKind string

const (
	Authentication ErrorKind = "authentication"
	Authorization  ErrorKind = "authorization"
	NotFound       ErrorKind = "not_found"
	Conflict       ErrorKind = "conflict"
	Validation     ErrorKind = "validation"
	RateLimited    ErrorKind = "rate_limited"
	Server         ErrorKind = "server"
	Unexpected     ErrorKind = "unexpected"
)

type APIError struct {
	Kind       ErrorKind
	Status     int
	RequestID  string
	RetryAfter time.Duration
	Cause      error
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("github API %s (status %d, request %q): %v", e.Kind, e.Status, e.RequestID, e.Cause)
}
func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func ClassifyResponse(resp *http.Response, now time.Time, cause error) error {
	if resp == nil {
		return cause
	}
	kind := Unexpected
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		kind = Authentication
	case http.StatusForbidden:
		kind = Authorization
	case http.StatusNotFound:
		kind = NotFound
	case http.StatusConflict:
		kind = Conflict
	case http.StatusUnprocessableEntity:
		kind = Validation
	case http.StatusTooManyRequests:
		kind = RateLimited
	default:
		if resp.StatusCode >= 500 {
			kind = Server
		}
	}
	if cause == nil {
		cause = errors.New(http.StatusText(resp.StatusCode))
	}
	return &APIError{Kind: kind, Status: resp.StatusCode, RequestID: resp.Header.Get("X-GitHub-Request-Id"), RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), now), Cause: cause}
}

func ParseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	at, err := http.ParseTime(value)
	if err != nil || !at.After(now) {
		return 0
	}
	return at.Sub(now)
}

type RNG interface{ Float64() float64 }
type RetryPolicy struct {
	Base, Max time.Duration
	RNG       RNG
}

func (p RetryPolicy) Delay(attempt int, err error) (time.Duration, bool) {
	var api *APIError
	if !errors.As(err, &api) {
		return 0, false
	}
	secondaryLimit := api.Kind == Authorization && api.RetryAfter > 0
	if api.Kind != RateLimited && api.Kind != Server && !secondaryLimit {
		return 0, false
	}
	if api.RetryAfter > 0 {
		return api.RetryAfter, true
	}
	base, max := p.Base, p.Max
	if base <= 0 {
		base = time.Second
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	if attempt < 0 {
		attempt = 0
	}
	d := base
	for i := 0; i < attempt && d < max; i++ {
		if d > max/2 {
			d = max
		} else {
			d *= 2
		}
	}
	jitter := .5
	if p.RNG != nil {
		jitter = p.RNG.Float64()
		if jitter < 0 {
			jitter = 0
		}
		if jitter > 1 {
			jitter = 1
		}
	}
	return time.Duration(float64(d) * (.5 + jitter/2)), true
}
