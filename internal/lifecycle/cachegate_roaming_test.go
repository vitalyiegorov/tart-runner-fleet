package lifecycle

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// homeGate is the LAN gate the image names, as the node's config does.
const homeGate = "http://192.168.1.64:3017/"

// roamingGate answers the guest's unsigned blob probe with status, as the node's own
// gate does: 403 from the owner across the SSH forward, 503 when the forward is down.
// The guest's default route goes through the gate's address.
func roamingGate(t *testing.T, guest *guestShell, status int) string {
	t.Helper()
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.trf/artifacts/blobs/0" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(gate.Close)
	guest.standIn(t, "ip", `echo "default via 127.0.0.1 dev enp0s1 proto dhcp src 127.0.0.2 metric 100"`)
	return gate.URL
}

func roamingLaunch(t *testing.T, guest *guestShell, roaming string, verified bool) error {
	t.Helper()
	bootstrap := StdinBootstrapper{Runner: guest, Gate: fixedGate(verified), Roaming: roaming, Home: homeGate}
	return bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil)
}

// TestAnOffLANGuestSendsItsArtifactsToItsOwnHostsGate is the incident: the roaming
// MacBook's guest (knee-doctor job 137580595802) was moved to GitHub and hit its
// artifact quota. With the gate's forward up, the guest's results URL and the name the
// owner signs blob URLs for both reach the node's own forwarding gate.
func TestAnOffLANGuestSendsItsArtifactsToItsOwnHostsGate(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	roaming := roamingGate(t, guest, http.StatusForbidden)
	if err := roamingLaunch(t, guest, roaming, false); err != nil || !guest.started {
		t.Fatalf("err = %v, started = %v", err, guest.started)
	}
	if got, want := guest.file(t, "actions-runner/.env"), "OTHER=1\nACTIONS_RESULTS_URL="+roaming+"/\nLAST=2\n"; got != want {
		t.Fatalf(".env = %q, want %q", got, want)
	}
	if got, want := guest.file(t, "hosts"), "127.0.0.1 localhost\n127.0.0.1 host.containers.internal\n"; got != want {
		t.Fatalf("hosts = %q, want %q", got, want)
	}
}

// TestAGuestThatCannotUseItsRoamingGateKeepsGitHub: anything short of the owner's
// answer through the guest's own host leaves the guest on ADR 0078's fallback with its
// hosts file as the image made it.
func TestAGuestThatCannotUseItsRoamingGateKeepsGitHub(t *testing.T) {
	tests := []struct {
		name  string
		setUp func(*testing.T, *guestShell) string
	}{
		{name: "the gate's SSH forward is down", setUp: func(t *testing.T, guest *guestShell) string {
			return roamingGate(t, guest, http.StatusServiceUnavailable)
		}},
		{name: "the gate has no artifact route", setUp: func(t *testing.T, guest *guestShell) string {
			return roamingGate(t, guest, http.StatusNotFound)
		}},
		{name: "nothing answers", setUp: func(t *testing.T, guest *guestShell) string {
			roamingGate(t, guest, http.StatusForbidden)
			return "http://127.0.0.1:1"
		}},
		{name: "the address is not the guest's own host", setUp: func(t *testing.T, guest *guestShell) string {
			roaming := roamingGate(t, guest, http.StatusForbidden)
			guest.standIn(t, "ip", `echo "default via 127.0.0.10 dev enp0s1"`)
			return roaming
		}},
		{name: "curl fails after the owner's 403", setUp: func(t *testing.T, guest *guestShell) string {
			roaming := roamingGate(t, guest, http.StatusForbidden)
			guest.standIn(t, "curl", "printf 403\nexit 18")
			return roaming
		}},
		{name: "sudo is refused", setUp: func(t *testing.T, guest *guestShell) string {
			roaming := roamingGate(t, guest, http.StatusForbidden)
			guest.standIn(t, "sudo", "exit 1")
			return roaming
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guest := roamingGuest(t, imageEnv)
			if err := roamingLaunch(t, guest, test.setUp(t, guest), false); err != nil {
				t.Fatal(err)
			}
			if got := guest.file(t, "actions-runner/.env"); got != "OTHER=1\nACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/\nLAST=2\n" {
				t.Fatalf(".env = %q", got)
			}
			if got := guest.file(t, "hosts"); got != imageHosts {
				t.Fatalf("hosts = %q", got)
			}
		})
	}
}

// TestARoamingGateLeavesAnImageWithNoOverrideAlone: an unpatched runner takes the job
// message's results URL, as with the GitHub fallback.
func TestARoamingGateLeavesAnImageWithNoOverrideAlone(t *testing.T) {
	guest := roamingGuest(t, "")
	if err := roamingLaunch(t, guest, roamingGate(t, guest, http.StatusForbidden), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(guest.home, "actions-runner", ".env")); !os.IsNotExist(err) {
		t.Fatalf("created .env: %v", err)
	}
	if got := guest.file(t, "hosts"); got != imageHosts {
		t.Fatalf("hosts = %q", got)
	}
}

// TestAProvenLaunchReturnsARetriedGuestToItsLANGate: a launch retried on a guest an
// earlier attempt sent to the roaming gate gets the image's LAN gate back, both names.
func TestAProvenLaunchReturnsARetriedGuestToItsLANGate(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	roaming := roamingGate(t, guest, http.StatusForbidden)
	if err := roamingLaunch(t, guest, roaming, false); err != nil {
		t.Fatal(err)
	}
	if err := roamingLaunch(t, guest, roaming, true); err != nil || !guest.started {
		t.Fatalf("err = %v, started = %v", err, guest.started)
	}
	if got := guest.file(t, "actions-runner/.env"); got != imageEnv {
		t.Fatalf(".env = %q", got)
	}
	if got := guest.file(t, "hosts"); got != imageHosts {
		t.Fatalf("hosts = %q", got)
	}
}

// TestAProvenLaunchLeavesAHomeGuestAlone: a guest that never roamed is not touched.
func TestAProvenLaunchLeavesAHomeGuestAlone(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	guest.standIn(t, "sed", "exit 1")
	if err := roamingLaunch(t, guest, roamingGate(t, guest, http.StatusForbidden), true); err != nil || !guest.started {
		t.Fatalf("err = %v, started = %v", err, guest.started)
	}
	if guest.file(t, "actions-runner/.env") != imageEnv || guest.file(t, "hosts") != imageHosts {
		t.Fatal("a home guest was changed")
	}
}

// TestAFailedHomeRestoreStopsTheRunner: like a failed npm clear, a guest that may
// still name a gate this launch did not choose does not start.
func TestAFailedHomeRestoreStopsTheRunner(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	roaming := roamingGate(t, guest, http.StatusForbidden)
	if err := roamingLaunch(t, guest, roaming, false); err != nil {
		t.Fatal(err)
	}
	guest.started = false
	guest.standIn(t, "sudo", "exit 1")
	if err := roamingLaunch(t, guest, roaming, true); err == nil || guest.started {
		t.Fatalf("err = %v, started = %v", err, guest.started)
	}
}

// TestAnUnreadableHostsFileStopsTheRunner: a guest whose .env is home again but whose
// hosts file still names the roaming gate is not left alone when sudo cannot read it.
func TestAnUnreadableHostsFileStopsTheRunner(t *testing.T) {
	guest := roamingGuest(t, imageEnv)
	roaming := roamingGate(t, guest, http.StatusForbidden)
	if err := roamingLaunch(t, guest, roaming, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guest.home, "actions-runner", ".env"), []byte(imageEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	guest.started = false
	guest.standIn(t, "sudo", "exit 1")
	if err := roamingLaunch(t, guest, roaming, true); err == nil || guest.started {
		t.Fatalf("err = %v, started = %v", err, guest.started)
	}
}

// TestTheRoamingScriptsGetTheirAddressesAsArguments pins the argument vectors: no
// script has an address spliced into its text.
func TestTheRoamingScriptsGetTheirAddressesAsArguments(t *testing.T) {
	for verified, want := range map[bool][]string{
		false: {resultsFallbackScript, roamingScriptName, "http://192.168.64.1:3017", "192.168.64.1"},
		true: {homeRestoreScript, roamingScriptName, "http://192.168.64.1:3017", "192.168.64.1",
			"http://192.168.1.64:3017", "192.168.1.64"},
	} {
		runner := &recordingRunner{}
		bootstrap := StdinBootstrapper{Runner: runner, Gate: fixedGate(verified), Roaming: "http://192.168.64.1:3017/", Home: homeGate}
		if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
			t.Fatal(err)
		}
		want = append([]string{"exec", "trf-small-1", "bash", "-c"}, want...)
		if len(runner.calls) != 2 || strings.Join(runner.calls[0], "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("calls = %q", runner.calls)
		}
	}
}

// TestAFailedRoamingRewriteStopsTheRunner: as with the GitHub rewrite, a guest whose
// results URL cannot be shown to have moved does not start, and neither does one whose
// roaming setting cannot be read.
func TestAFailedRoamingRewriteStopsTheRunner(t *testing.T) {
	for _, bootstrap := range []StdinBootstrapper{
		{Runner: &recordingRunner{failOn: roamingScriptName}, Gate: fixedGate(false), Roaming: "http://192.168.64.1:3017", Home: homeGate},
		{Runner: &recordingRunner{}, Gate: fixedGate(false), Roaming: "http://[::1", Home: homeGate},
	} {
		if err := bootstrap.Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err == nil {
			t.Fatal("the runner started")
		}
		if calls := bootstrap.Runner.(*recordingRunner).calls; len(calls) > 1 {
			t.Fatalf("calls = %q", calls)
		}
	}
}
