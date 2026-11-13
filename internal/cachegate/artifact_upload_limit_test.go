package cachegate

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

// Both guest-facing handlers must share the owner's buffer budget. Channels
// hold every admitted request inside Read, without allocating large payloads.
func TestArtifactStalledPUTsBoundUploadBuffers(t *testing.T) {
	const limit = 16
	f := newArtifactFixture(t)
	raw := f.create(f.token("123", "r", "j"), "r", "j", "upload") + "&comp=block&blockid=YQ=="
	v := &artifactVerifier{url: f.keys.URL, client: f.keys.Client(), clock: func() time.Time { return f.at }}
	handlers := []http.Handler{f.handler, artifactHandler(f.store, v, f.origin, map[string]bool{"123": true})}
	bodies := make([]*artifactPausedBody, limit)
	finished := make([]chan int, limit)
	var workers sync.WaitGroup
	firstReleased := false
	defer func() {
		for i, body := range bodies {
			if body != nil && (i != 0 || !firstReleased) {
				close(body.release)
			}
		}
		workers.Wait()
	}()
	for i := range limit {
		body := &artifactPausedBody{entered: make(chan struct{}), release: make(chan struct{})}
		bodies[i] = body
		done := make(chan int, 1)
		finished[i] = done
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := httptest.NewRecorder()
			handlers[i%len(handlers)].ServeHTTP(w, httptest.NewRequest(http.MethodPut, raw, body))
			done <- w.Code
		}()
		select {
		case <-body.entered:
		case code := <-done:
			t.Fatalf("PUT %d rejected before the expected capacity: %d", i, code)
		case <-time.After(2 * time.Second):
			t.Fatal("admitted PUT never started reading")
		}
	}
	// Saturation must reject without reading or queueing behind the stalled PUTs.
	var reads atomic.Int64
	results := make(chan int, 8)
	for range cap(results) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := httptest.NewRecorder()
			body := &artifactReadProbe{Reader: bytes.NewBufferString("overflow"), reads: &reads}
			f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, raw, body))
			results <- w.Code
		}()
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for range cap(results) {
		select {
		case code := <-results:
			if code != http.StatusServiceUnavailable {
				t.Fatalf("saturated owner returned %d, want immediate 503", code)
			}
		case <-deadline.C:
			t.Fatal("saturated PUT queued instead of returning 503")
		}
	}
	if reads.Load() != 0 {
		t.Fatal("saturated PUT consumed a body before admission")
	}
	if f.rpc(f.token("123", "r", "j"), "ListArtifacts", artifactBody("r", "j", "")).Code != http.StatusOK {
		t.Fatal("upload saturation blocked RPCs")
	}
	close(bodies[0].release)
	firstReleased = true
	if code := <-finished[0]; code != http.StatusCreated {
		t.Fatalf("admitted PUT failed: %d", code)
	}
	if f.blob(raw, http.MethodPut, []byte("retry")).Code != http.StatusCreated {
		t.Fatal("completed PUT did not release its buffer slot")
	}
}

type artifactReadProbe struct {
	io.Reader
	reads *atomic.Int64
}

func (r *artifactReadProbe) Read(p []byte) (int, error) { r.reads.Add(1); return r.Reader.Read(p) }

func TestArtifactUploadBufferSlotsReleasedAfterFailures(t *testing.T) {
	for _, test := range []struct {
		name, query string
		body        func() io.Reader
		status      int
		prepare     func(*artifactFixture, string)
	}{
		{"bad block id", "&comp=block&blockid=bad!", func() io.Reader { return bytes.NewBufferString("data") }, http.StatusBadRequest, nil},
		{"broken body", "&comp=block&blockid=YQ==", func() io.Reader { return iotest.ErrReader(io.ErrUnexpectedEOF) }, http.StatusBadRequest, nil},
		{"bad block list", "&comp=blocklist", func() io.Reader { return bytes.NewBufferString("<broken>") }, http.StatusBadRequest, nil},
		{"unknown operation", "&comp=unknown", func() io.Reader { return bytes.NewReader(nil) }, http.StatusBadRequest, nil},
		{"disk floor", "&comp=block&blockid=YQ==", func() io.Reader { return bytes.NewBufferString("data") }, http.StatusServiceUnavailable, func(f *artifactFixture, _ string) { f.budget.probe = func() (int64, error) { return 0, nil } }},
		{"deleted record", "&comp=block&blockid=YQ==", func() io.Reader { return bytes.NewBufferString("data") }, http.StatusNotFound, func(f *artifactFixture, auth string) { f.rpc(auth, "DeleteArtifact", artifactBody("r", "j", "upload")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newArtifactFixture(t)
			auth := f.token("123", "r", "j")
			raw := f.create(auth, "r", "j", "upload")
			if test.prepare != nil {
				test.prepare(f, auth)
			}
			for range 17 {
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, raw+test.query, test.body()))
				if w.Code != test.status {
					t.Fatalf("failed PUT leaked a slot: status %d, want %d", w.Code, test.status)
				}
			}
			f.budget.probe = func() (int64, error) { return 1 << 40, nil }
			retry := f.create(auth, "r", "j", "retry") + "&comp=block&blockid=YQ=="
			if f.blob(retry, http.MethodPut, []byte("retry")).Code != http.StatusCreated {
				t.Fatal("error path exhausted upload slots")
			}
		})
	}
}

func TestArtifactFullUploadBuffersRejectWithoutDrainingBody(t *testing.T) {
	f := newArtifactFixture(t)
	raw := f.create(f.token("123", "r", "j"), "r", "j", "upload") + "&comp=block&blockid=YQ=="
	for range cap(f.store.uploadSlots) {
		f.store.uploadSlots <- struct{}{}
	}
	defer func() {
		for range cap(f.store.uploadSlots) {
			<-f.store.uploadSlots
		}
	}()
	gate := httptest.NewServer(f.handler)
	defer gate.Close()
	target, _ := url.Parse(gate.URL)
	path, _ := url.Parse(raw)
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	// A rejected request must receive its response even if no body byte arrives.
	io.WriteString(conn, "PUT "+path.RequestURI()+" HTTP/1.1\r\nHost: gate\r\nContent-Length: 1\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPut})
	if err != nil {
		t.Fatal("busy response waited for an unread request body")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("busy owner did not return 503")
	}
}
