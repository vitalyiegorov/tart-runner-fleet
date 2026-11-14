package cachegate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Replay #514's generated Twirp JSON: StringValue is a JSON string, and
// useProtoFieldName emits mime_type rather than the JS property's mimeType.
func TestArtifactV7ZIPUsesExistingStoreAndDownload(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "run", "producer")
	body := artifactBody("run", "producer", "archive")
	body["version"] = 7
	body["mime_type"] = "application/zip"
	body["future_client_field"] = map[string]any{"ignored": true}
	w := f.rpc(auth, "CreateArtifact", body)
	if w.Code != http.StatusOK {
		t.Fatalf("v7 ZIP create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		URL string `json:"signed_upload_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// A v4 retry resolves the same record and signed path, then uses the
	// unchanged block upload/finalize flow. No second object or format exists.
	if raw := f.upload(auth, "run", "producer", "archive", []byte("zip bytes")); raw != created.URL {
		t.Fatalf("v7 and v4 resolved different uploads: %q != %q", created.URL, raw)
	}
	sibling := f.token("123", "run", "consumer")
	w = f.rpc(sibling, "ListArtifacts", artifactBody("run", "consumer", ""))
	var listed struct {
		Artifacts []struct {
			Digest string `json:"digest"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || w.Code != http.StatusOK || len(listed.Artifacts) != 1 {
		t.Fatalf("list: %d %s (%v)", w.Code, w.Body.String(), err)
	}
	// download-artifact compares this to its computed "sha256:<hex>"; v8's
	// default digest-mismatch is error, so a bare hex digest fails every download.
	sum := sha256.Sum256([]byte("zip bytes"))
	if want := "sha256:" + hex.EncodeToString(sum[:]); listed.Artifacts[0].Digest != want {
		t.Fatalf("listed digest %q, want %q", listed.Artifacts[0].Digest, want)
	}
	w = f.rpc(sibling, "GetSignedArtifactURL", artifactBody("run", "producer", "archive"))
	var signed struct {
		URL string `json:"signed_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &signed); err != nil || w.Code != http.StatusOK {
		t.Fatalf("sign: %d %s (%v)", w.Code, w.Body.String(), err)
	}
	download := f.blob(signed.URL, http.MethodGet, nil)
	if download.Code != http.StatusOK || download.Header().Get("Content-Type") != "application/zip" || download.Body.String() != "zip bytes" {
		t.Fatalf("download: %d %v %q", download.Code, download.Header(), download.Body.String())
	}
}

func TestArtifactCreateProtocolRefusals(t *testing.T) {
	f := newArtifactFixture(t)
	auth := f.token("123", "run", "job")
	for _, tc := range []struct {
		name    string
		version int
		mime    any
	}{
		{"raw", 7, "text/plain"},
		{"binary", 7, "application/octet-stream"},
		{"empty", 7, ""},
		{"missing", 7, nil},
		{"unknown-version", 8, "application/zip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := artifactBody("run", "job", tc.name)
			body["version"] = tc.version
			if tc.mime != nil {
				body["mime_type"] = tc.mime
			}
			w := f.rpc(auth, "CreateArtifact", body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_argument"`) {
				t.Fatalf("refusal: %d %s", w.Code, w.Body.String())
			}
			if tc.version == 7 && !strings.Contains(w.Body.String(), "version 7 requires application/zip; non-zip uploads are unsupported") {
				t.Fatalf("missing explicit MIME refusal: %s", w.Body.String())
			}
		})
	}
	w := f.rpc(auth, "ListArtifacts", artifactBody("run", "job", ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"artifacts":[]`) {
		t.Fatalf("refused creates left objects: %d %s", w.Code, w.Body.String())
	}
}
