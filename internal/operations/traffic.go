package operations

import (
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// Closed reasons a guest's network counters were not read (ADR 0080). An
// unmeasured sample carries one of these and never a byte count.
const (
	TrafficExecFailed  = "exec_failed"
	TrafficExecTimeout = "exec_timeout"
	TrafficUnreadable  = "unreadable"
	TrafficNoCommand   = "no_command"
)

// TrafficSample is one instance's guest interface counters, read once at the
// end of its job, with the identity the instance already carried. An
// ephemeral guest boots for one job, so the counters are that job's traffic.
type TrafficSample struct {
	InstanceID string
	Repo       string
	Platform   domain.Platform
	Profile    domain.ProfileID
	StartedAt  time.Time
	SampledAt  time.Time
	// Measured is false when the counters could not be read. ReceiveBytes and
	// TransmitBytes are then zero and must never be reported as a measurement.
	Measured      bool
	ReceiveBytes  int64
	TransmitBytes int64
	Unmeasured    string
}

// Valid reports whether the sample is internally consistent: a measured
// sample has non-negative counters and no reason, an unmeasured one has a
// closed reason and no counters.
func (s TrafficSample) Valid() bool {
	if s.InstanceID == "" || s.SampledAt.IsZero() {
		return false
	}
	if s.Measured {
		return s.Unmeasured == "" && s.ReceiveBytes >= 0 && s.TransmitBytes >= 0
	}
	switch s.Unmeasured {
	case TrafficExecFailed, TrafficExecTimeout, TrafficUnreadable, TrafficNoCommand:
		return s.ReceiveBytes == 0 && s.TransmitBytes == 0
	default:
		return false
	}
}

// TrafficRecord is a stored sample joined with the job GitHub ran on that
// runner, when the scale-set messages named one. The workflow fields are empty
// for a runner no recorded job was assigned to.
type TrafficRecord struct {
	TrafficSample
	ScaleSetID  int64
	RunID       int64
	WorkflowRef string
	JobName     string
}
