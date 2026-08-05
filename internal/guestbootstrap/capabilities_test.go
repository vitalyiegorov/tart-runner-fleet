package guestbootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image-capabilities.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const sealedManifest = `{"schemaVersion": 1, "image": "macos-tartelet-base", "sealedAt": "2026-08-05T04:00:00Z",
 "capabilities": ["android-build-sdk", "ios-simulator-prewarmed", "jvm", "maestro-cli", "node-runtime"]}`

// TestCapabilityCheckIsFailClosed states the four cases in one place, because
// the whole value of this check is that the third and fourth are failures. A
// declaration that has gone stale because an image was rebuilt is the only thing
// this backstop catches that the configuration gates cannot, and it can only
// catch it by refusing to proceed on an unreadable answer.
func TestCapabilityCheckIsFailClosed(t *testing.T) {
	tests := []struct {
		name         string
		required     []string
		manifest     string
		absent       bool
		wantMissing  string
		wantDetail   string
		wantNoFailed bool
	}{
		{name: "nothing required reads no manifest at all", absent: true, wantNoFailed: true},
		{name: "required and present", required: []string{"maestro-cli", "jvm"}, manifest: sealedManifest, wantNoFailed: true},
		{name: "required and missing", required: []string{"maestro-cli", "redroid-android"},
			manifest: sealedManifest, wantMissing: "redroid-android"},
		{name: "manifest absent", required: []string{"maestro-cli"}, absent: true, wantDetail: "cannot be read"},
		{name: "manifest is not JSON", required: []string{"maestro-cli"}, manifest: "{", wantDetail: "cannot be parsed"},
		{name: "manifest is a schema this build does not understand", required: []string{"maestro-cli"},
			manifest: `{"schemaVersion": 2, "capabilities": ["maestro-cli"]}`, wantDetail: "schemaVersion"},
		{name: "manifest declares nothing", required: []string{"maestro-cli"},
			manifest:   `{"schemaVersion": 1, "image": "x", "sealedAt": "", "capabilities": []}`,
			wantDetail: "declares no capabilities"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "absent.json")
			if !test.absent {
				path = writeManifest(t, test.manifest)
			}
			err := checkCapabilities(path, test.required)
			if test.wantNoFailed {
				if err != nil {
					t.Fatalf("checkCapabilities() = %v, want nil", err)
				}
				return
			}
			var failure *CapabilityError
			if !errors.As(err, &failure) {
				t.Fatalf("checkCapabilities() = %v, want a *CapabilityError", err)
			}
			if test.wantMissing != "" {
				if failure.MissingCapability != test.wantMissing || failure.Unverifiable() {
					t.Fatalf("failure = %+v, want missing %q", failure, test.wantMissing)
				}
				if !strings.Contains(failure.Error(), test.wantMissing) {
					t.Errorf("message %q does not name the missing capability", failure.Error())
				}
				return
			}
			if !failure.Unverifiable() || !strings.Contains(failure.Error(), test.wantDetail) {
				t.Fatalf("failure = %q, want an unverifiable manifest mentioning %q", failure.Error(), test.wantDetail)
			}
		})
	}
}

// TestBootstrapChecksCapabilitiesBeforeReadingTheJIT is what makes the failure
// message safe to print: it is constructed from the operator's own flag and the
// image's own manifest, before a byte of standard input has been read.
func TestBootstrapChecksCapabilitiesBeforeReadingTheJIT(t *testing.T) {
	config := bootstrapConfig(t)
	config.RequiredCapabilities = []string{"redroid-android"}
	config.CapabilityManifestPath = writeManifest(t, sealedManifest)
	launcher := &fakeLauncher{process: &fakeProcess{}}
	stdin := &countingReader{Reader: strings.NewReader("encoded-jit\n")}
	err := Bootstrap{Launcher: launcher}.Run(context.Background(), stdin, config)
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.MissingCapability != "redroid-android" {
		t.Fatalf("Run() = %v, want a missing-capability failure", err)
	}
	if stdin.reads != 0 {
		t.Fatalf("standard input was read %d times before the capability check", stdin.reads)
	}
	if launcher.spec.Path != "" {
		t.Fatal("the runner was started despite a failed capability check")
	}
	if strings.Contains(failure.Error(), "encoded-jit") {
		t.Fatal("the capability failure reflected standard input")
	}
}

// TestBootstrapWithoutCapabilitiesNeverTouchesTheManifest pins the no-op: the
// default manifest path is absolute and does not exist on a developer machine,
// and a bootstrap that requires nothing must not care.
func TestBootstrapWithoutCapabilitiesNeverTouchesTheManifest(t *testing.T) {
	config := bootstrapConfig(t)
	if config.capabilityManifestPath() != CapabilityManifestPath {
		t.Fatalf("default manifest path = %q", config.capabilityManifestPath())
	}
	launcher := &fakeLauncher{process: &fakeProcess{}}
	if err := (Bootstrap{Launcher: launcher}).Run(context.Background(), strings.NewReader("encoded-jit\n"), config); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if launcher.spec.Path != config.RunnerPath {
		t.Fatal("the runner was not started")
	}
}

func TestCapabilityGrammarRejectsWhatTheConfigurationRejects(t *testing.T) {
	for _, valid := range []string{"redroid-android", "jvm", "node-runtime", "a", "x9"} {
		if !ValidCapability(valid) {
			t.Errorf("ValidCapability(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", "-jvm", "jvm-", "Redroid", "redroid_android", "a,b", "a b"} {
		if ValidCapability(invalid) {
			t.Errorf("ValidCapability(%q) = true", invalid)
		}
	}
}

type countingReader struct {
	Reader interface{ Read([]byte) (int, error) }
	reads  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func TestParseCapabilityListRefusesNonsense(t *testing.T) {
	capabilities, err := ParseCapabilityList("jvm,maestro-cli")
	if err != nil || strings.Join(capabilities, ",") != "jvm,maestro-cli" {
		t.Fatalf("ParseCapabilityList() = %v, %v", capabilities, err)
	}
	for _, value := range []string{"", "jvm,", "JVM", "jvm maestro"} {
		if _, err := ParseCapabilityList(value); err == nil {
			t.Errorf("ParseCapabilityList(%q) was accepted", value)
		}
	}
}
