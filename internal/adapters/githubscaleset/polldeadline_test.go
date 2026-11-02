package githubscaleset

import (
	"context"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// slowBroker answers a long poll only after `answersAfter`, the way GitHub's
// broker does when it holds a poll for its ~50 s window plus transport time. It
// inspects the deadline it was handed instead of sleeping, so the replay is
// deterministic: a deadline shorter than the broker's answer is the
// client-side timeout every node logged as cause=timeout on 2026-11-02.
type slowBroker struct {
	answersAfter time.Duration
	granted      time.Duration
}

func (b *slowBroker) GetMessage(ctx context.Context, _ int, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, nil
	}
	b.granted = time.Until(deadline)
	if b.granted < b.answersAfter {
		return nil, context.DeadlineExceeded
	}
	return nil, nil
}
func (*slowBroker) DeleteMessage(context.Context, int) error              { return nil }
func (*slowBroker) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }

// TestIssue468ALongPollTheBrokerAnswersLateStillSucceeds replays the flood that
// #469's cause classification exposed: 13 of 13 statusless poll failures on the
// first two v0.1.836 nodes were cause=timeout. The default poll deadline was
// 55 s against a broker that holds a poll for up to ~50 s, so a poll answered
// a few seconds late failed. The default must cover the broker's hold plus an
// explicit transport margin.
func TestIssue468ALongPollTheBrokerAnswersLateStillSucceeds(t *testing.T) {
	broker := &slowBroker{answersAfter: 58 * time.Second}
	set, err := NewScaleSet(ScaleSetConfig{Messages: broker, JIT: &fakeScaleSet{}, ScaleSetID: 9, MaxCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Next(context.Background()); err != nil {
		t.Fatalf("a poll the broker answered after %s failed with a %s deadline: %v", broker.answersAfter, broker.granted.Round(time.Second), err)
	}
	if want := brokerLongPollHold + pollTransportMargin; broker.granted < want-time.Second {
		t.Fatalf("poll deadline = %s, want at least the broker hold plus margin, %s", broker.granted.Round(time.Second), want)
	}
	// An explicit PollTimeout still wins, so tests and operators keep control.
	explicit := &slowBroker{answersAfter: time.Second}
	set, err = NewScaleSet(ScaleSetConfig{Messages: explicit, JIT: &fakeScaleSet{}, ScaleSetID: 9, MaxCapacity: 1, PollTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Next(context.Background()); err != nil || explicit.granted > 5*time.Second {
		t.Fatalf("explicit PollTimeout not honoured: granted %s err %v", explicit.granted, err)
	}
}
