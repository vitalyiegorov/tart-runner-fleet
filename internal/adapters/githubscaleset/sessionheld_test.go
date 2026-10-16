package githubscaleset

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// fakeActionsService answers the official client's whole session handshake --
// registration token, admin connection, session create -- with sessionStatus
// on the create, so the error under test is the one the pinned SDK really
// produces rather than one this package imagines (ADR 0063 spike).
func fakeActionsService(t *testing.T, sessionStatus int) *httptest.Server {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	adminToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("POST /api/v3/repos/owner/repo/actions/runners/registration-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"registration","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("POST /api/v3/actions/runner-registration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"url":%q,"token":%q}`, server.URL+"/tenant/", adminToken)
	})
	mux.HandleFunc("POST /tenant/_apis/runtime/runnerscalesets/19/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(sessionStatus)
		_, _ = w.Write([]byte(`{"typeName":"TaskAgentSessionConflictException","message":"the scale set already has an active session"}`))
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func openSessionAgainst(t *testing.T, server *httptest.Server) error {
	t.Helper()
	client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: server.URL + "/owner/repo", PersonalAccessToken: "test"}, scaleset.WithRetryMax(0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = client.MessageSessionClient(ctx, 19, "macbook")
	if err == nil {
		t.Fatal("the fake must refuse the session")
	}
	return fmt.Errorf("create official message session: %w", err)
}

func TestTheOfficialClientsRefusedSessionIsRecognisedAsHeld(t *testing.T) {
	if err := openSessionAgainst(t, fakeActionsService(t, http.StatusConflict)); !SessionHeld(err) {
		t.Fatalf("a 409 on session create must read as held by another session: %v", err)
	}
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest, http.StatusInternalServerError} {
		if err := openSessionAgainst(t, fakeActionsService(t, status)); SessionHeld(err) {
			t.Fatalf("status %d is not a held session: %v", status, err)
		}
	}
}

func TestSessionHeldIsNarrowerThanAny409(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("plain failure"),
		errors.New(`request POST https://api/repos/o/r/actions/runners/registration-token failed(status="409 Conflict")`),
		&APIError{Kind: Conflict, Status: 409},
	} {
		if SessionHeld(err) {
			t.Fatalf("SessionHeld(%v) must be false: only a refused session create is a held session", err)
		}
	}
	if got := IngestFailureDetail(&SessionFailure{Reason: ReasonSessionHeld}); got != ReasonSessionHeld || phaseFor(got) != PhaseCreate {
		t.Fatalf("the held reason must be in the closed vocabulary and belong to the create phase, got %q", got)
	}
}
