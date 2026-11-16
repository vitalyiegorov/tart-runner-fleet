package operations

import "time"

// OrphanRequeue contains identifiers and closed outcome values only, never REST payloads.
type OrphanRequeue struct {
	Scope      string
	Repository string
	RunID      int64
	RunAttempt int
	RetryAt    time.Time
	At         time.Time
	Outcome    string
}
