package cachegate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const artifactService = "/twirp/github.actions.results.api.v1.ArtifactService/"
const artifactBlob = "/.trf/artifacts/blobs/"
const artifactBlockLimit = 16 << 20

// Fixed worst-case weights: sixteen 16 MiB bodies, shared by both owner listeners.
const artifactUploadSlots = 16

const artifactDownloadSlots = 16

func artifactRoute(path string) bool {
	return strings.HasPrefix(path, artifactService) || strings.HasPrefix(path, artifactBlob)
}

// artifactForwarder makes no authorization or storage decision. Its target is
// an operator-configured loopback SSH forward; redirects are never followed.
func artifactForwarder(target *url.URL) http.Handler {
	p := resultsProxy(target)
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { artifactError(w, http.StatusServiceUnavailable) }
	p.ModifyResponse = func(r *http.Response) error {
		if r.StatusCode >= 300 && r.StatusCode < 400 {
			_ = r.Body.Close()
			return errArtifactInput
		}
		return nil
	}
	p.ErrorLog = nil
	p.Transport = &http.Transport{Proxy: nil, ResponseHeaderTimeout: bodyStallTimeout, IdleConnTimeout: idleTimeout}
	return p
}

// artifactHandler is intentionally outside Decide's unsigned cache-ref filter.
func artifactHandler(s *artifactStore, v *artifactVerifier, origin string, repos map[string]bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, artifactBlob) {
			s.blob(w, r)
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, artifactService) {
			artifactError(w, 400)
			return
		}
		p, err := v.verify(r.Context(), r.Header.Get("Authorization"))
		if err != nil || !repos[p.repo] {
			if s.metrics != nil {
				s.metrics.refused.Add(1)
			}
			artifactError(w, 403)
			return
		}
		var req artifactRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || req.Run != p.run || req.Job == "" {
			artifactError(w, 403)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, artifactService)
		if (method == "CreateArtifact" || method == "FinalizeArtifact" || method == "ListArtifacts") && req.Job != p.job {
			artifactError(w, 403)
			return
		}
		response, err := s.handleRPC(p, method, req, origin)
		if err != nil {
			artifactStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
}

type artifactRequest struct {
	Run        string    `json:"workflow_run_backend_id"`
	Job        string    `json:"workflow_job_run_backend_id"`
	Name       string    `json:"name"`
	Version    int       `json:"version"`
	Expires    time.Time `json:"expires_at"`
	Size       string    `json:"size"`
	Hash       string    `json:"hash"`
	NameFilter string    `json:"name_filter"`
	IDFilter   string    `json:"id_filter"`
}

// Request bodies and response writes stay outside the database critical section.
func (s *artifactStore) handleRPC(p artifactPrincipal, method string, r artifactRequest, origin string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if method == "CreateArtifact" {
		release, err := s.reserveWrite()
		if err != nil {
			return nil, err
		}
		defer release()
	}
	return s.rpc(p, method, r, origin)
}

func (s *artifactStore) rpc(p artifactPrincipal, method string, r artifactRequest, origin string) (any, error) {
	if method == "ListArtifacts" {
		return s.listResponse(p, r)
	}
	if len(r.Name) == 0 || len(r.Name) > 255 || strings.ContainsAny(r.Name, "\x00\r\n") {
		return nil, errArtifactInput
	}
	if method == "CreateArtifact" {
		if r.Version != 4 {
			return nil, errArtifactInput
		}
		a, err := s.create(p, r.Name, r.Expires)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "signed_upload_url": s.signedURL(origin, a, "write")}, nil
	}
	a, err := s.named(p, r.Name)
	if err != nil {
		return nil, err
	}
	if a.Job != r.Job || a.Expires <= s.clock().Unix() {
		return nil, errArtifactMissing
	}
	switch method {
	case "FinalizeArtifact":
		return s.finalize(a, r)
	case "GetSignedArtifactURL":
		if !a.Final {
			return nil, errArtifactMissing
		}
		if release, e := s.reserveWrite(); e == nil {
			_, err = s.db.Exec("UPDATE artifacts SET access=? WHERE id=?", s.clock().Unix(), a.ID)
			release()
			if err != nil {
				return nil, err
			}
		}
		return map[string]string{"signed_url": s.signedURL(origin, a, "read")}, nil
	case "DeleteArtifact":
		if s.leases[a.ID] > 0 {
			return nil, errArtifactSpace
		}
		_, err = s.db.Exec("DELETE FROM artifacts WHERE id=?", a.ID)
		return map[string]any{"ok": true, "artifact_id": fmt.Sprint(a.ID)}, err
	default:
		return nil, errArtifactInput
	}
}
func (s *artifactStore) finalize(a artifactRecord, r artifactRequest) (any, error) {
	size, err := strconv.ParseInt(r.Size, 10, 64)
	if err != nil || size < 0 {
		return nil, errArtifactInput
	}
	if a.Final {
		if size != a.Size || r.Hash != "" && r.Hash != a.Hash {
			return nil, errArtifactInput
		}
		return map[string]any{"ok": true, "artifact_id": fmt.Sprint(a.ID)}, nil
	}
	hash := sha256.New()
	actual, err := s.hashUnlocked(a, hash)
	if err != nil {
		return nil, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if size != actual || r.Hash != "" && r.Hash != digest {
		return nil, errArtifactInput
	}
	if a.Expires <= s.clock().Unix() {
		return nil, errArtifactMissing
	}
	release, err := s.reserveWrite()
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err = s.db.Exec("UPDATE artifacts SET final=1,size=?,hash=? WHERE id=?", actual, digest, a.ID); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "artifact_id": fmt.Sprint(a.ID)}, nil
}
func (s *artifactStore) listResponse(p artifactPrincipal, r artifactRequest) (any, error) {
	all, err := s.list(p)
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for _, a := range all {
		if r.NameFilter != "" && a.Name != r.NameFilter || r.IDFilter != "" && r.IDFilter != fmt.Sprint(a.ID) {
			continue
		}
		result = append(result, map[string]any{"database_id": fmt.Sprint(a.ID), "workflow_run_backend_id": a.Run, "workflow_job_run_backend_id": a.Job, "name": a.Name, "size": fmt.Sprint(a.Size), "created_at": time.Unix(a.Created, 0).UTC().Format(time.RFC3339), "digest": strings.TrimPrefix(a.Hash, "sha256:")})
	}
	return map[string]any{"artifacts": result}, nil
}
func (s *artifactStore) signature(path, mode, expiry string) string {
	mac := hmac.New(sha256.New, s.secret[:])
	_, _ = fmt.Fprintf(mac, "trf-artifact-v1\n%s\n%s\n%s", path, mode, expiry)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *artifactStore) signedURL(origin string, a artifactRecord, mode string) string {
	ttl := 15 * time.Minute
	if mode == "write" {
		ttl = 6 * time.Hour
	} // 20 GiB over site A's uplink takes several hours.
	expiry := min(s.clock().Add(ttl).Unix(), a.Expires)
	path := artifactBlob + fmt.Sprint(a.ID)
	exp := fmt.Sprint(expiry)
	return origin + path + "?mode=" + mode + "&expires=" + exp + "&sig=" + s.signature(path, mode, exp)
}

// Capability verification and bounded network reads never hold the store lock
// or a disk reservation. The record is rechecked after the body arrives.
func (s *artifactStore) blob(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode, exp, sig := q.Get("mode"), q.Get("expires"), q.Get("sig")
	expiry, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || expiry <= s.clock().Unix() || !hmac.Equal([]byte(sig), []byte(s.signature(r.URL.Path, mode, exp))) || mode != "read" && mode != "write" {
		artifactError(w, 403)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, artifactBlob), 10, 64)
	if err != nil {
		artifactError(w, 403)
		return
	}
	if mode == "read" && r.Method == http.MethodGet {
		// Unlike uploads, GETs wait without buffering or pinning an artifact.
		select {
		case s.downloadSlots <- struct{}{}:
			defer func() { <-s.downloadSlots }()
		case <-r.Context().Done():
			artifactError(w, http.StatusRequestTimeout)
			return
		}
		// Cancellation and availability can become ready together. Never load a
		// block for a cancelled request, even if select acquired its own slot.
		if r.Context().Err() != nil {
			artifactError(w, http.StatusRequestTimeout)
			return
		}
		a, err := s.readableBlob(id, expiry)
		if err != nil {
			artifactStoreError(w, err)
			return
		}
		defer s.releaseLease(a.ID)
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprint(a.Size))
		if _, err = s.stream(a, artifactDownloadWriter{w}); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if mode != "write" || r.Method != http.MethodPut {
		artifactError(w, 403)
		return
	}
	put, release, err := s.readUpload(w, r)
	if err != nil {
		if errors.Is(err, errArtifactSpace) {
			// Do not let net/http drain a trickling rejected body before flushing 503.
			w.Header().Set("Connection", "close")
		}
		artifactStoreError(w, err)
		return
	}
	defer release()
	if err = s.writeBlob(id, expiry, put); err != nil {
		artifactStoreError(w, err)
		return
	}
	w.Header().Set("ETag", fmt.Sprintf("\"artifact-%d\"", id))
	w.Header().Set("Last-Modified", s.clock().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusCreated)
}
func (s *artifactStore) readableBlob(id, expiry int64) (artifactRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.record(id)
	if err != nil {
		return a, err
	}
	if !a.Final || min(a.Expires, expiry) <= s.clock().Unix() {
		return a, errArtifactMissing
	}
	s.leases[a.ID]++
	return a, nil
}
func (s *artifactStore) writeBlob(id, expiry int64, put func(artifactRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.record(id)
	if err != nil {
		return err
	}
	if min(a.Expires, expiry) <= s.clock().Unix() {
		return errArtifactMissing
	}
	if a.Final {
		return errArtifactAuth
	}
	release, err := s.reserveWrite()
	if err != nil {
		return err
	}
	defer release()
	return put(a)
}

// Success hands the slot to blob until writeBlob returns. Failed reads and
// panics before handoff release it here, without waiting on the store mutex.
func (s *artifactStore) readUpload(w http.ResponseWriter, r *http.Request) (put func(artifactRecord) error, release func(), err error) {
	select {
	case s.uploadSlots <- struct{}{}:
	default:
		return nil, nil, errArtifactSpace
	}
	releaseSlot := func() { <-s.uploadSlots }
	defer func() {
		if put == nil {
			releaseSlot()
		}
	}()
	switch r.URL.Query().Get("comp") {
	case "block":
		key := r.URL.Query().Get("blockid")
		decoded, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(decoded) == 0 || len(decoded) > 64 {
			return nil, nil, errArtifactInput
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, artifactBlockLimit))
		if err != nil {
			return nil, nil, errArtifactInput
		}
		return func(a artifactRecord) error { return s.putBlock(a, key, data) }, releaseSlot, nil
	case "blocklist":
		var list struct {
			XMLName xml.Name `xml:"BlockList"`
			Keys    []string `xml:"Latest"`
		}
		if xml.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&list) != nil {
			return nil, nil, errArtifactInput
		}
		return func(a artifactRecord) error { return s.commitBlocks(a, list.Keys) }, releaseSlot, nil
	default:
		return nil, nil, errArtifactInput
	}
}

// Caller holds mu. Committed blocks are immutable; a lease protects them
// while hashing yields the mutex to unrelated runs. Return with mu held.
func (s *artifactStore) hashUnlocked(a artifactRecord, w io.Writer) (int64, error) {
	s.leases[a.ID]++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.unlease(a.ID) }()
	return s.stream(a, w)
}
func (s *artifactStore) unlease(id int64) {
	s.leases[id]--
	if s.leases[id] == 0 {
		delete(s.leases, id)
	}
}
func (s *artifactStore) releaseLease(id int64) { s.mu.Lock(); defer s.mu.Unlock(); s.unlease(id) }

func artifactStoreError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, errArtifactMissing) {
		status = 404
	} else if errors.Is(err, errArtifactAuth) {
		status = 403
	} else if errors.Is(err, errArtifactInput) {
		status = 400
	}
	artifactError(w, status)
}
func artifactError(w http.ResponseWriter, status int) {
	code := "unavailable"
	switch status {
	case 403:
		code = "permission_denied"
	case 404:
		code = "not_found"
	case 400:
		code = "invalid_argument"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "msg": "artifact service: " + http.StatusText(status)})
}

// A stalled reader cannot retain its eviction lease indefinitely. Every block
// renews a progress deadline, rather than imposing a whole-transfer timeout.
type artifactDownloadWriter struct{ http.ResponseWriter }

func (w artifactDownloadWriter) Write(p []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(bodyStallTimeout))
	return w.ResponseWriter.Write(p)
}

// The SSH forward lands here, never on the cache/npm/identity listener.
func artifactOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !artifactRoute(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
