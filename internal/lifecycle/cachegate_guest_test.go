package lifecycle

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// guestShell is a guest as far as a launch can tell: every script it is sent really
// runs, in a throwaway HOME, and the runner helper only records that it was reached.
// A stand-in on bin shadows the real command, which is how a test makes one step fail.
type guestShell struct {
	home, bin string
	started   bool
}

func newGuestShell(t *testing.T) *guestShell {
	t.Helper()
	return &guestShell{home: t.TempDir(), bin: t.TempDir()}
}

func (g *guestShell) Run(ctx context.Context, _ io.Reader, args ...string) error {
	if len(args) < 5 || args[2] != "bash" {
		g.started = true
		return nil
	}
	// -c and not -lc: a login shell's profile could reset PATH past the stand-ins.
	command := exec.CommandContext(ctx, "bash", append([]string{"-c"}, args[4:]...)...)
	command.Env = []string{"HOME=" + g.home, "PATH=" + g.bin + ":" + os.Getenv("PATH")}
	return command.Run()
}

func (g *guestShell) standIn(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(g.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (g *guestShell) npmrc() string {
	raw, err := os.ReadFile(filepath.Join(g.home, ".npmrc"))
	if err != nil {
		return ""
	}
	return string(raw)
}

func (g *guestShell) seedNPMRC(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(g.home, ".npmrc"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// oldGateFile is what an earlier attempt at this launch left, naming a gate the node
// no longer proves.
const oldGateFile = npmMarker + "\nregistry=http://192.168.1.99:3017/npm/\nnoproxy=192.168.1.99\nno-proxy=192.168.1.99\n"

// npmFixture answers the guest's own probe with one status, for real curl to read.
func npmFixture(t *testing.T, status int) (string, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/npm/-/ping" {
			hits.Add(1)
		}
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "http://127.0.0.1:1/npm/-/ping")
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/npm/", hits
}

func provenLaunch(guest *guestShell, registry string) StdinBootstrapper {
	return StdinBootstrapper{Runner: guest, Gate: fixedGate(true),
		NPM: &NPMRoute{Registry: registry, Host: "127.0.0.1", Ready: fixedGate(true)}}
}

// TestAFleetFileThatCannotBeRemovedStopsTheRunner: the guest would otherwise keep the
// previous gate's address although this launch proved a different one.
func TestAFleetFileThatCannotBeRemovedStopsTheRunner(t *testing.T) {
	registry, _ := npmFixture(t, http.StatusOK)
	for name, launch := range map[string]func(*guestShell) StdinBootstrapper{
		"proven gate": func(g *guestShell) StdinBootstrapper { return provenLaunch(g, registry) },
		"unproven gate": func(g *guestShell) StdinBootstrapper {
			return StdinBootstrapper{Runner: g, Gate: fixedGate(false), NPM: npmRoute(true)}
		},
		"npm route down": func(g *guestShell) StdinBootstrapper {
			return StdinBootstrapper{Runner: g, Gate: fixedGate(true), NPM: npmRoute(false)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			guest := newGuestShell(t)
			guest.seedNPMRC(t, oldGateFile)
			guest.standIn(t, "rm", "exit 1")
			secret := jitSecret()
			if err := launch(guest).Bootstrap(context.Background(), "trf-small-1", secret, nil); err == nil {
				t.Fatal("a guest still naming the previous gate was allowed to start")
			}
			if guest.started {
				t.Fatal("the runner started beside a stale registry")
			}
			if secret.Reveal() != "" {
				t.Fatal("the secret outlived the failed launch")
			}
		})
	}
}

// TestAFileThatCannotBeJudgedStopsTheRunner: grep failing to read is not grep finding
// no marker. Reading it as the image's own file would leave a stale registry in place.
func TestAFileThatCannotBeJudgedStopsTheRunner(t *testing.T) {
	registry, _ := npmFixture(t, http.StatusOK)
	guest := newGuestShell(t)
	guest.seedNPMRC(t, oldGateFile)
	guest.standIn(t, "grep", "exit 2")
	if err := provenLaunch(guest, registry).Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err == nil || guest.started {
		t.Fatalf("err = %v, started = %v", err, guest.started)
	}
}

// TestARetriedLaunchNamesOnlyTheGateItJustProved is the case the removal exists for.
func TestARetriedLaunchNamesOnlyTheGateItJustProved(t *testing.T) {
	registry, _ := npmFixture(t, http.StatusOK)
	guest := newGuestShell(t)
	guest.seedNPMRC(t, oldGateFile)
	if err := provenLaunch(guest, registry).Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
		t.Fatal(err)
	}
	got := guest.npmrc()
	if !guest.started || !strings.Contains(got, "registry="+registry) || strings.Contains(got, "192.168.1.99") {
		t.Fatalf("started = %v, .npmrc = %q", guest.started, got)
	}
}

// TestAFailedOptionalWriteCarriesOnDirect is the other half: once any old setting is
// gone, a failure to write the new one costs only a cold job.
func TestAFailedOptionalWriteCarriesOnDirect(t *testing.T) {
	for name, failing := range map[string]string{"mktemp": "mktemp", "mv": "mv"} {
		t.Run(name, func(t *testing.T) {
			registry, _ := npmFixture(t, http.StatusOK)
			guest := newGuestShell(t)
			guest.seedNPMRC(t, oldGateFile)
			guest.standIn(t, failing, "exit 1")
			if err := provenLaunch(guest, registry).Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
				t.Fatal(err)
			}
			if !guest.started {
				t.Fatal("a guest with no setting did not start")
			}
			if got := guest.npmrc(); got != "" {
				t.Fatalf(".npmrc = %q", got)
			}
		})
	}
}

// TestTheGuestProbeRequiresExactly200 uses real curl, because the defect is in what
// real curl calls success: without --location, --fail passes a 302.
func TestTheGuestProbeRequiresExactly200(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect, http.StatusNoContent, http.StatusCreated, http.StatusNotFound, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			registry, hits := npmFixture(t, status)
			guest := newGuestShell(t)
			if err := provenLaunch(guest, registry).Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
				t.Fatal(err)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("the guest asked the route %d times", got)
			}
			if !guest.started {
				t.Fatal("a guest with no setting did not start")
			}
			if got := guest.npmrc(); got != "" {
				t.Fatalf("a %d gave the guest a registry: %q", status, got)
			}
		})
	}
	t.Run("OK", func(t *testing.T) {
		registry, _ := npmFixture(t, http.StatusOK)
		guest := newGuestShell(t)
		if err := provenLaunch(guest, registry).Bootstrap(context.Background(), "trf-small-1", jitSecret(), nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(guest.npmrc(), "registry="+registry) {
			t.Fatalf(".npmrc = %q", guest.npmrc())
		}
	})
}
