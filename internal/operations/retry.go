package operations

import "time"

type RetryPolicy struct {
	Initial     time.Duration
	Maximum     time.Duration
	MaxAttempts int
	Jitter      func(attempt int, delay time.Duration) time.Duration
}

func DurableCleanupRetryPolicy(maximum time.Duration) RetryPolicy {
	return RetryPolicy{Maximum: maximum}
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
