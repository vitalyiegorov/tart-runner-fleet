package cachegate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type artifactDownloadContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *artifactDownloadContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type artifactStalledDownload struct {
	header                   http.Header
	entered, release         chan struct{}
	enteredOnce, releaseOnce sync.Once
	mode                     string
	status                   int
}

func (w *artifactStalledDownload) Header() http.Header    { return w.header }
func (w *artifactStalledDownload) WriteHeader(status int) { w.status = status }
func (w *artifactStalledDownload) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.header.Get("Content-Type") == "application/zip" {
		w.enteredOnce.Do(func() { close(w.entered) })
		<-w.release
		if w.mode == "error" {
			return 0, io.ErrClosedPipe
		}
		if w.mode == "panic" {
			panic(http.ErrAbortHandler)
		}
	}
	return len(p), nil
}
func (w *artifactStalledDownload) finish() { w.releaseOnce.Do(func() { close(w.release) }) }

type artifactDownloadResult struct {
	status     int
	panicValue any
}
type artifactDownloadRequest struct {
	writer  *artifactStalledDownload
	waiting chan struct{}
	cancel  context.CancelFunc
	done    chan artifactDownloadResult
}
type artifactDownloadFixture struct {
	f        *artifactFixture
	handlers []http.Handler
	url      string
	requests []*artifactDownloadRequest
	workers  sync.WaitGroup
}

func newArtifactDownloadFixture(t *testing.T, mode string) *artifactDownloadFixture {
	t.Helper()
	f := newArtifactFixture(t)
	f.upload(f.token("123", "r", "j"), "r", "j", "download", []byte("download bytes"))
	a, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "download")
	if err != nil {
		t.Fatal(err)
	}
	v := &artifactVerifier{url: f.keys.URL, client: f.keys.Client(), clock: func() time.Time { return f.at }}
	d := &artifactDownloadFixture{f: f, url: f.store.signedURL(f.origin, a, "read"), handlers: []http.Handler{f.handler, artifactHandler(f.store, v, f.origin, map[string]bool{"123": true})}}
	t.Cleanup(func() {
		for _, r := range d.requests {
			r.cancel()
			r.writer.finish()
		}
		d.workers.Wait()
	})
	for i := range 16 {
		m := ""
		if i == 0 {
			m = mode
		}
		r := d.start(m)
		d.expectWriting(t, r)
	}
	return d
}
func (d *artifactDownloadFixture) start(mode string) *artifactDownloadRequest {
	ctx, cancel := context.WithCancel(context.Background())
	observed := &artifactDownloadContext{Context: ctx, waiting: make(chan struct{})}
	w := &artifactStalledDownload{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{}), mode: mode}
	r := &artifactDownloadRequest{writer: w, waiting: observed.waiting, cancel: cancel, done: make(chan artifactDownloadResult, 1)}
	handler := d.handlers[len(d.requests)%len(d.handlers)]
	d.requests = append(d.requests, r)
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer func() { r.done <- artifactDownloadResult{w.status, recover()} }()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, d.url, nil).WithContext(observed))
	}()
	return r
}
func (*artifactDownloadFixture) expectWriting(t *testing.T, r *artifactDownloadRequest) {
	t.Helper()
	select {
	case <-r.writer.entered:
	case result := <-r.done:
		t.Fatalf("download ended before writing: status %d", result.status)
	case <-time.After(2 * time.Second):
		t.Fatal("download never acquired a free buffer")
	}
}
func (*artifactDownloadFixture) expectWaiting(t *testing.T, r *artifactDownloadRequest) {
	t.Helper()
	select {
	case <-r.waiting:
	case <-r.writer.entered:
		t.Fatal("extra GET loaded a block while all download buffers were occupied")
	case result := <-r.done:
		t.Fatalf("extra GET returned %d instead of waiting", result.status)
	case <-time.After(2 * time.Second):
		t.Fatal("GET never reached cancellable admission")
	}
	select {
	case <-r.writer.entered:
		t.Fatal("waiting GET already loaded block bytes")
	case result := <-r.done:
		t.Fatalf("waiting GET returned %d", result.status)
	default:
	}
}
func (*artifactDownloadFixture) result(t *testing.T, r *artifactDownloadRequest) artifactDownloadResult {
	t.Helper()
	select {
	case result := <-r.done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("download did not finish")
		return artifactDownloadResult{}
	}
}

func TestArtifactStalledGETsBoundDownloadBuffers(t *testing.T) {
	d := newArtifactDownloadFixture(t, "")
	extra := d.start("")
	d.expectWaiting(t, extra)
	if !d.f.store.mu.TryLock() {
		t.Fatal("waiting GET holds global store lock")
	}
	d.f.store.mu.Unlock()
	if len(d.f.store.uploadSlots) != 0 {
		t.Fatal("downloads consumed upload slots")
	}
	d.f.upload(d.f.token("123", "other", "j"), "other", "j", "upload", []byte("unrelated"))
	d.requests[0].writer.finish()
	if r := d.result(t, d.requests[0]); r.status != http.StatusOK || r.panicValue != nil {
		t.Fatal("initial download failed")
	}
	d.expectWriting(t, extra)
}
func TestArtifactCancelledDownloadWaiterDoesNotReleaseSlot(t *testing.T) {
	d := newArtifactDownloadFixture(t, "")
	waiter := d.start("")
	d.expectWaiting(t, waiter)
	waiter.cancel()
	if r := d.result(t, waiter); r.status != http.StatusRequestTimeout || r.panicValue != nil {
		t.Fatal("cancelled waiter did not exit without streaming")
	}
	if len(d.f.store.downloadSlots) != 16 {
		t.Fatal("cancelled waiter released a slot it never acquired")
	}
	next := d.start("")
	d.expectWaiting(t, next)
	d.requests[0].writer.finish()
	d.result(t, d.requests[0])
	d.expectWriting(t, next)
}
func TestArtifactDownloadSlotReleasedOnCompletionOrAbort(t *testing.T) {
	for _, mode := range []string{"complete", "error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			d := newArtifactDownloadFixture(t, mode)
			next := d.start("")
			d.expectWaiting(t, next)
			d.requests[0].writer.finish()
			result := d.result(t, d.requests[0])
			if mode == "complete" {
				if result.panicValue != nil {
					t.Fatal("normal completion panicked")
				}
			} else {
				err, ok := result.panicValue.(error)
				if !ok || !errors.Is(err, http.ErrAbortHandler) {
					t.Fatal("aborted download did not unwind")
				}
			}
			d.expectWriting(t, next)
			next.writer.finish()
			d.result(t, next)
			retry := d.start("")
			d.expectWriting(t, retry)
		})
	}
}
