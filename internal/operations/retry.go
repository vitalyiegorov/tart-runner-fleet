package operations

import "time"

type RetryPolicy struct {
	Initial     time.Duration
	Maximum     time.Duration
	MaxAttempts int
	// MaxElapsed bounds how long an operation may go on retrying, measured from
	// the instant it was created. It exists because an attempt ceiling is not a
	// time bound: it only becomes one if every attempt is instantaneous, and a
	// failing attempt is not. On 2026-08-10 a drain spent the backend's full
	// 45-second command deadline on each try, so the 720-attempt ceiling that was
	// reasoned about as "six hours at the thirty-second backoff ceiling" was in
	// fact fifteen hours, and `fleet operations discharge` refused with
	// `operation_not_dead` for all of it. Bounding both makes the bound the
	// record already claimed the true one (ADR 0039).
	MaxElapsed time.Duration
	Jitter     func(attempt int, delay time.Duration) time.Duration
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

// DurableCleanupMaxElapsed is the same six hours as wall-clock, so the bound no
// longer depends on how long each attempt happens to take. Whichever ceiling is
// reached first ends the retrying.
const DurableCleanupMaxElapsed = 6 * time.Hour

func DurableCleanupRetryPolicy(maximum time.Duration) RetryPolicy {
	return RetryPolicy{Maximum: maximum, MaxAttempts: DurableCleanupMaxAttempts}
}

// Next reports when the given attempt may run and whether it may run at all.
// startedAt is the operation's creation instant; a zero value disables the
// elapsed bound rather than expiring the operation immediately, because an
// unknown start is not a long one.
func (p RetryPolicy) Next(attempt int, now, startedAt time.Time) (time.Time, bool) {
	if p.MaxAttempts > 0 && attempt >= p.MaxAttempts {
		return time.Time{}, false
	}
	_ = startedAt
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
