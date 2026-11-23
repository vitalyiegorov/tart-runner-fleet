package lifecycle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imageHosts is the site-A Tart base's /etc/hosts (LINUX_BASE_IMAGE.md, layer c).
const imageHosts = "127.0.0.1 localhost\n192.168.1.64 host.containers.internal\n"

const imageEnv = "OTHER=1\nACTIONS_RESULTS_URL=http://192.168.1.64:3017/\nLAST=2\n"

// roamingGuest is a guest whose sudo works on a stand-in /etc/hosts in its HOME.
func roamingGuest(t *testing.T, env string) *guestShell {
	t.Helper()
	guest := newGuestShell(t)
	if err := os.Mkdir(filepath.Join(guest.home, "actions-runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(guest.home, "actions-runner", ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(guest.home, "hosts"), []byte(imageHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	guest.standIn(t, "sudo", `[ "$1" = -n ] || exit 1
shift
for arg; do
  shift
  [ "$arg" = /etc/hosts ] && arg=$HOME/hosts
  set -- "$@" "$arg"
done
exec "$@"`)
	return guest
}

func (g *guestShell) file(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(g.home, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// roamingGate answers the guest's probe as the node's own gate does, and returns the
// route a node hands its launches for it.
func roamingGate(t *testing.T) *Roaming {
	t.Helper()
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad challenge", http.StatusBadRequest)
	}))
	t.Cleanup(gate.Close)
	parsed, _ := url.Parse(gate.URL)
	return &Roaming{URL: gate.URL, Host: parsed.Hostname()}
}

func roamingLaunch(t *testing.T, guest *guestShell, roaming *Roaming) {
	t.Helper()
	bootstrap := StdinBootstrapper{Runner: guest, Gate: fixedGate(false), Roaming: roaming}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if !guest.started {
		t.Fatal("the runner did not start")
	}
}

// TestAnOffLANGuestSendsItsArtifactsToItsOwnHostsGate is the incident: the roaming
// MacBook's guest (knee-doctor job 137580595802) was moved to GitHub and hit its
// artifact quota. With a roaming gate the guest's results URL and the name the owner
// signs blob URLs for both reach the node's own forwarding gate.
func TestAnOffLANGuestSendsItsArtifactsToItsOwnHostsGate(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	roaming := roamingGate(t)
	roamingLaunch(t, guest, roaming)
	if got, want := guest.file(t, "actions-runner/.env"), "OTHER=1\nACTIONS_RESULTS_URL="+roaming.URL+"/\nLAST=2\n"; got != want {
		t.Fatalf(".env = %q, want %q", got, want)
	}
	if got, want := guest.file(t, "hosts"), "127.0.0.1 localhost\n"+roaming.Host+" host.containers.internal\n"; got != want {
		t.Fatalf("hosts = %q, want %q", got, want)
	}
}

// TestAGuestThatCannotReachItsRoamingGateKeepsGitHub: nothing answers, so the guest
// takes ADR 0078's fallback and its hosts file is left as the image made it.
func TestAGuestThatCannotReachItsRoamingGateKeepsGitHub(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	roamingLaunch(t, guest, &Roaming{URL: "http://127.0.0.1:1", Host: "127.0.0.1"})
	if got := guest.file(t, "actions-runner/.env"); got != "OTHER=1\nACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/\nLAST=2\n" {
		t.Fatalf(".env = %q", got)
	}
	if got := guest.file(t, "hosts"); got != imageHosts {
		t.Fatalf("hosts = %q", got)
	}
}

// TestAGuestThatCannotMoveTheBlobNameKeepsGitHub: a results URL at the roaming gate
// with blob URLs still naming the LAN gate would fail every artifact, so a refused
// sudo keeps the whole guest on GitHub.
func TestAGuestThatCannotMoveTheBlobNameKeepsGitHub(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	guest.standIn(t, "sudo", "exit 1")
	roamingLaunch(t, guest, roamingGate(t))
	if got := guest.file(t, "actions-runner/.env"); !strings.Contains(got, "ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/\n") {
		t.Fatalf(".env = %q", got)
	}
	if got := guest.file(t, "hosts"); got != imageHosts {
		t.Fatalf("hosts = %q", got)
	}
}

// TestARoamingGateLeavesAnImageWithNoOverrideAlone: an unpatched runner takes the job
// message's results URL, as with the GitHub fallback.
func TestARoamingGateLeavesAnImageWithNoOverrideAlone(t *testing.T) {
	guest := roamingGuest(t, "")
	roamingLaunch(t, guest, roamingGate(t))
	if _, err := os.Stat(filepath.Join(guest.home, "actions-runner", ".env")); !os.IsNotExist(err) {
		t.Fatalf("created .env: %v", err)
	}
	if got := guest.file(t, "hosts"); got != imageHosts {
		t.Fatalf("hosts = %q", got)
	}
}

// TestAProvenLANGateNeverAsksForTheRoamingGate: at home the guest keeps the image's
// LAN gate, which already forwards to the owner.
func TestAProvenLANGateNeverAsksForTheRoamingGate(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(true), Roaming: &Roaming{URL: "http://192.168.64.1:3017", Host: "192.168.64.1"}}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0][3] != bootstrapHelper {
		t.Fatalf("calls = %q", runner.calls)
	}
}

// TestAnUnprovenGateAsksForTheRoamingGateWithItsAddress pins the argument vector: the
// script never has the address spliced into its text.
func TestAnUnprovenGateAsksForTheRoamingGateWithItsAddress(t *testing.T) {
	runner := &recordingRunner{}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(false), Roaming: &Roaming{URL: "http://192.168.64.1:3017/", Host: "192.168.64.1"}}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "trf-small-1", "bash", "-c", roamingScript, roamingScriptName, "http://192.168.64.1:3017", "192.168.64.1"}
	if len(runner.calls) != 2 || strings.Join(runner.calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("calls = %q", runner.calls)
	}
}

// TestAFailedRoamingRewriteStopsTheRunner: as with the GitHub rewrite, a guest whose
// results URL cannot be shown to have moved does not start.
func TestAFailedRoamingRewriteStopsTheRunner(t *testing.T) {
	runner := &recordingRunner{failOn: roamingScriptName}
	bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(false), Roaming: &Roaming{URL: "http://192.168.64.1:3017", Host: "192.168.64.1"}}
	if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err == nil {
		t.Fatal("the runner started")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %q", runner.calls)
	}
}
