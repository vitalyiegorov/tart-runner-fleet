// Package adminapi defines the stable, read-only operator API shared by
// fleetd and fleetctl. These DTOs are compatibility contracts; persistence and
// domain structs must not leak into this package.
package adminapi

import "time"

const (
	APIVersion  = "fleet.v1"
	StatusPath  = "/v1/status"
	HealthPath  = "/healthz"
	ReadyPath   = "/readyz"
	MetricsPath = "/metrics"
)

type StatusEnvelope struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	GeneratedAt time.Time `json:"generatedAt"`
	Revision    uint64    `json:"revision"`
	Data        Status    `json:"data"`
	Warnings    []Warning `json:"warnings"`
}

type Status struct {
	ControllerVersion  string           `json:"controllerVersion"`
	ControllerMode     string           `json:"controllerMode"`
	HostMode           string           `json:"hostMode"`
	LastLoopTick       time.Time        `json:"lastLoopTick"`
	LastSuccessfulTick time.Time        `json:"lastSuccessfulTick"`
	Live               Check            `json:"live"`
	Ready              Check            `json:"ready"`
	QueueSLO           *Check           `json:"queueSlo,omitempty"`
	Queues             []Queue          `json:"queues"`
	Instances          []Instance       `json:"instances"`
	Observations       []Observation    `json:"observations"`
	Operations         OperationSummary `json:"operations"`
	HostPressure       HostPressure     `json:"hostPressure"`
}

type HostPressure struct {
	AvailableMemoryMiB int64   `json:"availableMemoryMiB"`
	FreeDiskGiB        int64   `json:"freeDiskGiB"`
	SwapUsedMiB        int64   `json:"swapUsedMiB"`
	SwapOuts           int64   `json:"swapOuts"`
	CPUIdlePercent     float64 `json:"cpuIdlePercent"`
	LoadAverage        float64 `json:"loadAverage"`
	AdmissionAllowed   bool    `json:"admissionAllowed"`
	AdmissionReason    string  `json:"admissionReason"`
}

type Check struct {
	OK      bool     `json:"ok"`
	Reasons []string `json:"reasons"`
}

// EffectiveQueueSLO keeps a new fleetctl compatible with an older fleet.v1
// daemon during atomic handoff. Older daemons did not emit queueSlo; their
// existing ready and queue fields remain authoritative until the new daemon is
// loaded.
func (s Status) EffectiveQueueSLO() Check {
	if s.QueueSLO == nil {
		return Check{OK: true, Reasons: []string{}}
	}
	return *s.QueueSLO
}

type Queue struct {
	Profile          string    `json:"profile"`
	Jobs             int       `json:"jobs"`
	OldestEnqueuedAt time.Time `json:"oldestEnqueuedAt"`
	OldestAgeSeconds float64   `json:"oldestAgeSeconds"`
}

type Instance struct {
	Profile   string `json:"profile"`
	Count     int    `json:"count"`
	CPU       int    `json:"cpu"`
	MemoryMiB int    `json:"memoryMiB"`
}

type Observation struct {
	Name       string    `json:"name"`
	Freshness  string    `json:"freshness"`
	ObservedAt time.Time `json:"observedAt"`
	AgeSeconds float64   `json:"ageSeconds"`
}

type OperationSummary struct {
	Retrying int `json:"retrying"`
	Dead     int `json:"dead"`
}

type Warning struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}
