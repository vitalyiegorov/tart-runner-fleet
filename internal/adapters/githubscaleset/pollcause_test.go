package githubscaleset_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
)

// storeRefusal is the shape the SQLite driver's error has: a numeric result code
// and nothing the adapter may render.
type storeRefusal struct{ code int }

func (e storeRefusal) Error() string { return fmt.Sprintf("database is locked (%d)", e.code) }
func (e storeRefusal) Code() int     { return e.code }

// timeoutError is a net.Error that timed out, as a dial or read deadline does.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func sessionPollFailure(cause error) error {
	return &githubscaleset.SessionFailure{Reason: githubscaleset.ReasonMessagePollFailed, Cause: fmt.Errorf("poll message: %w", cause)}
}

// TestIssue468AStatuslessPollFailureNamesItsTransportCause replays the flood
// that persisted after #465: every node logged message_poll_failed with no
// status, so a timeout, a reset, an HTTP/2 stream error and a store refusal were
// one indistinguishable line. Each must now name a closed cause, derived from
// the error's type and never from its text.
func TestIssue468AStatuslessPollFailureNamesItsTransportCause(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a context deadline", context.DeadlineExceeded, githubscaleset.CauseTimeout},
		{"a net.Error timeout", &net.OpError{Op: "read", Net: "tcp", Err: timeoutError{}}, githubscaleset.CauseTimeout},
		{"an OS deadline", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, githubscaleset.CauseTimeout},
		{"a cancellation", context.Canceled, githubscaleset.CauseCanceled},
		{"a connection reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, githubscaleset.CauseReset},
		{"a broken pipe", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}, githubscaleset.CauseReset},
		{"a refused dial", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, githubscaleset.CauseRefused},
		{"an unexpected EOF", io.ErrUnexpectedEOF, githubscaleset.CauseEOF},
		{"an EOF", io.EOF, githubscaleset.CauseEOF},
		{"a DNS failure", &net.DNSError{Err: "no such host", Name: "broker.actions.githubusercontent.com"}, githubscaleset.CauseDNS},
		{"an untrusted certificate", x509.UnknownAuthorityError{}, githubscaleset.CauseTLS},
		{"a TLS record error", tls.RecordHeaderError{Msg: "bad record"}, githubscaleset.CauseTLS},
		{"a busy store", storeRefusal{code: 5}, githubscaleset.CauseStoreBusy},
		{"a stale store snapshot", storeRefusal{code: 517}, githubscaleset.CauseStoreBusy},
		{"anything else", errors.New("decode broker message"), githubscaleset.CauseOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := githubscaleset.DescribeIngestFailure(sessionPollFailure(tc.err))
			if failure.Reason != githubscaleset.ReasonMessagePollFailed || failure.Status != 0 {
				t.Fatalf("failure = %#v, want a statusless poll failure", failure)
			}
			if failure.Cause != tc.want || !githubscaleset.ValidPollCause(failure.Cause) {
				t.Fatalf("cause = %q, want %q", failure.Cause, tc.want)
			}
		})
	}
}

// TestIssue468AnHTTP2StreamResetIsNamed drives a real HTTP/2 exchange whose
// server aborts the stream, the `stream error ... INTERNAL_ERROR; received from
// peer` shape seen from GitHub's infrastructure on 2026-11-02. Its type is
// unexported in net/http, so this proves the classifier on the genuine article.
func TestIssue468AnHTTP2StreamResetIsNamed(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err == nil {
		_, err = io.ReadAll(response.Body)
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("the aborted stream must fail the read")
	}
	if got := githubscaleset.DescribeIngestFailure(sessionPollFailure(err)).Cause; got != githubscaleset.CauseHTTP2 {
		t.Fatalf("cause = %q for %T, want %q", got, errors.Unwrap(err), githubscaleset.CauseHTTP2)
	}
}

// TestIssue468TheCauseIsOnlyForFailuresWithoutAResponse keeps the new field off
// failures that already carry GitHub's answer, off non-transport reasons, and
// off success, and keeps the vocabulary closed and the text unrendered.
func TestIssue468TheCauseIsOnlyForFailuresWithoutAResponse(t *testing.T) {
	answered := githubscaleset.DescribeIngestFailure(&githubscaleset.APIError{Kind: githubscaleset.Unexpected, Status: http.StatusBadRequest, Cause: io.EOF})
	if answered.Status != http.StatusBadRequest || answered.Cause != "" {
		t.Fatalf("a failure GitHub answered must name its status, not a transport cause: %#v", answered)
	}
	conflict := githubscaleset.DescribeIngestFailure(&githubscaleset.SessionFailure{Reason: githubscaleset.ReasonDemandCommitConflict, Cause: io.EOF})
	if conflict.Cause != "" {
		t.Fatalf("a store conflict is not a transport failure: %#v", conflict)
	}
	release := githubscaleset.DescribeIngestFailure(&githubscaleset.SessionFailure{Reason: githubscaleset.ReasonSessionReleaseFailed, Cause: context.DeadlineExceeded})
	if release.Cause != githubscaleset.CauseTimeout {
		t.Fatalf("a statusless release failure names its cause too: %#v", release)
	}
	if githubscaleset.DescribeIngestFailure(nil).Cause != "" {
		t.Fatal("success has no cause")
	}
	if githubscaleset.ValidPollCause("i/o timeout") || githubscaleset.ValidPollCause("") {
		t.Fatal("only the closed vocabulary is valid")
	}
	secret := githubscaleset.DescribeIngestFailure(sessionPollFailure(errors.New("Bearer ghs_secret"))).Cause
	if strings.Contains(secret, "ghs") || secret != githubscaleset.CauseOther {
		t.Fatalf("the cause must never carry error text: %q", secret)
	}
}
