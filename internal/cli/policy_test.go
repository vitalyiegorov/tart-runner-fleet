package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// policyNodeJSON is one node's whole configuration, parameterised by the block
// issue #304 turned on. `burst` is spliced into `macosBurst` so a node can state
// the key, state it false, or — the 2026-08-23 mac studio — not have it at all.
func policyNodeJSON(scaleSet, burst string) string {
	return `{"baseVm":"linux","vmPrefix":"gha","pollSeconds":20,"maxLinuxWhenMacosIdle":1,` +
		`"maxLinuxCpu":2,"maxLinuxMemoryMb":4096,"linuxReservationAgeSeconds":300,"minFreeDiskGb":1,` +
		`"linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048}],` +
		`"macosBurst":{"enabled":false` + burst + `},"github":{"scaleSets":[{"profile":"small","name":"` +
		scaleSet + `","id":1,"maxCapacity":1,"labels":["self-hosted","linux-small"]}]},` +
		`"targets":[{"type":"repo","slug":"owner/repo","maxActive":1}]}`
}

// TestConfigPolicyNamesTheKeyOneNodeLacks is the regression issue #304 asks for
// by name: node A's `macosBurst` carries `mixedPlatformAdmission: true`, node
// B's does not carry the key at all, and nothing in either file is wrong on its
// own. The diff must name the key and print `true` against `false` — never
// `absent`, because the scheduler reads the missing key as false and the
// operator must see the value the tick actually used.
func TestConfigPolicyNamesTheKeyOneNodeLacks(t *testing.T) {
	dir := t.TempDir()
	mini := writeNode(t, dir, "mac-mini.json", policyNodeJSON("set-mini", `,"mixedPlatformAdmission":true`))
	studio := writeNode(t, dir, "mac-studio.json", policyNodeJSON("set-studio", ""))

	var stdout, stderr bytes.Buffer
	code := executeWith(context.Background(), []string{"config", "policy", mini, studio}, &stdout, &stderr, dependencies{})
	if code != exitDegraded {
		t.Fatalf("code=%d, want %d; stdout=%q stderr=%q", code, exitDegraded, stdout.String(), stderr.String())
	}
	line := ""
	for _, row := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(row, "macosBurst.mixedPlatformAdmission") {
			line = row
		}
	}
	if line == "" {
		t.Fatalf("the diff did not name macosBurst.mixedPlatformAdmission:\n%s", stdout.String())
	}
	if !strings.Contains(line, "true") || !strings.Contains(line, "false") {
		t.Fatalf("the diff row does not carry both values: %q", line)
	}
	if strings.Contains(line, policyValueAbsent) {
		t.Fatalf("a key the scheduler reads as false rendered as absent: %q", line)
	}
	for _, fragment := range []string{mini, studio, "policy key disagrees across 2 nodes"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Errorf("the diff does not say %q:\n%s", fragment, stdout.String())
		}
	}
}

// TestConfigPolicyAgreesWhenTheKeyIsStatedBothWays covers the other half of the
// same fact: a node that states the key false and a node that omits it are the
// same node, so they must not be reported as drift. A projection that omitted an
// unstated key would fail here, which is what makes the rule testable at all.
func TestConfigPolicyAgreesWhenTheKeyIsStatedBothWays(t *testing.T) {
	dir := t.TempDir()
	stated := writeNode(t, dir, "stated.json", policyNodeJSON("set-a", `,"mixedPlatformAdmission":false`))
	silent := writeNode(t, dir, "silent.json", policyNodeJSON("set-b", ""))
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "policy", stated, silent},
		&stdout, &stderr, dependencies{}); code != exitSuccess {
		t.Fatalf("code=%d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "no policy drift across 2 nodes") {
		t.Fatalf("identical policies were not reported identical:\n%s", stdout.String())
	}
}

// TestConfigPolicyPrintsOneNodesDeclaration is the single-path shape: the policy
// itself, digest included, so an operator can read what a node decides with and
// pipe it into anything.
func TestConfigPolicyPrintsOneNodesDeclaration(t *testing.T) {
	dir := t.TempDir()
	node := writeNode(t, dir, "node.json", policyNodeJSON("set", `,"mixedPlatformAdmission":true`))
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "policy", node},
		&stdout, &stderr, dependencies{}); code != exitSuccess {
		t.Fatalf("code=%d; stderr=%q", code, stderr.String())
	}
	document := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("single-path output is not JSON: %v\n%s", err, stdout.String())
	}
	digest, _ := document[adminapi.PolicyDigestKey].(string)
	if len(digest) != 64 {
		t.Fatalf("policyDigest = %#v", document[adminapi.PolicyDigestKey])
	}
	burst, ok := document["macosBurst"].(map[string]any)
	if !ok || burst["mixedPlatformAdmission"] != true {
		t.Fatalf("macosBurst = %#v", document["macosBurst"])
	}
	if _, leaked := document["stateDir"]; leaked {
		t.Fatalf("the state directory reached the published policy: %#v", document)
	}
}

// TestConfigPolicyDiffsTwoStatusDocuments is the mode that needs no file access
// and no SSH, which is the constraint #304 states: an operator with two
// `fleet status --output json` documents can name the key two nodes disagree on.
func TestConfigPolicyDiffsTwoStatusDocuments(t *testing.T) {
	dir := t.TempDir()
	first := writeNode(t, dir, "node-a-status.json", statusDocumentWith(t, "d1", true, nil))
	// The second node runs a release that projects a key the first has never
	// heard of. That is drift too, and it must render as `absent` rather than as
	// a value either node stated.
	second := writeNode(t, dir, "node-b-status.json", statusDocumentWith(t, "d2", false,
		map[string]any{"kvmProfiles": []any{"large"}}))
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "policy", first, second},
		&stdout, &stderr, dependencies{}); code != exitDegraded {
		t.Fatalf("code=%d, want %d; stdout=%q stderr=%q", code, exitDegraded, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "macosBurst.mixedPlatformAdmission") {
		t.Fatalf("two status documents were not compared by key:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "kvmProfiles") || !strings.Contains(stdout.String(), policyValueAbsent) {
		t.Fatalf("a key only one node projects was not reported absent on the other:\n%s", stdout.String())
	}
}

// TestConfigPolicyRefusesWhatItCannotRead keeps every usage and parse failure on
// exit 2: a diff that cannot read one side has not established agreement, and
// must never be confused with the exit 0 that says two nodes match.
func TestConfigPolicyRefusesWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	valid := writeNode(t, dir, "valid.json", policyNodeJSON("set", ""))
	garbage := writeNode(t, dir, "garbage.json", "{not json")
	// A status document from a daemon too old to declare a policy is read as a
	// configuration file and refused, rather than silently compared as empty.
	old := writeNode(t, dir, "old-status.json", `{"apiVersion":"fleet.v1","data":{"queues":[]}}`)
	for name, args := range map[string][]string{
		"no path":        {"config", "policy"},
		"absent path":    {"config", "policy", filepath.Join(dir, "missing.json")},
		"unparsable":     {"config", "policy", valid, garbage},
		"older document": {"config", "policy", valid, old},
	} {
		var stdout, stderr bytes.Buffer
		if code := executeWith(context.Background(), args, &stdout, &stderr, dependencies{}); code != exitUsage {
			t.Errorf("%s: code=%d, want %d; stdout=%q", name, code, exitUsage, stdout.String())
		}
		if stderr.Len() == 0 {
			t.Errorf("%s: refused silently", name)
		}
	}
}

// TestConfigUsageNamesBothSubcommands keeps the unknown-subcommand message
// pointing at everything the command does, so `fleet config polcy` does not read
// as "there is no such thing".
func TestConfigUsageNamesBothSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "polcy"}, &stdout, &stderr,
		dependencies{}); code != exitUsage {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "fleet config policy") {
		t.Fatalf("usage does not name the policy subcommand: %q", stderr.String())
	}
}

// TestStatusAndDoctorPublishThePolicyDigest is the at-a-glance half of ADR 0053:
// one line on the human status and one informational doctor row, so two nodes
// are comparable before anything is diffed.
func TestStatusAndDoctorPublishThePolicyDigest(t *testing.T) {
	status := healthyStatus()
	status.Data.Policy = &adminapi.Policy{Digest: "0123456789abcdef0123", Fields: map[string]any{
		"macosBurst": map[string]any{"mixedPlatformAdmission": false}}}
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var stdout bytes.Buffer
	if code := executeWith(context.Background(), []string{"status"}, &stdout, &bytes.Buffer{}, deps); code != exitSuccess {
		t.Fatalf("status code=%d", code)
	}
	if !strings.Contains(stdout.String(), "policy 0123456789ab\n") {
		t.Fatalf("status did not print the digest prefix:\n%s", stdout.String())
	}
	assertDoctorPasses(t, status, "policy")

	var doctorOut bytes.Buffer
	if code := executeWith(context.Background(), []string{"doctor"}, &doctorOut, &bytes.Buffer{}, deps); code != exitSuccess {
		t.Fatalf("doctor code=%d", code)
	}
	if !strings.Contains(doctorOut.String(), "0123456789ab (1 declared keys)") {
		t.Fatalf("doctor did not state the declaration:\n%s", doctorOut.String())
	}
}

// TestStatusAndDoctorSayNothingForAnOlderDaemon keeps the handoff rule: a daemon
// that declares no policy prints no digest line and still passes the row, which
// is what stops a rolling update reading as a fleet-wide fault.
func TestStatusAndDoctorSayNothingForAnOlderDaemon(t *testing.T) {
	status := healthyStatus()
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var stdout bytes.Buffer
	if code := executeWith(context.Background(), []string{"status"}, &stdout, &bytes.Buffer{}, deps); code != exitSuccess {
		t.Fatalf("status code=%d", code)
	}
	if strings.Contains(stdout.String(), "\npolicy ") {
		t.Fatalf("an undeclared policy printed a line:\n%s", stdout.String())
	}
	assertDoctorPasses(t, status, "policy")
	var doctorOut bytes.Buffer
	if code := executeWith(context.Background(), []string{"doctor"}, &doctorOut, &bytes.Buffer{}, deps); code != exitSuccess {
		t.Fatalf("doctor code=%d", code)
	}
	if !strings.Contains(doctorOut.String(), "not declared by this daemon") {
		t.Fatalf("doctor invented an answer for a daemon that gave none:\n%s", doctorOut.String())
	}
	if shortDigest("") != "-" || shortDigest("abc") != "abc" {
		t.Fatal("a short or absent digest is not rendered as itself")
	}
}

// statusDocumentWith is the `fleet status --output json` document of a node that
// declares one policy key, which is the artifact an operator copies out of a
// node they cannot reach a file on.
func statusDocumentWith(t *testing.T, digest string, mixed bool, extra map[string]any) string {
	t.Helper()
	fields := map[string]any{"macosBurst": map[string]any{"mixedPlatformAdmission": mixed}}
	for key, value := range extra {
		fields[key] = value
	}
	envelope := adminapi.StatusEnvelope{APIVersion: adminapi.APIVersion, Kind: "Status",
		Data: adminapi.Status{Policy: &adminapi.Policy{Digest: digest, Fields: fields}}}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
