package telemetry

import (
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// EmptyExitBackoffMetric is one queued demand whose instances keep exiting without
// its job (issue #493, ADR 0094).
type EmptyExitBackoffMetric struct {
	Demand, Scope, Profile string
	Exits                  int
	LastExitAt, RetryAt    time.Time
}

// ImageSignalMetric is one display-only finding about an image generation (ADR 0094). It
// reaches no check and no decision, so it has no judgement of its own to make.
type ImageSignalMetric struct {
	Name, Image, Detail string
}

// SetImageSignals publishes the whole set, so a finding that stopped being true goes.
func (h *Health) SetImageSignals(signals []ImageSignalMetric) {
	h.mu.Lock()
	h.revision++
	h.imageSignals = append([]ImageSignalMetric(nil), signals...)
	h.mu.Unlock()
}

func imageSignalRows(snapshot Snapshot) []adminapi.ImageSignal {
	if len(snapshot.ImageSignals) == 0 {
		return nil
	}
	rows := make([]adminapi.ImageSignal, 0, len(snapshot.ImageSignals))
	for _, signal := range snapshot.ImageSignals {
		rows = append(rows, adminapi.ImageSignal{Name: signal.Name, Image: signal.Image, Detail: signal.Detail})
	}
	return rows
}

// SetEmptyExitBackoffs publishes the whole set, so a demand that left the queue
// stops being reported.
func (h *Health) SetEmptyExitBackoffs(backoffs []EmptyExitBackoffMetric) {
	h.mu.Lock()
	h.revision++
	h.emptyExitBackoffs = append([]EmptyExitBackoffMetric(nil), backoffs...)
	h.mu.Unlock()
}

// emptyExitResult fails while any demand is over the bound. It never touches
// readiness or health, so it cannot defer an update (ADR 0052): a release may well be
// what ends the condition.
func emptyExitResult(backoffs []EmptyExitBackoffMetric) HealthResult {
	reasons := make([]string, 0, len(backoffs))
	for _, backoff := range backoffs {
		reasons = append(reasons, fmt.Sprintf("demand_empty_exit_backoff: %s (%s/%s): %d instances exited without a job, "+
			"the last at %s; next attempt no earlier than %s; the job at GitHub is untouched",
			backoff.Demand, backoff.Scope, backoff.Profile, backoff.Exits,
			backoff.LastExitAt.UTC().Format(time.RFC3339), backoff.RetryAt.UTC().Format(time.RFC3339)))
	}
	return HealthResult{OK: len(reasons) == 0, Reasons: reasons}
}

func emptyExitRows(snapshot Snapshot) []adminapi.EmptyExitBackoff {
	if len(snapshot.EmptyExitBackoffs) == 0 {
		return nil
	}
	rows := make([]adminapi.EmptyExitBackoff, 0, len(snapshot.EmptyExitBackoffs))
	for _, backoff := range snapshot.EmptyExitBackoffs {
		rows = append(rows, adminapi.EmptyExitBackoff{Demand: backoff.Demand, Scope: backoff.Scope, Profile: backoff.Profile,
			Exits: backoff.Exits, LastExitAt: backoff.LastExitAt, RetryAt: backoff.RetryAt})
	}
	return rows
}
