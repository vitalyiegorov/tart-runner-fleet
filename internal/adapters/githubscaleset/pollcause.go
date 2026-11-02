package githubscaleset

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"syscall"
)

// The poll CAUSE vocabulary names why a broker exchange failed without GitHub
// answering at all. It exists because a statusless message_poll_failed was one
// line for a dial timeout, a reset connection, an HTTP/2 stream error and a
// local store refusal alike: on 2026-11-02 every node logged ~35-100 of them an
// hour and the deferred-transaction fix of #465 removed only a fraction, with
// nothing left to say which of the rest dominated (issue #468).
//
// It is closed and derived only from error identity and type, never from error
// text, so it is as safe to render as the reason itself.
const (
	CauseTimeout   = "timeout"
	CauseCanceled  = "canceled"
	CauseReset     = "reset"
	CauseRefused   = "refused"
	CauseEOF       = "eof"
	CauseHTTP2     = "http2"
	CauseTLS       = "tls"
	CauseDNS       = "dns"
	CauseStoreBusy = "store_busy"
	CauseOther     = "other"
)

// ValidPollCause guards the closed cause vocabulary at every rendering site.
func ValidPollCause(cause string) bool {
	switch cause {
	case CauseTimeout, CauseCanceled, CauseReset, CauseRefused, CauseEOF, CauseHTTP2,
		CauseTLS, CauseDNS, CauseStoreBusy, CauseOther:
		return true
	default:
		return false
	}
}

// statuslessReason reports the reasons whose failure can happen before any HTTP
// response: the poll, the session release and create, and the escalation that
// discards a failing session. A store conflict or a REST observation reason is
// not a transport failure and gets no cause.
func statuslessReason(reason string) bool {
	switch reason {
	case ReasonMessagePollFailed, ReasonSessionReleaseFailed, ReasonSessionCreateFailed, ReasonRecreatedAfterFailures:
		return true
	default:
		return false
	}
}

// pollCause classifies a failure that never reached an HTTP response. The order
// is deliberate: a store refusal and a cancellation are this process's own
// doing and win over anything they wrap, and an HTTP/2 framing error is named
// before the generic EOF it may also carry.
func pollCause(err error) string {
	if err == nil {
		return ""
	}
	if storeBusy(err) {
		return CauseStoreBusy
	}
	if errors.Is(err, context.Canceled) {
		return CauseCanceled
	}
	if timedOut(err) {
		return CauseTimeout
	}
	if http2Failure(err) {
		return CauseHTTP2
	}
	if tlsFailure(err) {
		return CauseTLS
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return CauseDNS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return CauseRefused
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return CauseReset
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return CauseEOF
	}
	return CauseOther
}

// storeBusy recognises SQLite's busy family (5, and its extended codes such as
// 517 BUSY_SNAPSHOT) by the driver's numeric code. The adapter cannot import
// the store, and the number is the only part of that error that is safe to read.
func storeBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 5
}

func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) && network.Timeout()
}

func tlsFailure(err error) bool {
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	return errors.As(err, &record) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &authority) || errors.As(err, &invalid) || errors.As(err, &host)
}

// http2Failure recognises net/http's bundled HTTP/2 stream, GOAWAY and
// connection errors, and their golang.org/x/net/http2 originals. The bundled
// types are unexported, so the type's identity (package and name, never the
// message) is the only handle on them.
func http2Failure(err error) bool {
	found := false
	walkErrors(err, func(e error) {
		t := reflect.TypeOf(e)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.PkgPath() + "." + t.Name() {
		case "net/http.http2StreamError", "net/http.http2GoAwayError", "net/http.http2ConnectionError",
			"golang.org/x/net/http2.StreamError", "golang.org/x/net/http2.GoAwayError", "golang.org/x/net/http2.ConnectionError":
			found = true
		}
	})
	return found
}

// walkErrors visits every error in err's tree, following both single and
// joined unwrapping, so a classifier sees a type however deeply it is wrapped.
func walkErrors(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	// This IS the unwrapping errors.As performs, extended to visit every node:
	// switching on the value (not the error interface) inspects one level and
	// recurses, so no wrapped error is missed.
	switch wrapped := any(err).(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			walkErrors(inner, visit)
		}
	case interface{ Unwrap() error }:
		walkErrors(wrapped.Unwrap(), visit)
	}
}
