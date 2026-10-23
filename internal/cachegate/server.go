package cachegate

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// readHeaderTimeout bounds a client that opens a connection and never sends a
// request. There is deliberately no read or write timeout: a cache save or
// restore streams gigabytes and is bounded by the job, not by the gate.
const readHeaderTimeout = 10 * time.Second

// idleTimeout closes a keep-alive connection no request has used for this long.
// A guest's next cache call opens a new one, so nothing waits on it.
const idleTimeout = 2 * time.Minute

// bodyStallTimeout is how long a request body may go without delivering a byte.
// It is a progress bound, not a transfer bound: a multi-gigabyte save that keeps
// sending is never cut, and a client that sends headers and stops is.
var bodyStallTimeout = 60 * time.Second

// refusalLogBurst and refusalLogWindow bound how many refusals are logged.
// Refusals are judged before the cache server verifies a token, so anyone who
// reaches the gate can provoke one; the excess is counted, not written.
const (
	refusalLogBurst  = 60
	refusalLogWindow = time.Minute
)

// now is time.Now; a test replaces it to step the refusal log window.
var now = time.Now

// shutdownGrace is how long a stopping gate lets in-flight transfers finish.
const shutdownGrace = 20 * time.Second

// Handler proxies every request Decide allows to the cache server or to
// GitHub's results service, and answers every other one itself. Refusals are
// logged with the reason, which names the scope; the token is never logged.
func Handler(cache, results *url.URL, logOutput io.Writer) http.Handler {
	logger := slog.New(slog.NewTextHandler(logOutput, nil))
	proxies := map[Upstream]*httputil.ReverseProxy{
		CacheServer: httputil.NewSingleHostReverseProxy(cache),
		Results:     resultsProxy(results),
	}
	limit := &refusalLog{logger: logger}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verdict := Decide(r.URL.Path, r.Header)
		if verdict.Allow {
			r.Body = &progressBody{ReadCloser: r.Body, control: http.NewResponseController(w)}
			proxies[verdict.Upstream].ServeHTTP(w, r)
			return
		}
		if verdict.Status == http.StatusForbidden {
			limit.refused(r.URL.Path, verdict.Reason)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(verdict.Status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": twirpCode(verdict.Status), "msg": verdict.Reason})
	})
}

// refusalLog writes at most refusalLogBurst refusals per refusalLogWindow and
// reports how many it suppressed when the next window opens.
type refusalLog struct {
	logger     *slog.Logger
	mu         sync.Mutex
	window     time.Time
	written    int
	suppressed int
}

func (l *refusalLog) refused(path, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at := now(); !at.Before(l.window.Add(refusalLogWindow)) {
		if l.suppressed > 0 {
			l.logger.Warn("cache write refusals suppressed", "suppressed", l.suppressed, "window", refusalLogWindow)
		}
		l.window, l.written, l.suppressed = at, 0, 0
	}
	if l.written == refusalLogBurst {
		l.suppressed++
		return
	}
	l.written++
	l.logger.Warn("cache write refused", "path", path, "reason", reason)
}

// progressBody extends the connection's read deadline before every read of a
// request body, so a body that stops arriving is cut after bodyStallTimeout
// while one that keeps arriving is never cut.
type progressBody struct {
	io.ReadCloser
	control *http.ResponseController
}

func (b *progressBody) Read(p []byte) (int, error) {
	_ = b.control.SetReadDeadline(time.Now().Add(bodyStallTimeout))
	return b.ReadCloser.Read(p)
}

// newServer is the gate's http.Server: a header deadline, an idle bound, and
// no whole-request read or write timeout (see bodyStallTimeout).
func newServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout}
}

// resultsProxy forwards to GitHub with the Host header GitHub expects; the
// default director would keep the gate's own host:port, which GitHub's front
// end does not route.
func resultsProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = target.Host
	}}
}

func twirpCode(status int) string {
	if status == http.StatusForbidden {
		return "permission_denied"
	}
	return "bad_route"
}

// Execute is `fleet cache-gate`: it serves until ctx is cancelled and returns
// the process exit code (0 on a clean stop, 1 when it cannot serve, 2 on
// usage errors).
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cache-gate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "", "address guests reach the gate on, host:port")
	cacheRaw := flags.String("upstream", "", "absolute URL of the cache server, normally on loopback")
	resultsRaw := flags.String("results-upstream", defaultResults, "absolute URL of GitHub's results service")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	cache, cacheOK := absoluteURL(*cacheRaw)
	results, resultsOK := absoluteURL(*resultsRaw)
	if flags.NArg() != 0 || *listen == "" || !cacheOK || !resultsOK {
		_, _ = fmt.Fprintln(stderr, "usage: fleet cache-gate --listen host:port --upstream http://127.0.0.1:port [--results-upstream url]")
		return 2
	}
	listener, err := listenTCP("tcp", *listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cache-gate: listen: %v\n", err)
		return 1
	}
	server := newServer(Handler(cache, results, stdout))
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	_, _ = fmt.Fprintf(stdout, "cache-gate: serving %s, cache %s, results %s\n", listener.Addr(), cache.Redacted(), results.Redacted())
	select {
	case err := <-served:
		// Serve only returns early when accepting fails; exit non-zero so the
		// supervisor restarts the gate instead of leaving a dead listener.
		_, _ = fmt.Fprintf(stderr, "cache-gate: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	return 0
}

// defaultResults is the results service github.com runners are given.
const defaultResults = "https://results-receiver.actions.githubusercontent.com"

func absoluteURL(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	return parsed, err == nil && parsed.Scheme != "" && parsed.Host != ""
}

// listenTCP is net.Listen; a test replaces it to make accepting fail.
var listenTCP = net.Listen
