package guestbootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// CapabilityManifestPath is where an image states, in its own words, what it
// provides. It is written once at seal time by the procedures in
// docs/BASE_IMAGE.md and docs/LINUX_BASE_IMAGE.md and read here, in the guest,
// by the one process that runs inside every ephemeral clone before the runner
// starts. Node C's base already carried the informal ancestor of this file,
// appended to `$HOME/.ci-base-manifest`; this is the same idea with a schema and
// a reader (ADR 0034, amendment 2026-08-04c §3).
const CapabilityManifestPath = "/usr/local/share/tart-runner-fleet/image-capabilities.json"

// CapabilityManifestSchema is the only shape this build understands. A manifest
// from the future is refused rather than read optimistically: the whole point of
// the check is that an answer it cannot trust is not an answer.
const CapabilityManifestSchema = 1

// CapabilityFlag is how the daemon tells a guest what was expected of it. The
// value is a comma-separated list of capability identifiers. The flag is passed
// only when the assigned scale set requires something, so a guest that needs no
// capability is invoked with exactly the argument vector it always was.
const CapabilityFlag = "--require-capabilities"

// A guest capability check has two distinct outcomes and they call for opposite
// repairs, so they leave the process with different statuses: rebuild or
// re-audit the image, versus correct a declaration that has gone stale. The
// daemon maps these back to a closed failure reason; see internal/lifecycle.
const (
	ExitCapabilityMissing      = 3
	ExitCapabilityUnverifiable = 4
)

// capabilityGrammar is the same vocabulary internal/config validates, restated
// here because this package is the guest side of the boundary and must not
// import the host's configuration to check one token.
var capabilityGrammar = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidCapability reports whether a token is a capability identifier.
func ValidCapability(capability string) bool { return capabilityGrammar.MatchString(capability) }

// CapabilityError is a failed guest capability check. Every value in it comes
// from the operator's own configuration or from the image's own manifest, and it
// is constructed before standard input is read at all, so unlike every other
// failure this helper can produce it is safe to print: it cannot contain the JIT
// configuration.
type CapabilityError struct {
	// MissingCapability is the required capability the image does not provide. It
	// is empty when the manifest itself could not be used, which is the stale
	// declaration case.
	MissingCapability string
	// Detail is the operator-facing explanation.
	Detail string
}

func (e *CapabilityError) Error() string {
	if e.MissingCapability != "" {
		return "guest image lacks required capability " + strconv.Quote(e.MissingCapability) + ": " + e.Detail
	}
	return "guest image capability manifest is unusable: " + e.Detail
}

// Unverifiable distinguishes "the image answered and the answer was no" from
// "the image could not answer".
func (e *CapabilityError) Unverifiable() bool { return e.MissingCapability == "" }

// capabilityManifest is the sealed guest manifest. Image and SealedAt are
// carried so a failure can quote provenance rather than only a verdict.
type capabilityManifest struct {
	SchemaVersion int      `json:"schemaVersion"`
	Image         string   `json:"image"`
	SealedAt      string   `json:"sealedAt"`
	Capabilities  []string `json:"capabilities"`
}

// checkCapabilities compares what the daemon expected of this guest against what
// the guest itself says it provides.
//
// It is fail-closed in both directions and deliberately so. Requiring nothing
// reads nothing, which keeps every guest that predates this feature on exactly
// today's path. Requiring something and being unable to read the answer is a
// failure, not a pass: a declaration that went stale when an image was rebuilt
// is the only thing this check catches which the configuration gates cannot, and
// treating an absent manifest as consent would catch nothing at all.
func checkCapabilities(path string, required []string) error {
	if len(required) == 0 {
		return nil
	}
	body, err := os.ReadFile(path) // #nosec G304 -- a fixed guest path, or a test override.
	if err != nil {
		return &CapabilityError{Detail: fmt.Sprintf("%s cannot be read; seal the image with one", path)}
	}
	var manifest capabilityManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return &CapabilityError{Detail: fmt.Sprintf("%s cannot be parsed as a capability manifest", path)}
	}
	if manifest.SchemaVersion != CapabilityManifestSchema {
		return &CapabilityError{Detail: fmt.Sprintf("%s declares schemaVersion %d; this release reads %d",
			path, manifest.SchemaVersion, CapabilityManifestSchema)}
	}
	if len(manifest.Capabilities) == 0 {
		return &CapabilityError{Detail: fmt.Sprintf("%s declares no capabilities, so it can answer for nothing", path)}
	}
	provided := make(map[string]struct{}, len(manifest.Capabilities))
	for _, capability := range manifest.Capabilities {
		provided[capability] = struct{}{}
	}
	for _, capability := range required {
		if _, ok := provided[capability]; ok {
			continue
		}
		return &CapabilityError{MissingCapability: capability,
			Detail: fmt.Sprintf("image %q sealed at %s declares %v", manifest.Image, manifest.SealedAt, manifest.Capabilities)}
	}
	return nil
}

// ParseCapabilityList reads the value of CapabilityFlag. An empty list is
// refused because the daemon never sends the flag without one, so an empty value
// means the argument vector was assembled wrongly.
func ParseCapabilityList(value string) ([]string, error) {
	if value == "" {
		return nil, errors.New("capability list is empty")
	}
	capabilities := strings.Split(value, ",")
	for _, capability := range capabilities {
		if !ValidCapability(capability) {
			return nil, fmt.Errorf("capability %q is not a lowercase identifier", capability)
		}
	}
	return capabilities, nil
}
