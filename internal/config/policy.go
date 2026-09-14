package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Policy is the bounded, credential-free projection of an effective
// configuration that a scheduling decision can depend on. It exists because a
// node's load-bearing settings were knowable only by reading its file over SSH:
// the mac studio's `macosBurst` lacked `mixedPlatformAdmission`, the mac mini's
// had it, and the resulting Linux starvation ran for weeks across three
// episodes while every single-node check passed (issue #304, ADR 0053).
//
// Three rules govern what may appear here, and they are the whole reason this
// is a hand-written projection rather than a dump of Config:
//
//   - Every field is read by a decision. If nothing in the scheduler, the
//     admission guardrails, or the placement of a label consults it, it does not
//     belong here — an unbounded document becomes another thing nobody reads.
//   - Nothing secret, and nothing path-like. Credentials, key files, app and
//     installation identifiers, socket paths and the state directory are
//     excluded: the first class must never be published, and the second
//     legitimately differs per host, so including it would make every pair of
//     nodes disagree and teach an operator to ignore the answer.
//   - Nothing is omitted. Every field is encoded whatever its value, because
//     the drift this exists to catch is a MISSING key: absent and deliberately
//     false are indistinguishable in a configuration file, and they must not be
//     indistinguishable here.
//
// The encoding is deterministic — struct fields in declaration order, map keys
// sorted by encoding/json, every slice sorted here — so two nodes running the
// same policy produce the same bytes and therefore the same digest.
type Policy struct {
	// HostBudget is the static ceiling on this node's total admission envelope,
	// read by every tick that sizes the envelope it admits against (ADR 0034).
	HostBudget PolicyResources `json:"hostBudget"`
	// LinuxCapacity is the Linux-only cap and instance count the same envelope
	// computation reads, which under `elasticHostEnvelope` stops being the shared
	// cross-platform bound and becomes a Linux one (ADR 0018).
	LinuxCapacity PolicyLinuxCapacity `json:"linuxCapacity"`
	// HostPressureFloors are the guardrail thresholds the host probe judges
	// pressure against on every tick; a node below any of them admits nothing.
	HostPressureFloors PolicyFloors `json:"hostPressureFloors"`
	// ElasticHostEnvelope selects ADR 0018's measured envelope over the static
	// one, which changes what every admission decision is taken against.
	ElasticHostEnvelope bool `json:"elasticHostEnvelope"`
	// PressureMemoryAccounting selects the kernel memory-pressure signal the
	// available-memory floor above is evaluated with.
	PressureMemoryAccounting bool `json:"pressureMemoryAccounting"`
	// MacOSBurst is the whole cross-platform admission policy: whether macOS runs
	// here at all, whether Linux may fill the residual envelope beside a live
	// macOS cohort, and the vectors the two macOS profiles are charged. This is
	// the block issue #304 was about, and `mixedPlatformAdmission` is projected
	// explicitly false when the file omits it.
	MacOSBurst PolicyMacOSBurst `json:"macosBurst"`
	// PollSeconds is the tick interval: how often any of these decisions is taken
	// at all, and therefore how quickly a queue drains.
	PollSeconds int `json:"pollSeconds"`
	// ReservationAgeSeconds is the age at which a queued demand becomes the
	// global FIFO head capacity is reserved for (ADR 0017, ADR 0045).
	ReservationAgeSeconds int `json:"reservationAgeSeconds"`
	// SessionYield is when this node stops holding the scale-set sessions GitHub
	// binds jobs to, which decides whether a blocked node withholds a sibling's
	// work (ADR 0047).
	SessionYield PolicySessionYield `json:"sessionYield"`
	// UpdateDrain is when this node refuses admission on purpose to reach a
	// pending generation (ADR 0048) — a node admitting nothing for a reason no
	// other node shares.
	UpdateDrain PolicyUpdateDrain `json:"updateDrain"`
	// Targets is the per-target admission policy keyed by `type:slug`: the
	// repository cap every admission is checked against and the scheduling class
	// that orders it.
	Targets map[string]PolicyTarget `json:"targets"`
	// Profiles is every Linux profile's resource vector and per-profile cap,
	// keyed by profile ID. The scheduler charges the vector stated here, never a
	// label, so two nodes disagreeing on one is two nodes admitting different
	// work behind the same name.
	Profiles map[string]PolicyProfile `json:"profiles"`
	// AdvertisedLabels is every runner label this node's scale sets publish to
	// GitHub, sorted. It is what decides which jobs can arrive here at all.
	AdvertisedLabels []string `json:"advertisedLabels"`
	// GuestArch is the architecture component of every label the node derives,
	// so it decides the names above (ADR 0032 §1, ADR 0034 §4).
	GuestArch string `json:"guestArch"`
	// ExecutorBackend is the execution technology admission provisions onto; an
	// empty backend on a Linux node is an observe-only node that admits nothing.
	ExecutorBackend string `json:"executorBackend"`
	// ExecutorImage is the OCI reference every runner container is created from,
	// tag included and credentials never: a registry reference carries none, and
	// the podman binary path beside it is deliberately not projected.
	ExecutorImage string `json:"executorImage"`
	// KVMProfiles are the profile IDs whose containers are granted `/dev/kvm`,
	// sorted. A profile missing here runs an Android emulator without
	// acceleration, which is the same silent per-node divergence as #304's.
	KVMProfiles []string `json:"kvmProfiles"`
	// BaseImageCapabilities is what each of the node's two guest images declares
	// it provides, sorted. ADR 0034's cross-node parity rule is decided on
	// exactly this against the labels above.
	BaseImageCapabilities PolicyCapabilities `json:"baseImageCapabilities"`
	// CanonicalJobInventory selects the REST job inventory and truthful
	// advertised capacity over scale-set lookahead, which changes what demand the
	// scheduler believes exists (ADR 0015).
	CanonicalJobInventory bool `json:"canonicalJobInventory"`
	// SerialLogEnabled is whether a Linux guest's console is written somewhere
	// durable. It decides nothing about admission and everything about whether a
	// dead guest leaves evidence, which is why it is a bool and not a path: the
	// directory is per-host, the posture is fleet-wide (ADR 0046).
	SerialLogEnabled bool `json:"serialLogEnabled"`
	// Priority is the declared tier order and escalation bound the planner ranks
	// queued demand by (ADR 0037).
	Priority PolicyPriority `json:"priority"`
}

// PolicyResources is one resource vector. `memoryMb` keeps the spelling the
// configuration file uses, so an operator reading a diff sees the key they must
// edit.
type PolicyResources struct {
	CPU       int `json:"cpu"`
	MemoryMiB int `json:"memoryMb"`
}

// PolicyLinuxCapacity is the Linux cap of the admission envelope.
type PolicyLinuxCapacity struct {
	CPU          int `json:"cpu"`
	MemoryMiB    int `json:"memoryMb"`
	MaxInstances int `json:"maxInstances"`
}

// PolicyFloors are the host guardrails admission is refused under. A zero is a
// floor the operator did not set, and it is encoded rather than omitted for the
// same reason every other field is: an unset floor and an absent key are the
// same drift.
type PolicyFloors struct {
	MinFreeDiskGiB        int     `json:"minFreeDiskGb"`
	MinAvailableMemoryMiB int     `json:"minAvailableMemoryMb"`
	MaxSwapUsedMiB        int     `json:"maxSwapUsedMb"`
	MaxLoadAverage        float64 `json:"maxLoadAverage"`
	MinCPUIdlePercent     float64 `json:"minCpuIdlePercent"`
}

// PolicyMacOSBurst is the cross-platform admission policy in full. Every field
// is read by a tick: `enabled` by whether macOS is considered at all,
// `admissionPolicy` by ADR 0014's exclusivity, `mixedPlatformAdmission` by
// whether Linux may fill the residual envelope beside a live macOS cohort,
// `mixedProfileCohorts` by whether two macOS profiles may run side by side, the
// two vectors by what each admission is charged, and `nestedVirtualization` by
// what the guest can host.
//
// The base VM name and the shared directory are not projected: both are local
// artifacts of one host. What the image CARRIES is projected, one field up.
type PolicyMacOSBurst struct {
	Enabled                bool          `json:"enabled"`
	AdmissionPolicy        string        `json:"admissionPolicy"`
	MixedPlatformAdmission bool          `json:"mixedPlatformAdmission"`
	MixedProfileCohorts    bool          `json:"mixedProfileCohorts"`
	NestedVirtualization   bool          `json:"nestedVirtualization"`
	Builder                PolicyProfile `json:"builder"`
	Maestro                PolicyProfile `json:"maestro"`
}

// PolicyProfile is one profile's charged vector and its own cap.
type PolicyProfile struct {
	CPU       int `json:"cpu"`
	MemoryMiB int `json:"memoryMb"`
	MaxActive int `json:"maxActive"`
}

// PolicyTarget is one target's admission policy.
type PolicyTarget struct {
	SchedulingClass string `json:"schedulingClass"`
	MaxActive       int    `json:"maxActive"`
}

// PolicySessionYield is the withdrawal policy of this node's broker sessions.
type PolicySessionYield struct {
	Enabled           bool `json:"enabled"`
	BlockedForSeconds int  `json:"blockedForSeconds"`
	HealthyForSeconds int  `json:"healthyForSeconds"`
}

// PolicyUpdateDrain is how this node reaches the quiescence its own update
// needs.
type PolicyUpdateDrain struct {
	Enabled           bool `json:"enabled"`
	PendingForSeconds int  `json:"pendingForSeconds"`
	MaxWaitSeconds    int  `json:"maxWaitSeconds"`
	CooldownSeconds   int  `json:"cooldownSeconds"`
}

// PolicyCapabilities is what each of a node's two guest images declares, sorted.
// A node has two images and each answers only for the scale sets whose profile
// it boots, so they are never merged.
type PolicyCapabilities struct {
	Linux []string `json:"linux"`
	MacOS []string `json:"macos"`
}

// PolicyPriority is the declared tier order and the escalation bound. Tier names
// are in DECLARED order rather than sorted, because the order is the ranking: a
// sorted list would compare equal for two nodes that rank the same tiers
// differently.
type PolicyPriority struct {
	EscalateAfterSeconds int      `json:"escalateAfterSeconds"`
	Tiers                []string `json:"tiers"`
}

// ProjectPolicy derives the published policy from an effective configuration.
// It reads only the decoded Config, never the host, so `fleet config policy`
// answers for a file on a machine that will never run it — the same property
// ADR 0034 requires of `fleet config validate`.
func ProjectPolicy(cfg Config) Policy {
	policy := Policy{
		HostBudget: PolicyResources{CPU: cfg.HostBudget.CPU, MemoryMiB: cfg.HostBudget.MemoryMiB},
		LinuxCapacity: PolicyLinuxCapacity{CPU: cfg.Linux.Capacity.CPU, MemoryMiB: cfg.Linux.Capacity.MemoryMiB,
			MaxInstances: cfg.Linux.MaxInstances},
		HostPressureFloors: PolicyFloors{MinFreeDiskGiB: cfg.Guards.MinFreeDiskGiB,
			MinAvailableMemoryMiB: cfg.Guards.MinAvailableMemoryMiB, MaxSwapUsedMiB: cfg.Guards.MaxSwapUsedMiB,
			MaxLoadAverage: cfg.Guards.MaxLoadAverage, MinCPUIdlePercent: cfg.Guards.MinCPUIdlePercent},
		ElasticHostEnvelope:      cfg.Guards.ElasticHostEnvelope,
		PressureMemoryAccounting: cfg.Guards.PressureMemoryAccounting,
		MacOSBurst: PolicyMacOSBurst{Enabled: cfg.MacOS.Enabled,
			AdmissionPolicy:        string(cfg.MacOS.AdmissionPolicy),
			MixedPlatformAdmission: cfg.MacOS.MixedPlatformAdmission,
			MixedProfileCohorts:    cfg.MacOS.MixedProfileCohorts,
			NestedVirtualization:   cfg.MacOS.NestedVirtualization,
			Builder:                projectProfile(cfg.MacOS.Builder), Maestro: projectProfile(cfg.MacOS.Maestro)},
		PollSeconds:           int(cfg.PollInterval.Seconds()),
		ReservationAgeSeconds: int(cfg.ReservationAge.Seconds()),
		SessionYield: PolicySessionYield{Enabled: cfg.SessionYield.Enabled,
			BlockedForSeconds: int(cfg.SessionYield.BlockedFor.Seconds()),
			HealthyForSeconds: int(cfg.SessionYield.HealthyFor.Seconds())},
		UpdateDrain: PolicyUpdateDrain{Enabled: cfg.UpdateDrain.Enabled,
			PendingForSeconds: int(cfg.UpdateDrain.PendingFor.Seconds()),
			MaxWaitSeconds:    int(cfg.UpdateDrain.MaxWait.Seconds()),
			CooldownSeconds:   int(cfg.UpdateDrain.Cooldown.Seconds())},
		Targets:          projectTargets(cfg.Targets),
		Profiles:         projectProfiles(cfg.Linux.Profiles),
		AdvertisedLabels: advertisedLabels(cfg),
		GuestArch:        cfg.GuestArchOrDefault(),
		ExecutorBackend:  string(cfg.Executor.Backend),
		ExecutorImage:    cfg.Executor.Image,
		KVMProfiles:      sortedCopy(cfg.Executor.KVMProfiles),
		BaseImageCapabilities: PolicyCapabilities{Linux: sortedCopy(cfg.Linux.BaseImageCapabilities),
			MacOS: sortedCopy(cfg.MacOS.BaseImageCapabilities)},
		CanonicalJobInventory: cfg.GitHub.CanonicalJobInventory,
		SerialLogEnabled:      strings.TrimSpace(cfg.Linux.SerialLogDirectory) != "",
		Priority:              projectPriority(cfg.Priority),
	}
	return policy
}

func projectProfile(profile Profile) PolicyProfile {
	normalized := profile.normalized()
	return PolicyProfile{CPU: normalized.Resources.CPU, MemoryMiB: normalized.Resources.MemoryMiB,
		MaxActive: normalized.MaxActive}
}

func projectProfiles(profiles []Profile) map[string]PolicyProfile {
	projected := make(map[string]PolicyProfile, len(profiles))
	for _, profile := range profiles {
		projected[profile.ID] = projectProfile(profile)
	}
	return projected
}

func projectTargets(targets []Target) map[string]PolicyTarget {
	projected := make(map[string]PolicyTarget, len(targets))
	for _, target := range targets {
		normalized := target.normalized()
		projected[normalized.Type+":"+normalized.Slug] = PolicyTarget{
			SchedulingClass: string(normalized.SchedulingClass), MaxActive: normalized.MaxActive}
	}
	return projected
}

func projectPriority(priority Priority) PolicyPriority {
	tiers := make([]string, 0, len(priority.Tiers))
	for _, tier := range priority.Tiers {
		tiers = append(tiers, tier.Name)
	}
	return PolicyPriority{EscalateAfterSeconds: int(priority.EscalateAfter.Seconds()), Tiers: tiers}
}

// advertisedLabels is every label this node's scale sets publish, deduplicated
// and sorted. It is derived exactly as provisioning derives it, so the projected
// set is the set GitHub is told about rather than the set written in the file.
func advertisedLabels(cfg Config) []string {
	sets := cfg.ProfileLabelSets()
	seen := make(map[string]struct{})
	labels := make([]string, 0, len(sets))
	for _, scaleSet := range cfg.ScopedScaleSets() {
		for _, label := range sets[scaleSet.ScaleSet.Profile].Advertise(scaleSet.ScaleSet.Labels) {
			if _, duplicate := seen[label]; duplicate {
				continue
			}
			seen[label] = struct{}{}
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels
}

func sortedCopy(values []string) []string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	if copied == nil {
		return []string{}
	}
	return copied
}

// CanonicalJSON is the exact bytes the digest is taken over. Marshalling this
// type cannot fail — every field is a string, number, bool, or a map or slice of
// those — so there is no error to return and no unreachable branch to pretend to
// handle.
func (p Policy) CanonicalJSON() []byte {
	encoded, _ := json.Marshal(p)
	return encoded
}

// Digest identifies one policy as a hex sha256 of CanonicalJSON, so two nodes
// can be compared at a glance before anything is diffed. Readers print a prefix
// of it; the digest itself is never part of the bytes it is taken over.
func (p Policy) Digest() string {
	sum := sha256.Sum256(p.CanonicalJSON())
	return hex.EncodeToString(sum[:])
}

// Fields is the policy as decoded JSON, which is the form every consumer of it
// wants: the status document carries it verbatim beside its digest, and the diff
// walks it by key path without this package's Go types. Round-tripping bytes
// this package just produced cannot fail.
func (p Policy) Fields() map[string]any {
	fields := map[string]any{}
	_ = json.Unmarshal(p.CanonicalJSON(), &fields)
	return fields
}
