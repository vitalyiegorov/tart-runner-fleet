package githubscaleset

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// These two interfaces are the complete preview-library surface used here.
// Churn in github.com/actions/scaleset is therefore confined to this file.
type officialMessages interface {
	GetMessage(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error)
	DeleteMessage(context.Context, int) error
	AcquireJobs(context.Context, []int64) ([]int64, error)
}
type officialJIT interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

type ScaleSetConfig struct {
	Messages       officialMessages
	JIT            officialJIT
	ScaleSetID     int
	MaxCapacity    int
	PollTimeout    time.Duration
	RequestTimeout time.Duration
	InitialCursor  int
}

type ScaleSet struct {
	messages                    officialMessages
	jit                         officialJIT
	id, capacity                int
	pollTimeout, requestTimeout time.Duration
	mu                          sync.Mutex
	cursor                      int
	outstanding                 bool
}

func NewScaleSet(c ScaleSetConfig) (*ScaleSet, error) {
	if c.Messages == nil || c.JIT == nil || c.ScaleSetID <= 0 || c.MaxCapacity < 0 || c.InitialCursor < 0 {
		return nil, errors.New("message client, JIT client, positive scale-set ID, and nonnegative capacity are required")
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = 55 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}
	return &ScaleSet{messages: c.Messages, jit: c.JIT, id: c.ScaleSetID, capacity: c.MaxCapacity, pollTimeout: c.PollTimeout, requestTimeout: c.RequestTimeout, cursor: c.InitialCursor}, nil
}

func (s *ScaleSet) LastMessageID() int { s.mu.Lock(); defer s.mu.Unlock(); return s.cursor }

// Next performs one official ~50 second long poll. The cursor advances only
// after DeleteMessage succeeds, so a crash or handler failure is redelivered.
func (s *ScaleSet) Next(ctx context.Context) (*PendingMessage, error) {
	s.mu.Lock()
	if s.outstanding {
		s.mu.Unlock()
		return nil, ErrUnackedMessage
	}
	cursor := s.cursor
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.pollTimeout)
	defer cancel()
	m, err := s.messages.GetMessage(ctx, cursor, s.capacity)
	if err != nil {
		return nil, fmt.Errorf("long-poll scale-set messages: %w", err)
	}
	if m == nil {
		return nil, nil
	}
	d := Demand{MessageID: m.MessageID}
	if m.Statistics != nil {
		d.Assigned = m.Statistics.TotalAssignedJobs
		d.Running = m.Statistics.TotalRunningJobs
	}
	d.Events = appendEvents(nil, m)
	s.mu.Lock()
	s.outstanding = true
	s.mu.Unlock()
	p := &PendingMessage{Demand: d}
	p.nack = func() { s.mu.Lock(); s.outstanding = false; s.mu.Unlock() }
	p.ack = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
		defer cancel()
		if err := s.messages.DeleteMessage(ctx, d.MessageID); err != nil {
			return fmt.Errorf("acknowledge scale-set message %d: %w", d.MessageID, err)
		}
		s.mu.Lock()
		s.cursor = d.MessageID
		s.outstanding = false
		s.mu.Unlock()
		return nil
	}
	return p, nil
}

// Handle makes the required ordering explicit: durable work is committed
// first, then the GitHub message is deleted as its acknowledgement. A failed
// commit leaves both the cursor and remote message untouched for redelivery.
func (s *ScaleSet) Handle(ctx context.Context, commit func(context.Context, Demand) error) error {
	if commit == nil {
		return errors.New("commit callback is required")
	}
	m, err := s.Next(ctx)
	if err != nil || m == nil {
		return err
	}
	if err := commit(ctx, m.Demand); err != nil {
		m.Nack()
		return fmt.Errorf("commit demand message %d: %w", m.Demand.MessageID, err)
	}
	return m.Ack(ctx)
}

func appendEvents(dst []JobEvent, m *scaleset.RunnerScaleSetMessage) []JobEvent {
	for _, v := range m.JobAvailableMessages {
		if v != nil {
			dst = append(dst, eventFromBase(JobAvailable, v.JobMessageBase))
		}
	}
	for _, v := range m.JobAssignedMessages {
		if v != nil {
			dst = append(dst, eventFromBase(JobAssigned, v.JobMessageBase))
		}
	}
	for _, v := range m.JobStartedMessages {
		if v != nil {
			e := eventFromBase(JobStarted, v.JobMessageBase)
			e.RunnerID = v.RunnerID
			e.RunnerName = v.RunnerName
			dst = append(dst, e)
		}
	}
	for _, v := range m.JobCompletedMessages {
		if v != nil {
			e := eventFromBase(JobCompleted, v.JobMessageBase)
			e.RunnerID = v.RunnerID
			e.RunnerName = v.RunnerName
			e.Result = v.Result
			dst = append(dst, e)
		}
	}
	return dst
}

func eventFromBase(kind JobEventKind, v scaleset.JobMessageBase) JobEvent {
	return JobEvent{Kind: kind, RunnerRequestID: v.RunnerRequestID, Owner: v.OwnerName, Repository: v.RepositoryName, WorkflowRunID: v.WorkflowRunID, JobID: v.JobID, EventName: v.EventName, Labels: append([]string(nil), v.RequestLabels...), QueueTime: v.QueueTime}
}

func (s *ScaleSet) AcquireJobs(ctx context.Context, requestIDs []int64) (AcquireResult, error) {
	requested := append([]int64(nil), requestIDs...)
	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()
	acquired, err := s.messages.AcquireJobs(ctx, requested)
	if err != nil {
		return AcquireResult{}, fmt.Errorf("acquire scale-set jobs: %w", err)
	}
	set := make(map[int64]struct{}, len(acquired))
	for _, id := range acquired {
		set[id] = struct{}{}
	}
	result := AcquireResult{Acquired: append([]int64(nil), acquired...)}
	for _, id := range requested {
		if _, ok := set[id]; !ok {
			result.Rejected = append(result.Rejected, id)
		}
	}
	return result, nil
}

// AcquireAndGenerateJIT enforces the JobAvailable protocol order.
func (s *ScaleSet) AcquireAndGenerateJIT(ctx context.Context, requestID int64, name, workFolder string) (*JITSecret, error) {
	result, err := s.AcquireJobs(ctx, []int64{requestID})
	if err != nil {
		return nil, err
	}
	if len(result.Acquired) != 1 || result.Acquired[0] != requestID {
		return nil, fmt.Errorf("request %d: %w", requestID, ErrJobNotAcquired)
	}
	return s.GenerateJIT(ctx, name, workFolder)
}

func (s *ScaleSet) Close(ctx context.Context) error {
	closer, ok := s.messages.(interface{ Close(context.Context) error })
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()
	if err := closer.Close(ctx); err != nil {
		return fmt.Errorf("close scale-set message session: %w", err)
	}
	return nil
}

func (s *ScaleSet) GenerateJIT(ctx context.Context, name, workFolder string) (*JITSecret, error) {
	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()
	config, err := s.jit.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: workFolder}, s.id)
	if err != nil {
		return nil, fmt.Errorf("generate JIT runner configuration: %w", err)
	}
	if config == nil || config.EncodedJITConfig == "" {
		return nil, errors.New("GitHub returned an empty JIT runner configuration")
	}
	encoded := config.EncodedJITConfig
	config.EncodedJITConfig = ""
	return NewJITSecret(encoded), nil
}
