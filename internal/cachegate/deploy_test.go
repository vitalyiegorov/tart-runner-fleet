package cachegate

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func deployFile(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "deploy"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func uncommented(text string) string {
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// TestTheBackendIsPublishedToLoopbackOnly: the gate is the only way in, so a guest
// that addresses the host directly finds no port.
func TestTheBackendIsPublishedToLoopbackOnly(t *testing.T) {
	unit := uncommented(deployFile(t, "npm-cache", "tart-runner-fleet-npm-cache.container"))
	publishes := regexp.MustCompile(`(?m)^PublishPort=(.*)$`).FindAllStringSubmatch(unit, -1)
	if len(publishes) != 1 || !strings.HasPrefix(publishes[0][1], "127.0.0.1:3020:") {
		t.Fatalf("publishes = %q", publishes)
	}
	if !regexp.MustCompile(`(?m)^Image=[^:]+@sha256:[0-9a-f]{64}$`).MatchString(unit) {
		t.Fatal("the image is not pinned by digest")
	}
	if strings.Contains(unit, "ContainerName=trf-") {
		t.Fatal("a sidecar in the trf- namespace stops the scheduler (issue #415)")
	}
	for _, want := range []string{"ContainerName=fleet-npm-cache", "DropCapability=all", "NoNewPrivileges=true",
		"/verdaccio/conf:ro", "/verdaccio/storage:Z", "--memory="} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit lacks %q", want)
		}
	}
	for _, refused := range []string{"EnvironmentFile", "Environment=", "Secret=", "VERDACCIO_PUBLIC_URL"} {
		if strings.Contains(unit, refused) {
			t.Errorf("the unit carries %q: the cache needs no credential and no fixed public URL", refused)
		}
	}
}

// TestTheSampleConfigIsTheSecondLock pins what the gate already enforces, so that a
// mistake at the gate still meets a server that refuses.
func TestTheSampleConfigIsTheSecondLock(t *testing.T) {
	config := uncommented(deployFile(t, "npm-cache", "verdaccio.yaml"))
	for _, want := range []string{"url_prefix: " + NPMPrefix, "max_users: -1", "url: https://registry.npmjs.org/"} {
		if !strings.Contains(config, want) {
			t.Errorf("the config lacks %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^web:\n\s+enable: false$`).MatchString(config) {
		t.Error("the web UI is on")
	}
	if strings.Contains(config, "$authenticated") || strings.Contains(config, "$anonymous") {
		t.Error("the config grants a group that exists")
	}
	for _, line := range strings.Split(config, "\n") {
		field := strings.TrimSpace(line)
		if strings.HasPrefix(field, "publish:") || strings.HasPrefix(field, "unpublish:") {
			if !strings.HasSuffix(field, " $nobody") {
				t.Errorf("%q must name nobody", field)
			}
		}
	}
	if got := strings.Count(config, "url: "); got != 1 {
		t.Errorf("the config names %d uplinks, want npmjs alone", got)
	}
	uplink := config[strings.Index(config, "uplinks:"):strings.Index(config, "packages:")]
	if strings.Contains(uplink, "auth:") || strings.Contains(uplink, "token") {
		t.Error("the uplink is not anonymous")
	}
	maxage := regexp.MustCompile(`maxage: (\d+)m\b`).FindStringSubmatch(config)
	if maxage == nil {
		t.Fatal("no maxage in minutes")
	}
	if minutes, _ := strconv.Atoi(maxage[1]); minutes < 1 || minutes > 60 {
		t.Errorf("maxage %dm: metadata must be cached, and for at most an hour", minutes)
	}
	if !strings.Contains(config, "enabled: false") {
		t.Error("the audit middleware is on")
	}
}

// TestTheDropInIsTheShippedGateCommandAndOneFlag: a gate that drifted here would run
// old flags on the one node with the npm route.
func TestTheDropInIsTheShippedGateCommandAndOneFlag(t *testing.T) {
	execStart := regexp.MustCompile(`(?m)^ExecStart=(.+)$`)
	shipped := execStart.FindStringSubmatch(deployFile(t, "actions-cache", "tart-runner-fleet-cache-gate.service"))
	dropIn := deployFile(t, "npm-cache", "tart-runner-fleet-cache-gate.service.d", "npm-cache.conf")
	found := execStart.FindAllStringSubmatch(dropIn, -1)
	if shipped == nil || len(found) != 1 {
		t.Fatalf("shipped = %q, drop-in = %q", shipped, found)
	}
	if want := shipped[1] + " --npm-upstream http://127.0.0.1:3020"; found[0][1] != want {
		t.Fatalf("drop-in = %q\nwant      %q", found[0][1], want)
	}
	if !regexp.MustCompile(`(?m)^ExecStart=$`).MatchString(dropIn) {
		t.Fatal("the drop-in does not clear the shipped ExecStart")
	}
	upstream := strings.Fields(found[0][1])
	if _, ok := validNPMUpstream(upstream[len(upstream)-1]); !ok {
		t.Fatalf("the gate would refuse its own unit: %q", upstream[len(upstream)-1])
	}
}
