package lifecycle

import (
	"errors"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
)

// The guest-capability failure vocabulary is closed for the reason ADR 0020
// gives for every other one: a bootstrap failure is persisted on a durable
// operation and rendered to operators, so it can never carry text a child
// process produced. It lives here rather than beside the runner-administration
// reasons in internal/adapters/githubscaleset because it is not a fact about
// GitHub — nothing was asked of GitHub — but about the image the daemon booted.
//
// The two reasons call for opposite repairs, which is the whole reason there are
// two. A missing capability means the image is wrong: rebuild or re-audit it
// against the seal step. An unverifiable manifest means the declaration is
// wrong, or was right and went stale when the image was last rebuilt; that stale
// case is the only thing this backstop catches which the configuration gates
// cannot.
const (
	ReasonGuestCapabilityMissing      = "guest_capability_missing"
	ReasonGuestCapabilityUnverifiable = "guest_capability_unverifiable"
)

// BootstrapFailureReasons lists the closed vocabulary.
func BootstrapFailureReasons() []string {
	return []string{ReasonGuestCapabilityMissing, ReasonGuestCapabilityUnverifiable}
}

// ValidBootstrapFailureReason guards the vocabulary at every recording site, so
// an unclassified string can never reach a durable operation or the admin API.
func ValidBootstrapFailureReason(reason string) bool {
	switch reason {
	case ReasonGuestCapabilityMissing, ReasonGuestCapabilityUnverifiable:
		return true
	default:
		return false
	}
}

// exitStatus is the part of a failed child process this package is allowed to
// read. `*os/exec.ExitError` satisfies it, and naming the one method rather than
// the concrete type keeps the classifier testable without executing anything.
type exitStatus interface{ ExitCode() int }

// guestCapabilityReason classifies the helper's exit status. The status is the
// entire channel on purpose: the helper's standard output and error are
// discarded because they may reflect the JIT configuration supplied on its
// standard input, and an exit code cannot.
func guestCapabilityReason(err error) string {
	var status exitStatus
	if !errors.As(err, &status) {
		return ""
	}
	switch status.ExitCode() {
	case guestbootstrap.ExitCapabilityMissing:
		return ReasonGuestCapabilityMissing
	case guestbootstrap.ExitCapabilityUnverifiable:
		return ReasonGuestCapabilityUnverifiable
	default:
		return ""
	}
}

// capabilityArguments appends the daemon's expectation to the helper's argument
// vector, and only when there is one: a guest that needs no capability is
// invoked with exactly the arguments it always was, which is what makes this
// feature a no-op for every image that predates it.
func capabilityArguments(args, capabilities []string) ([]string, error) {
	if len(capabilities) == 0 {
		return args, nil
	}
	for _, capability := range capabilities {
		if !guestbootstrap.ValidCapability(capability) {
			return nil, errors.New("guest capability is outside the closed vocabulary")
		}
	}
	return append(args, guestbootstrap.CapabilityFlag+"="+strings.Join(capabilities, ",")), nil
}
