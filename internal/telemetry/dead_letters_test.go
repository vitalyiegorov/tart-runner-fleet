package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// phantomLetter is the 2026-07-25 dead letter: a deregister GitHub refuses to
// release, parked because nothing else can advance the instance and its VM is
// stopped.
func phantomLetter() DeadLetter {
	return DeadLetter{OperationID: "op-ea9b705d234ad29f14e79b6d", Kind: "deregister",
		Code: "deregister:runner_busy", ResourceID: "trf-maestro-096ffcb3a52d8624", Attempts: 835, Parked: true}
}

// The count alone is not actionable. `fleet operations` must be able to name the
// operation an operator will discharge, and `fleet update` must be able to see
// which resources are parked, both from the same published document.
func TestDeadLettersReachStatusAndMetrics(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetDeadLetters([]DeadLetter{phantomLetter(),
		{OperationID: "op-2", Kind: "deregister", Code: "deregister:runner_forbidden", ResourceID: "trf-linux-2", Attempts: 720},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := health.Snapshot()
	if len(snapshot.DeadLetters) != 2 {
		t.Fatalf("snapshot dead letters=%#v", snapshot.DeadLetters)
	}
	snapshot.DeadLetters[0] = DeadLetter{}
	if health.Snapshot().DeadLetters[0].OperationID != "op-ea9b705d234ad29f14e79b6d" {
		t.Fatal("snapshot aliases the dead-letter list")
	}

	server, err := NewServer(health, ServerConfig{ControllerVersion: "v1", ControllerMode: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	statusResponse := request(t, server, http.MethodGet, adminapi.StatusPath)
	defer statusResponse.Body.Close()
	var status adminapi.StatusEnvelope
	if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	letters := status.Data.Operations.DeadLetters
	if len(letters) != 2 || letters[0].OperationID != "op-ea9b705d234ad29f14e79b6d" ||
		letters[0].ResourceID != "trf-maestro-096ffcb3a52d8624" || letters[0].Attempts != 835 || !letters[0].Parked {
		t.Fatalf("status dead letters=%#v", letters)
	}
	if letters[1].Parked {
		t.Fatalf("a resource with progressing work must not publish as parked: %#v", letters[1])
	}
	// Exactly one of the two is parked, so an alert can fire on capacity nothing
	// will reclaim on its own.
	assertResponse(t, server, http.MethodGet, adminapi.MetricsPath, http.StatusOK, "fleet_operations_parked 1")
}

// A fleet with nothing parked must emit exactly the document older clients saw.
func TestHealthyFleetOmitsTheDeadLetterField(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetDeadLetters(nil); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(health, ServerConfig{ControllerVersion: "v1", ControllerMode: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodGet, adminapi.StatusPath)
	defer response.Body.Close()
	body := readAllString(t, response)
	if strings.Contains(body, "deadLetters") {
		t.Fatalf("healthy status document names dead letters: %s", body)
	}
	assertResponse(t, server, http.MethodGet, adminapi.MetricsPath, http.StatusOK, "fleet_operations_parked 0")
}

// Rule 4 in its sharpest form: a rejected dead-letter observation must never be
// stored as an empty list, because "nothing is parked" is the reading that would
// release the update quiescence gate on a fleet that is not parked at all.
func TestSetDeadLettersRejectsAnythingUnbounded(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetDeadLetters([]DeadLetter{phantomLetter()}); err != nil {
		t.Fatal(err)
	}
	oversized := make([]DeadLetter, maxDeadLetters+1)
	for index := range oversized {
		oversized[index] = phantomLetter()
	}
	rejects := map[string][]DeadLetter{
		"too many": oversized,
		"negative attempts": {{OperationID: "op-1", Kind: "deregister", Code: "deregister:runner_busy",
			ResourceID: "trf-1", Attempts: -1}},
		"unbounded operation id": {{OperationID: strings.Repeat("o", 200), Kind: "deregister",
			Code: "deregister:runner_busy", ResourceID: "trf-1"}},
		"upstream text as a resource": {{OperationID: "op-1", Kind: "deregister", Code: "deregister:runner_busy",
			ResourceID: "Runner trf-maestro is currently running a job"}},
		"upstream text as a code": {{OperationID: "op-1", Kind: "deregister",
			Code: "Bad request - Runner is currently running a job", ResourceID: "trf-1"}},
		"upstream text as a kind": {{OperationID: "op-1", Kind: "https://api.github.com/orgs/budgie-at",
			Code: "deregister:runner_busy", ResourceID: "trf-1"}},
	}
	for name, letters := range rejects {
		t.Run(name, func(t *testing.T) {
			if err := health.SetDeadLetters(letters); !errors.Is(err, errInvalidMetric) {
				t.Fatalf("error=%v want errInvalidMetric", err)
			}
			// The prior good observation must survive a rejection intact.
			if snapshot := health.Snapshot(); len(snapshot.DeadLetters) != 1 {
				t.Fatalf("rejection replaced the published dead letters: %#v", snapshot.DeadLetters)
			}
		})
	}
}

type fakeMutator struct {
	result  adminapi.DischargeResult
	err     error
	request adminapi.DischargeRequest
	calls   int
}

func (f *fakeMutator) DischargeDeadLetter(_ context.Context, request adminapi.DischargeRequest) (adminapi.DischargeResult, error) {
	f.calls++
	f.request = request
	return f.result, f.err
}

func mutatingServer(t *testing.T, mutator Mutator) *Server {
	t.Helper()
	health, _ := newTestHealth(t)
	server, err := NewServer(health, ServerConfig{ControllerVersion: "v1", ControllerMode: "authority", Mutator: mutator})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func postDischarge(t *testing.T, server *Server, body string) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://localhost"+adminapi.DischargePath, strings.NewReader(body))
	server.httpServer.Handler.ServeHTTP(recorder, req)
	return recorder.Result()
}

func readAllString(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// The read-only listener must have no mutating route at all. Registering it only
// on the private socket server makes that structural rather than a check that a
// future refactor could drop.
func TestReadOnlyServerHasNoMutatingRoute(t *testing.T) {
	health, _ := newTestHealth(t)
	server, err := NewServer(health, ServerConfig{ControllerVersion: "v1", ControllerMode: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	response := postDischarge(t, server, `{"confirm":"discharge-dead-letter"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("read-only server answered the mutation with %d", response.StatusCode)
	}
}

func TestDischargeRouteAcceptsOnlyPostAndBoundedBodies(t *testing.T) {
	mutator := &fakeMutator{result: adminapi.DischargeResult{APIVersion: adminapi.APIVersion,
		Kind: adminapi.DischargeKind, OperationID: "op-1", OperationDischarged: true}}
	server := mutatingServer(t, mutator)

	getResponse := request(t, server, http.MethodGet, adminapi.DischargePath)
	defer getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusMethodNotAllowed || getResponse.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET on the mutation route = %d allow=%q", getResponse.StatusCode, getResponse.Header.Get("Allow"))
	}
	if mutator.calls != 0 {
		t.Fatal("a GET reached the mutator")
	}

	for name, body := range map[string]string{
		"unparseable": "{",
		"oversized":   `{"reason":"` + strings.Repeat("x", adminapi.MaxRequestBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := postDischarge(t, server, body)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest ||
				!strings.Contains(readAllString(t, response), adminapi.RefusalInvalidRequest) {
				t.Fatalf("%s body = %d", name, response.StatusCode)
			}
		})
	}

	response := postDischarge(t, server, `{"operationId":"op-1","instanceId":"trf-1","confirm":"discharge-dead-letter","reason":"leak"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("accepted mutation = %d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	if mutator.request.OperationID != "op-1" || mutator.request.Reason != "leak" {
		t.Fatalf("mutator saw %#v", mutator.request)
	}
	if !strings.Contains(readAllString(t, response), adminapi.DischargeKind) {
		t.Fatal("mutation response is not a DischargeResult")
	}
}

// A refusal must travel as its closed-vocabulary code with the matching status,
// and anything the mutator did not classify must degrade to store_unavailable
// rather than echoing an internal error to the operator.
func TestDischargeRouteAnswersBoundedRefusals(t *testing.T) {
	for name, testCase := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"vm running":     {err: adminapi.Refusal{Code: adminapi.RefusalVMRunning}, status: http.StatusConflict, code: adminapi.RefusalVMRunning},
		"not authority":  {err: adminapi.Refusal{Code: adminapi.RefusalNotAuthority}, status: http.StatusForbidden, code: adminapi.RefusalNotAuthority},
		"unknown letter": {err: adminapi.Refusal{Code: adminapi.RefusalUnknownOperation}, status: http.StatusNotFound, code: adminapi.RefusalUnknownOperation},
		"unclassified refusal code": {err: adminapi.Refusal{Code: "sql: database is locked"},
			status: http.StatusServiceUnavailable, code: adminapi.RefusalStoreUnavailable},
		"opaque failure": {err: errors.New("panic in the mutator"), status: http.StatusServiceUnavailable,
			code: adminapi.RefusalStoreUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			server := mutatingServer(t, &fakeMutator{err: testCase.err})
			response := postDischarge(t, server, `{"operationId":"op-1","instanceId":"trf-1","confirm":"discharge-dead-letter","reason":"leak"}`)
			defer response.Body.Close()
			body := readAllString(t, response)
			if response.StatusCode != testCase.status || !strings.Contains(body, testCase.code) {
				t.Fatalf("refusal = %d %s", response.StatusCode, body)
			}
			if strings.Contains(body, "panic") || strings.Contains(body, "sql:") {
				t.Fatalf("refusal echoed unbounded text: %s", body)
			}
		})
	}
}
