package podman

import (
	"context"
	"testing"
)

// What a guest prints is the guest's to choose. A job with sudo can replace `grep` or
// `cat` in its own guest, so the host keeps a bounded amount whatever it is sent, and
// still reports how the command ended.
func TestAnOutputLimitBoundsWhatTheHostKeeps(t *testing.T) {
	runner := ExecRunner{Binary: "/bin/sh", OutputLimit: 1024}
	output, err := runner.Run(context.Background(), "-c", "head -c 1048576 /dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1024 {
		t.Fatalf("kept %d bytes of a 1 MiB answer", len(output))
	}
	unbounded, err := ExecRunner{Binary: "/bin/sh"}.Run(context.Background(), "-c", "head -c 4096 /dev/zero")
	if err != nil || len(unbounded) != 4096 {
		t.Fatalf("a runner with no limit keeps what it always kept: %d, %v", len(unbounded), err)
	}
	if _, err := runner.Run(context.Background(), "-c", "exit 3"); err == nil {
		t.Fatal("a bounded runner still reports a failed command")
	}
}
