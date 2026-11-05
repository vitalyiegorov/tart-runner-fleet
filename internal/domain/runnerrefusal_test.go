package domain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRunnerRefusalReadsOnlyTheRunnersOwnLine(t *testing.T) {
	version, ok := ParseRunnerRefusal([]byte("An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.\n"))
	if !ok || version != "2.336.0" {
		t.Fatalf("got %q %v", version, ok)
	}
	for _, line := range []string{
		"", "2026-11-05 08:54:16Z: Listening for Jobs\n",
		"2026-11-05 08:54:16Z: Running job: Runner version v2.336.0 is deprecated and cannot receive messages\n",
		"An error occurred: Runner version v2.336 is deprecated and cannot receive messages\n",
		"An error occurred: something else\n",
	} {
		if version, ok := ParseRunnerRefusal([]byte(line)); ok {
			t.Fatalf("%q read as a refusal of %q", line, version)
		}
	}
}

// The command runs in a guest and takes no external value, and it must read the
// file guestbootstrap writes.
func TestTheRefusalCommandReadsTheBootstrapLogOnly(t *testing.T) {
	command := RunnerRefusalLogCommand()
	if len(command) != 3 || command[0] != "/bin/sh" || command[1] != "-c" {
		t.Fatalf("got %q", command)
	}
	if !strings.Contains(command[2], `"$HOME/actions-runner/.tart-runner-fleet/runner.log"`) || !strings.HasPrefix(command[2], "grep -m1 -E ") {
		t.Fatalf("got %q", command[2])
	}
}

// The whole observed line, in both the Go matcher and the guest's own grep, so a
// line that merely begins the same way is no refusal.
func TestOnlyTheCompleteRefusalLineMatchesInGoAndInGrep(t *testing.T) {
	const complete = "An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages."
	nearMisses := []string{
		"An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages",
		complete + " Update the runner.",
		complete + ".",
		"prefix " + complete,
	}
	for _, line := range nearMisses {
		if version, ok := ParseRunnerRefusal([]byte(line + "\n")); ok {
			t.Fatalf("%q read as a refusal of %q", line, version)
		}
	}
	if version, ok := ParseRunnerRefusal([]byte("Listening for Jobs\n" + complete + "\nlater\n")); !ok || version != "2.336.0" {
		t.Fatalf("got %q %v", version, ok)
	}

	home := t.TempDir()
	dir := filepath.Join(home, "actions-runner", ".tart-runner-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(lines ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "runner.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func() (string, error) {
		t.Helper()
		argv := RunnerRefusalLogCommand()
		command := exec.Command(argv[0], argv[1:]...)
		command.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		output, err := command.Output()
		return string(output), err
	}
	write(nearMisses...)
	if output, err := run(); err != nil || output != "" {
		t.Fatalf("grep matched a near miss: %q %v", output, err)
	}
	write(append(nearMisses, complete)...)
	output, err := run()
	if err != nil || output != complete+"\n" {
		t.Fatalf("grep must print exactly the complete line: %q %v", output, err)
	}
}

// A job can write that log, so what it writes must be bounded by the grammar and by the
// command: no version a status document could not carry, and no matter how many lines,
// one line of bounded length.
func TestAJobCannotMakeTheRefusalReadLarge(t *testing.T) {
	line := func(version string) string {
		return "An error occurred: Runner version v" + version + " is deprecated and cannot receive messages."
	}
	longest := "999999.999999.999999"
	if version, ok := ParseRunnerRefusal([]byte(line(longest) + "\n")); !ok || version != longest {
		t.Fatalf("got %q %v", version, ok)
	}
	if len(longest) > 128 {
		t.Fatalf("the longest version the grammar admits, %d bytes, must fit a status field", len(longest))
	}
	huge := strings.Repeat("9", 1000) + ".0.0"
	for _, version := range []string{"1234567.0.0", "0.1234567.0", "0.0.1234567", huge} {
		if got, ok := ParseRunnerRefusal([]byte(line(version) + "\n")); ok {
			t.Fatalf("a version of %d bytes was admitted as %d", len(version), len(got))
		}
	}

	home := t.TempDir()
	dir := filepath.Join(home, "actions-runner", ".tart-runner-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	flood := strings.Repeat(line("2.336.0")+"\n", 100_000) + line(huge) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "runner.log"), []byte(flood), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := RunnerRefusalLogCommand()
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := line("2.336.0") + "\n"; string(output) != want {
		t.Fatalf("a flood of %d bytes printed %d bytes: one line was all it may", len(flood), len(output))
	}
}
