package adminapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func validRequest() DischargeRequest {
	return DischargeRequest{OperationID: "op-ea9b705d234ad29f14e79b6d", InstanceID: "trf-maestro-096ffcb3a52d8624",
		ReapInstance: true, Confirm: DischargeConfirmation, Reason: "permanent GitHub registration leak"}
}

// The daemon, not the CLI, is where a mutation's guards must live: any client that
// can open the socket is subject to the same confirmation and reason contract.
func TestDischargeRequestValidation(t *testing.T) {
	for name, testCase := range map[string]struct {
		request DischargeRequest
		valid   bool
	}{
		"complete":              {request: validRequest(), valid: true},
		"no reap is still fine": {request: DischargeRequest{OperationID: "op-1", InstanceID: "trf-1", Confirm: DischargeConfirmation, Reason: "r"}, valid: true},
		"wrong token":           {request: DischargeRequest{OperationID: "op-1", InstanceID: "trf-1", Confirm: "yes", Reason: "r"}},
		"no operation":          {request: DischargeRequest{InstanceID: "trf-1", Confirm: DischargeConfirmation, Reason: "r"}},
		"no instance":           {request: DischargeRequest{OperationID: "op-1", Confirm: DischargeConfirmation, Reason: "r"}},
		"blank reason":          {request: DischargeRequest{OperationID: "op-1", InstanceID: "trf-1", Confirm: DischargeConfirmation, Reason: " \t "}},
		"unbounded reason":      {request: DischargeRequest{OperationID: "op-1", InstanceID: "trf-1", Confirm: DischargeConfirmation, Reason: strings.Repeat("x", MaxReasonBytes+1)}},
		"unbounded operation":   {request: DischargeRequest{OperationID: strings.Repeat("o", MaxIdentifierBytes+1), InstanceID: "trf-1", Confirm: DischargeConfirmation, Reason: "r"}},
		"unbounded instance":    {request: DischargeRequest{OperationID: "op-1", InstanceID: strings.Repeat("i", MaxIdentifierBytes+1), Confirm: DischargeConfirmation, Reason: "r"}},
	} {
		t.Run(name, func(t *testing.T) {
			if testCase.request.Valid() != testCase.valid {
				t.Fatalf("Valid()=%t want %t", testCase.request.Valid(), testCase.valid)
			}
		})
	}
}

// The refusal vocabulary is closed, and every member maps to exactly one status.
// A code outside it must never be accepted from the wire.
func TestRefusalVocabularyIsClosedAndMappedToStatuses(t *testing.T) {
	statuses := map[string]int{
		RefusalUnknownOperation: http.StatusNotFound,
		RefusalNotAuthority:     http.StatusForbidden,
		RefusalInvalidRequest:   http.StatusBadRequest,
		RefusalUnconfirmed:      http.StatusBadRequest,
		RefusalReasonRequired:   http.StatusBadRequest,
		RefusalStoreUnavailable: http.StatusServiceUnavailable,
		RefusalVMUnobserved:     http.StatusServiceUnavailable,
		RefusalVMDeleteFailed:   http.StatusServiceUnavailable,
		RefusalResourceMismatch: http.StatusConflict,
		RefusalOperationLive:    http.StatusConflict,
		RefusalResourceBusy:     http.StatusConflict,
		RefusalInstanceState:    http.StatusConflict,
		RefusalVMRunning:        http.StatusConflict,
	}
	for code, want := range statuses {
		if !ValidRefusalCode(code) {
			t.Fatalf("%q is not in the closed vocabulary", code)
		}
		if got := RefusalStatus(code); got != want {
			t.Fatalf("RefusalStatus(%q)=%d want %d", code, got, want)
		}
	}
	for _, code := range []string{"", "runner is currently running a job", "sql: database is locked"} {
		if ValidRefusalCode(code) {
			t.Fatalf("%q must not be a refusal code", code)
		}
	}
	refusal := Refusal{Code: RefusalVMRunning}
	if !errors.Is(refusal, ErrRefused) || !strings.Contains(refusal.Error(), RefusalVMRunning) {
		t.Fatalf("refusal=%v", refusal)
	}
}

func dischargeClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestDischargeClientPostsAndReadsTheResult(t *testing.T) {
	var method, contentType string
	var body []byte
	client := dischargeClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, contentType = r.Method, r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		if r.URL.Path != DischargePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"fleet.v1","kind":"DischargeResult","operationId":"op-ea9b705d234ad29f14e79b6d",` +
			`"instanceId":"trf-maestro-096ffcb3a52d8624","operationDischarged":true,"instanceReaped":true,"vmDeleted":true}`))
	})
	result, err := client.Discharge(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || contentType != "application/json" {
		t.Fatalf("method=%s contentType=%s", method, contentType)
	}
	if !strings.Contains(string(body), DischargeConfirmation) || !strings.Contains(string(body), "reapInstance") {
		t.Fatalf("request body=%s", body)
	}
	if !result.OperationDischarged || !result.InstanceReaped || !result.VMDeleted {
		t.Fatalf("result=%#v", result)
	}
}

func TestDischargeClientRefusesLocallyBeforeSending(t *testing.T) {
	sent := false
	client := dischargeClient(t, func(http.ResponseWriter, *http.Request) { sent = true })
	_, err := client.Discharge(context.Background(), DischargeRequest{OperationID: "op-1", InstanceID: "trf-1"})
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != RefusalInvalidRequest {
		t.Fatalf("error=%v", err)
	}
	if sent {
		t.Fatal("an unconfirmed mutation left the operator's machine")
	}
}

func TestDischargeClientSurfacesBoundedRefusalsAndRejectsEverythingElse(t *testing.T) {
	for name, testCase := range map[string]struct {
		status  int
		body    string
		header  string
		wantErr error
		code    string
	}{
		"bounded refusal": {status: http.StatusConflict,
			body: `{"apiVersion":"fleet.v1","kind":"Refusal","code":"vm_running"}`, wantErr: ErrRefused, code: RefusalVMRunning},
		"unknown code is not echoed": {status: http.StatusConflict,
			body: `{"apiVersion":"fleet.v1","kind":"Refusal","code":"runner is currently running a job"}`, wantErr: ErrResponse},
		"error page is not echoed": {status: http.StatusInternalServerError, body: "<html>panic</html>", wantErr: ErrResponse},
		"non-json success":         {status: http.StatusOK, body: "{}", header: "text/plain", wantErr: ErrInvalidResponse},
		"wrong kind":               {status: http.StatusOK, body: `{"apiVersion":"fleet.v1","kind":"Status"}`, wantErr: ErrInvalidResponse},
		"unparseable success":      {status: http.StatusOK, body: "{", wantErr: ErrInvalidResponse},
	} {
		t.Run(name, func(t *testing.T) {
			client := dischargeClient(t, func(w http.ResponseWriter, _ *http.Request) {
				contentType := testCase.header
				if contentType == "" {
					contentType = "application/json"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, testCase.body)
			})
			_, err := client.Discharge(context.Background(), validRequest())
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("error=%v want %v", err, testCase.wantErr)
			}
			var refusal Refusal
			if testCase.code != "" && (!errors.As(err, &refusal) || refusal.Code != testCase.code) {
				t.Fatalf("refusal=%v want %s", err, testCase.code)
			}
		})
	}
}

func TestDischargePostRejectsAnUnbuildableRequest(t *testing.T) {
	client := &Client{baseURL: "://invalid", http: http.DefaultClient}
	if _, err := client.post(context.Background(), DischargePath, []byte("{}")); err == nil ||
		!strings.Contains(err.Error(), "build request") {
		t.Fatalf("error=%v", err)
	}
}

func TestDischargeClientRejectsAnOversizedResponseAndATransportFailure(t *testing.T) {
	client := dischargeClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat("x", MaxResponseBytes+1))
	})
	if _, err := client.Discharge(context.Background(), validRequest()); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized response error=%v", err)
	}
	offline, err := NewClient("unix:///nonexistent/fleet-discharge.sock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offline.Discharge(context.Background(), validRequest()); err == nil {
		t.Fatal("offline daemon mutation succeeded")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Discharge(canceled, validRequest()); err == nil {
		t.Fatal("canceled mutation succeeded")
	}
}
