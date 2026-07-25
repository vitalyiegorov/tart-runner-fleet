package operations

import "time"

type RetryPolicy struct {
	Initial     time.Duration
	Maximum     time.Duration
	MaxAttempts int
	Jitter      func(attempt int, delay time.Duration) time.Duration
}

// DurableCleanupMaxAttempts is the escalation ceiling for owned runner cleanup,
// not a retry budget. ADR 0007 requires cleanup to keep retrying for as long as
// GitHub may legitimately refuse to remove a runner that is still executing a
// job, and GitHub's own maximum job duration is six hours; at the thirty-second
// backoff ceiling that is 720 attempts. Past it, no refusal can still be
// legitimate, so the operation dead-letters: it stops retrying invisibly and
// becomes a published dead letter carrying its classified reason. Cleanup is
// surfaced, never forced — nothing is deleted on its behalf.
const DurableCleanupMaxAttempts = 720

func DurableCleanupRetryPolicy(maximum time.Duration) RetryPolicy {
	return RetryPolicy{Maximum: maximum, MaxAttempts: DurableCleanupMaxAttempts}
}

func (p RetryPolicy) Next(attempt int, now time.Time) (time.Time, bool) {
	if p.MaxAttempts > 0 && attempt >= p.MaxAttempts {
		return time.Time{}, false
	}
	delay := p.Initial
	if delay <= 0 {
		delay = time.Second
	}
	for i := 1; i < attempt; i++ {
		if p.Maximum > 0 && delay >= p.Maximum/2 {
			delay = p.Maximum
			break
		}
		delay *= 2
	}
	if p.Maximum > 0 && delay > p.Maximum {
		delay = p.Maximum
	}
	if p.Jitter != nil {
		delay += p.Jitter(attempt, delay)
		if delay < 0 {
			delay = 0
		}
		if p.Maximum > 0 && delay > p.Maximum {
			delay = p.Maximum
		}
	}
	return now.Add(delay), true
}
