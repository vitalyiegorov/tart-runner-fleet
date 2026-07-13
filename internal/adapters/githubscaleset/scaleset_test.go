package githubscaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type fakeScaleSet struct {
	messages                  []*scaleset.RunnerScaleSetMessage
	getErr, deleteErr, jitErr error
	gets                      []int
	deleted                   []int
	jitSetting                *scaleset.RunnerScaleSetJitRunnerSetting
	jitID                     int
	runner                    *scaleset.RunnerReference
	removedRunner             int64
	deadlineSeen              bool
	acquireResult             []int64
	acquireErr, closeErr      error
	acquireArgs               [][]int64
	closed                    int
	ops                       []string
}

func (f *fakeScaleSet) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return f.runner, f.getErr
}

func (f *fakeScaleSet) RemoveRunner(_ context.Context, id int64) error {
	f.removedRunner = id
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.runner = nil
	return nil
}

func (f *fakeScaleSet) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	_, f.deadlineSeen = ctx.Deadline()
	f.acquireArgs = append(f.acquireArgs, append([]int64(nil), ids...))
	f.ops = append(f.ops, "acquire")
	return append([]int64(nil), f.acquireResult...), f.acquireErr
}
func (f *fakeScaleSet) Close(ctx context.Context) error {
	_, f.deadlineSeen = ctx.Deadline()
	f.closed++
	return f.closeErr
}

func (f *fakeScaleSet) GetMessage(ctx context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	_, f.deadlineSeen = ctx.Deadline()
	f.gets = append(f.gets, last)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.messages) == 0 {
		return nil, nil
	}
	return f.messages[0], nil
}
func (f *fakeScaleSet) DeleteMessage(ctx context.Context, id int) error {
	_, f.deadlineSeen = ctx.Deadline()
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}
func (f *fakeScaleSet) GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	_, f.deadlineSeen = ctx.Deadline()
	f.jitSetting = s
	f.jitID = id
	f.ops = append(f.ops, "jit")
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "top-secret"}, nil
}

func TestScaleSetAckCursorRedeliveryAndJIT(t *testing.T) {
	f := &fakeScaleSet{messages: []*scaleset.RunnerScaleSetMessage{{MessageID: 7, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3, TotalRunningJobs: 1}}}}
	s, err := NewScaleSet(ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 9, MaxCapacity: 4, PollTimeout: time.Second, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Demand.MessageID != 7 || m.Demand.Assigned != 3 || m.Demand.Running != 1 || !f.deadlineSeen {
		t.Fatalf("unexpected demand: %+v", m.Demand)
	}
	if _, err = s.Next(context.Background()); !errors.Is(err, ErrUnackedMessage) {
		t.Fatalf("expected unacked error, got %v", err)
	}
	f.deleteErr = errors.New("temporary")
	if err = m.Ack(context.Background()); err == nil {
		t.Fatal("expected failed ack")
	}
	if s.LastMessageID() != 0 {
		t.Fatal("cursor advanced before successful ack")
	}
	f.deleteErr = nil
	if err = m.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = m.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.LastMessageID() != 7 || len(f.deleted) != 2 {
		t.Fatalf("cursor/deletes: %d/%v", s.LastMessageID(), f.deleted)
	}
	secret, err := s.GenerateJIT(context.Background(), "runner", "_work")
	if err != nil {
		t.Fatal(err)
	}
	if secret.Reveal() != "top-secret" || f.jitID != 9 || f.jitSetting.Name != "runner" {
		t.Fatal("bad JIT bridge")
	}
	if fmt.Sprint(secret) != "[REDACTED]" || fmt.Sprintf("%#v", secret) != "[REDACTED]" {
		t.Fatal("secret formatted")
	}
	if _, err := json.Marshal(secret); err == nil {
		t.Fatal("secret serialized")
	}
	if _, err := secret.MarshalText(); err == nil {
		t.Fatal("secret text serialized")
	}
	if _, err := secret.MarshalBinary(); err == nil {
		t.Fatal("secret binary serialized")
	}
	if secret.LogValue().String() != "[REDACTED]" {
		t.Fatal("secret logged")
	}
	secret.Destroy()
	if secret.Reveal() != "" {
		t.Fatal("secret not destroyed")
	}
}

func TestScaleSetRunnerRegistrationAndIdempotentDeregistration(t *testing.T) {
	fake := &fakeScaleSet{runner: &scaleset.RunnerReference{ID: 7, Name: "runner", RunnerScaleSetID: 9}}
	scale, err := NewScaleSet(ScaleSetConfig{Messages: fake, JIT: fake, Runners: fake, ScaleSetID: 9})
	if err != nil {
		t.Fatal(err)
	}
	registered, err := scale.Registered(context.Background(), "runner")
	if err != nil || !registered {
		t.Fatalf("Registered() = %v, %v", registered, err)
	}
	if err := scale.Deregister(context.Background(), "runner"); err != nil || fake.removedRunner != 7 {
		t.Fatalf("Deregister() = %v, removed=%d", err, fake.removedRunner)
	}
	registered, err = scale.Registered(context.Background(), "runner")
	if err != nil || registered {
		t.Fatalf("Registered(absent) = %v, %v", registered, err)
	}
	if err := scale.Deregister(context.Background(), "runner"); err != nil {
		t.Fatalf("Deregister(absent) = %v", err)
	}
}

func TestScaleSetRunnerAdministrationFailsClosed(t *testing.T) {
	fake := &fakeScaleSet{}
	withoutRunners, err := NewScaleSet(ScaleSetConfig{Messages: fake, JIT: fake, ScaleSetID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutRunners.Registered(context.Background(), "runner"); err == nil {
		t.Fatal("registration observation accepted without runner administration")
	}
	if err := withoutRunners.Deregister(context.Background(), "runner"); err == nil {
		t.Fatal("deregistration accepted without runner administration")
	}

	scale, err := NewScaleSet(ScaleSetConfig{Messages: fake, JIT: fake, Runners: fake, ScaleSetID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scale.Registered(context.Background(), "bad runner name"); err == nil {
		t.Fatal("invalid runner name accepted")
	}
	if err := scale.Deregister(context.Background(), "bad runner name"); err == nil {
		t.Fatal("invalid runner name accepted for removal")
	}
	want := errors.New("github unavailable")
	fake.getErr = want
	if _, err := scale.Registered(context.Background(), "runner"); !errors.Is(err, want) {
		t.Fatalf("Registered() error = %v", err)
	}
	if err := scale.Deregister(context.Background(), "runner"); !errors.Is(err, want) {
		t.Fatalf("Deregister() observation error = %v", err)
	}
	fake.getErr = nil
	fake.runner = &scaleset.RunnerReference{ID: 8, Name: "runner", RunnerScaleSetID: 9}
	fake.deleteErr = want
	if err := scale.Deregister(context.Background(), "runner"); !errors.Is(err, want) {
		t.Fatalf("Deregister() removal error = %v", err)
	}
}

func TestScaleSetHandleCommitsBeforeDeleteAndNacksFailure(t *testing.T) {
	f := &fakeScaleSet{messages: []*scaleset.RunnerScaleSetMessage{{MessageID: 8}}}
	s, _ := NewScaleSet(ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 1, PollTimeout: time.Second, RequestTimeout: time.Second})
	if err := s.Handle(context.Background(), nil); err == nil {
		t.Fatal("nil commit")
	}
	commitErr := errors.New("store failed")
	if err := s.Handle(context.Background(), func(context.Context, Demand) error {
		if len(f.deleted) != 0 {
			t.Fatal("deleted before commit")
		}
		return commitErr
	}); !errors.Is(err, commitErr) {
		t.Fatalf("commit error: %v", err)
	}
	if len(f.deleted) != 0 || s.LastMessageID() != 0 {
		t.Fatal("failed commit acknowledged")
	}
	if err := s.Handle(context.Background(), func(context.Context, Demand) error {
		if len(f.deleted) != 0 {
			t.Fatal("deleted before successful commit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != 8 {
		t.Fatal("not deleted after commit")
	}
	var nilPending *PendingMessage
	nilPending.Nack()
	m := &PendingMessage{}
	m.Nack()
	m.Nack()
}

func TestScaleSetHandleRedeliversAfterTransientAckFailure(t *testing.T) {
	f := &fakeScaleSet{messages: []*scaleset.RunnerScaleSetMessage{{MessageID: 9}}, deleteErr: errors.New("temporary delete failure")}
	s, _ := NewScaleSet(ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 1, PollTimeout: time.Second, RequestTimeout: time.Second})
	commits := 0
	commit := func(context.Context, Demand) error {
		commits++
		return nil
	}
	if err := s.Handle(context.Background(), commit); !errors.Is(err, f.deleteErr) {
		t.Fatalf("ack error: %v", err)
	}
	if s.LastMessageID() != 0 {
		t.Fatal("cursor advanced after failed acknowledgement")
	}
	f.deleteErr = nil
	if err := s.Handle(context.Background(), commit); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if commits != 2 || s.LastMessageID() != 9 || len(f.deleted) != 2 {
		t.Fatalf("commits=%d cursor=%d deletes=%v", commits, s.LastMessageID(), f.deleted)
	}
}

func TestScaleSetPreservesMixedEventsWithDeepCopies(t *testing.T) {
	queued := time.Unix(50, 0)
	labels := []string{"self-hosted", "arm64"}
	base := scaleset.JobMessageBase{RunnerRequestID: 91, OwnerName: "owner", RepositoryName: "repo", WorkflowRunID: 77, JobID: "job-uuid", EventName: "push", RequestLabels: labels, QueueTime: queued}
	f := &fakeScaleSet{messages: []*scaleset.RunnerScaleSetMessage{{MessageID: 12,
		JobAvailableMessages: []*scaleset.JobAvailable{{AcquireJobURL: "https://secret/acquire", JobMessageBase: base}, nil},
		JobAssignedMessages:  []*scaleset.JobAssigned{{JobMessageBase: base}},
		JobStartedMessages:   []*scaleset.JobStarted{{RunnerID: 5, RunnerName: "runner", JobMessageBase: base}},
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "success", RunnerID: 5, RunnerName: "runner", JobMessageBase: base}},
	}}}
	s, _ := NewScaleSet(ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 1, InitialCursor: 4})
	m, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	labels[0] = "mutated"
	base.RequestLabels[1] = "mutated"
	if len(m.Demand.Events) != 4 {
		t.Fatalf("events: %+v", m.Demand.Events)
	}
	for i, e := range m.Demand.Events {
		if e.RunnerRequestID != 91 || e.Owner != "owner" || e.Repository != "repo" || e.WorkflowRunID != 77 || e.JobID != "job-uuid" || e.EventName != "push" || e.QueueTime != queued || e.Labels[0] != "self-hosted" {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if m.Demand.Events[0].Kind != JobAvailable || m.Demand.Events[1].Kind != JobAssigned || m.Demand.Events[2].Kind != JobStarted || m.Demand.Events[3].Kind != JobCompleted {
		t.Fatal("event kinds")
	}
	if m.Demand.Events[2].RunnerID != 5 || m.Demand.Events[3].Result != "success" {
		t.Fatal("runner/completion fields")
	}
	if strings.Contains(fmt.Sprintf("%+v", m.Demand), "secret/acquire") {
		t.Fatal("AcquireJobURL leaked")
	}
	if f.gets[0] != 4 {
		t.Fatal("durable initial cursor not used")
	}
}

// Regression: the public scale-set API can report a job already assigned to
// the scale set without a runnerRequestId. That is a capacity signal, not a
// malformed event: the controller must derive stable local identity and start
// one preassigned runner instead of permanently nacking the broker message.
func TestScaleSetNormalizesPreassignedJobWithoutRunnerRequestID(t *testing.T) {
	assignedAt := time.Unix(75, 0).UTC()
	base := scaleset.JobMessageBase{OwnerName: "owner", RepositoryName: "repo", WorkflowRunID: 77,
		JobID: "328810ee-dcb6-52e5-9204-89672c6a2919", EventName: "workflow_dispatch",
		RequestLabels: []string{"self-hosted", "linux-small"}, ScaleSetAssignTime: assignedAt}
	f := &fakeScaleSet{messages: []*scaleset.RunnerScaleSetMessage{{MessageID: 13,
		Statistics:          &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base}},
	}}}
	s, _ := NewScaleSet(ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 1})
	m, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Demand.Events) != 1 {
		t.Fatalf("events = %#v", m.Demand.Events)
	}
	event := m.Demand.Events[0]
	if event.Kind != JobAvailable || event.RunnerRequestID <= 0 || event.QueueTime != assignedAt {
		t.Fatalf("preassigned event = %#v", event)
	}

	// Redelivery and later lifecycle messages for the same workflow job must
	// derive exactly the same positive identity.
	started := eventFromBase(JobStarted, base)
	if started.RunnerRequestID != event.RunnerRequestID {
		t.Fatalf("identity changed: assigned=%d started=%d", event.RunnerRequestID, started.RunnerRequestID)
	}
}

func TestAcquirePartialOrderingCloseAndErrors(t *testing.T) {
	f := &fakeScaleSet{acquireResult: []int64{1}}
	s, _ := NewScaleSet(ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 1})
	r, err := s.AcquireJobs(context.Background(), []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Acquired) != 1 || r.Acquired[0] != 1 || len(r.Rejected) != 1 || r.Rejected[0] != 2 {
		t.Fatalf("partial result: %+v", r)
	}
	f.acquireResult = nil
	if _, err = s.AcquireAndGenerateJIT(context.Background(), 2, "r", "w"); !errors.Is(err, ErrJobNotAcquired) {
		t.Fatalf("rejection: %v", err)
	}
	f.acquireResult = []int64{2}
	f.ops = nil
	secret, err := s.AcquireAndGenerateJIT(context.Background(), 2, "r", "w")
	if err != nil || secret.Reveal() == "" {
		t.Fatalf("acquire/JIT: %v", err)
	}
	if fmt.Sprint(f.ops) != "[acquire jit]" {
		t.Fatalf("order: %v", f.ops)
	}
	f.acquireErr = errors.New("acquire")
	if _, err = s.AcquireJobs(context.Background(), []int64{1}); err == nil {
		t.Fatal("acquire error")
	}
	if _, err = s.AcquireAndGenerateJIT(context.Background(), 1, "r", "w"); err == nil {
		t.Fatal("acquire/JIT error")
	}
	f.acquireErr = nil
	if err := s.Close(context.Background()); err != nil || f.closed != 1 {
		t.Fatal("close")
	}
	f.closeErr = errors.New("close")
	if s.Close(context.Background()) == nil {
		t.Fatal("close error")
	}
	withoutClose := &noCloseMessages{inner: f}
	s2, _ := NewScaleSet(ScaleSetConfig{Messages: withoutClose, JIT: f, ScaleSetID: 1})
	if err := s2.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type noCloseMessages struct{ inner *fakeScaleSet }

func (n *noCloseMessages) GetMessage(c context.Context, a, b int) (*scaleset.RunnerScaleSetMessage, error) {
	return n.inner.GetMessage(c, a, b)
}
func (n *noCloseMessages) DeleteMessage(c context.Context, id int) error {
	return n.inner.DeleteMessage(c, id)
}
func (n *noCloseMessages) AcquireJobs(c context.Context, ids []int64) ([]int64, error) {
	return n.inner.AcquireJobs(c, ids)
}

func TestScaleSetErrorsAndDefaults(t *testing.T) {
	if _, err := NewScaleSet(ScaleSetConfig{}); err == nil {
		t.Fatal("expected validation")
	}
	f := &fakeScaleSet{}
	s, err := NewScaleSet(ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.Next(context.Background()); err != nil || m != nil {
		t.Fatalf("empty poll: %v %v", m, err)
	}
	if err := s.Handle(context.Background(), func(context.Context, Demand) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.getErr = errors.New("poll")
	if _, err = s.Next(context.Background()); err == nil {
		t.Fatal("poll error missing")
	}
	f.getErr = nil
	f.messages = []*scaleset.RunnerScaleSetMessage{{MessageID: 2}}
	m, err := s.Next(context.Background())
	if err != nil || m.Demand.Assigned != 0 {
		t.Fatal("nil statistics")
	}
	_ = m.Ack(context.Background())
	f.jitErr = errors.New("jit")
	if _, err = s.GenerateJIT(context.Background(), "r", "w"); err == nil {
		t.Fatal("jit error missing")
	}
	f.jitErr = nil
	f.messages = nil
	fakeEmpty := &emptyJIT{fakeScaleSet: *f}
	s.jit = fakeEmpty
	if _, err = s.GenerateJIT(context.Background(), "r", "w"); err == nil {
		t.Fatal("empty JIT missing")
	}
	var nilMessage *PendingMessage
	if nilMessage.Ack(context.Background()) == nil {
		t.Fatal("nil ack")
	}
}

type emptyJIT struct{ fakeScaleSet }

func (f *emptyJIT) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return nil, nil
}

type fixedRNG float64

func (r fixedRNG) Float64() float64 { return float64(r) }

func TestClassifyAndRetryPolicy(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for status, want := range map[int]ErrorKind{401: Authentication, 403: Authorization, 404: NotFound, 409: Conflict, 422: Validation, 429: RateLimited, 500: Server, 418: Unexpected} {
		resp := &http.Response{StatusCode: status, Header: http.Header{"X-Github-Request-Id": []string{"rid"}}}
		if status == 429 {
			resp.Header.Set("Retry-After", "3")
		}
		err := ClassifyResponse(resp, now, nil)
		var api *APIError
		if !errors.As(err, &api) || api.Kind != want || api.RequestID != "rid" {
			t.Fatalf("%d: %#v", status, api)
		}
		if api.Error() == "" || api.Unwrap() == nil {
			t.Fatal("error contract")
		}
	}
	if ClassifyResponse(nil, now, errors.New("x")).Error() != "x" {
		t.Fatal("nil response")
	}
	if ParseRetryAfter("5", now) != 5*time.Second || ParseRetryAfter("-1", now) != 0 || ParseRetryAfter("bad", now) != 0 {
		t.Fatal("seconds parsing")
	}
	date := now.Add(4 * time.Second).Format(http.TimeFormat)
	if ParseRetryAfter(date, now) != 4*time.Second || ParseRetryAfter(now.Format(http.TimeFormat), now) != 0 {
		t.Fatal("date parsing")
	}
	p := RetryPolicy{Base: time.Second, Max: 3 * time.Second, RNG: fixedRNG(1)}
	if d, ok := p.Delay(3, &APIError{Kind: Server}); !ok || d != 3*time.Second {
		t.Fatalf("delay %v %v", d, ok)
	}
	if d, ok := p.Delay(0, &APIError{Kind: RateLimited, RetryAfter: 7 * time.Second}); !ok || d != 7*time.Second {
		t.Fatal("retry-after")
	}
	if _, ok := p.Delay(0, &APIError{Kind: Validation}); ok {
		t.Fatal("validation retried")
	}
	if d, ok := p.Delay(0, &APIError{Kind: Authorization, RetryAfter: 2 * time.Second}); !ok || d != 2*time.Second {
		t.Fatal("secondary rate limit not retried")
	}
	if _, ok := p.Delay(0, errors.New("transport")); ok {
		t.Fatal("transport classified")
	}
	for _, rng := range []fixedRNG{-1, 2} {
		d, ok := RetryPolicy{RNG: rng}.Delay(-1, &APIError{Kind: Server})
		if !ok || d < 500*time.Millisecond || d > time.Second {
			t.Fatal("jitter clamp")
		}
	}
}

func TestSnapshotCopiesAndTokenFunc(t *testing.T) {
	s := &Snapshot{at: time.Unix(1, 0), runs: map[int64]WorkflowRun{1: {ID: 1}}, jobs: map[int64]WorkflowJob{2: {ID: 2, Labels: []string{"a"}}}, runners: map[int64]Runner{3: {ID: 3, Labels: []string{"b"}}}, queued: []int64{2}}
	if s.ObservedAt().Unix() != 1 {
		t.Fatal()
	}
	if _, ok := s.Run(1); !ok {
		t.Fatal()
	}
	if _, ok := s.Job(2); !ok {
		t.Fatal()
	}
	if _, ok := s.Runner(3); !ok {
		t.Fatal()
	}
	jobs := s.QueuedJobs()
	jobs[0].Labels[0] = "changed"
	again, _ := s.Job(2)
	if again.Labels[0] != "a" {
		t.Fatal("snapshot mutable")
	}
	var nilSnap *Snapshot
	if !nilSnap.ObservedAt().IsZero() || nilSnap.QueuedJobs() != nil {
		t.Fatal("nil snapshot")
	}
	if _, ok := nilSnap.Run(1); ok {
		t.Fatal()
	}
	if _, ok := nilSnap.Job(1); ok {
		t.Fatal()
	}
	if _, ok := nilSnap.Runner(1); ok {
		t.Fatal()
	}
	token, err := TokenSourceFunc(func(context.Context) (string, error) { return "t", nil }).Token(context.Background())
	if token != "t" || err != nil {
		t.Fatal()
	}
	if NewJITSecret("x").String() != "[REDACTED]" || (*JITSecret)(nil).Reveal() != "" {
		t.Fatal()
	}
	(*JITSecret)(nil).Destroy()
	if !strings.Contains((&APIError{Kind: Server, Status: 500}).Error(), "server") || (*APIError)(nil).Error() != "<nil>" || (*APIError)(nil).Unwrap() != nil {
		t.Fatal()
	}
}
