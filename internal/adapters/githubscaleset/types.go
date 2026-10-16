package githubscaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Freshness is explicit so a failed observation can never masquerade as an
// empty GitHub queue.
type Freshness uint8

const (
	Unavailable Freshness = iota
	Stale
	Fresh
)

type Repository struct{ Owner, Name string }

type WorkflowRun struct {
	ID         int64
	Repository Repository
	Status     string
	Attempt    int
	CreatedAt  time.Time
}

type WorkflowJob struct {
	ID         int64
	RunID      int64
	Repository Repository
	Name       string
	Status     string
	Labels     []string
	RunAttempt int
	CreatedAt  time.Time
	// QueueTimeExact is false when GitHub omitted both job timestamps and the
	// run creation time had to be used as an upper bound. Inferred timestamps
	// remain observable but must not trigger a runner queue SLO.
	QueueTimeExact bool
	StartedAt      time.Time
	CompletedAt    time.Time
}

type Runner struct {
	ID         int64
	Repository Repository
	Name       string
	Status     string
	Busy       bool
	Labels     []string
}

// Snapshot is immutable after construction. Accessors return values or copies.
type Snapshot struct {
	at      time.Time
	runs    map[int64]WorkflowRun
	jobs    map[int64]WorkflowJob
	runners map[int64]Runner
	queued  []int64
}

func (s *Snapshot) ObservedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.at
}
func (s *Snapshot) Run(id int64) (WorkflowRun, bool) {
	if s == nil {
		return WorkflowRun{}, false
	}
	v, ok := s.runs[id]
	return cloneRun(v), ok
}
func (s *Snapshot) Job(id int64) (WorkflowJob, bool) {
	if s == nil {
		return WorkflowJob{}, false
	}
	v, ok := s.jobs[id]
	return cloneJob(v), ok
}
func (s *Snapshot) Runner(id int64) (Runner, bool) {
	if s == nil {
		return Runner{}, false
	}
	v, ok := s.runners[id]
	return cloneRunner(v), ok
}
func (s *Snapshot) QueuedJobs() []WorkflowJob {
	if s == nil {
		return nil
	}
	out := make([]WorkflowJob, 0, len(s.queued))
	for _, id := range s.queued {
		out = append(out, cloneJob(s.jobs[id]))
	}
	return out
}
func cloneRun(v WorkflowRun) WorkflowRun { return v }
func cloneJob(v WorkflowJob) WorkflowJob { v.Labels = append([]string(nil), v.Labels...); return v }
func cloneRunner(v Runner) Runner        { v.Labels = append([]string(nil), v.Labels...); return v }

type Observation struct {
	Freshness Freshness
	Snapshot  *Snapshot
	Err       error
	// Budget is the installation's primary rate budget as the latest GitHub
	// response reported it, on success and on failure alike. It is unobserved
	// when no response carried the headers; an unobserved budget is never an
	// empty one.
	Budget Budget
}

// Budget is the primary rate limit window of the installation a refresh spent
// from. GitHub reports it on every response, so a refresh learns where the
// shared window stands even when another node spent it.
type Budget struct {
	Limit     int
	Remaining int
	Reset     time.Time
	Observed  bool
}

// budgetReserveDivisor keeps one tenth of the window for everything else that
// spends from the same installation: session creation, JIT credentials, the
// parked-set audit, and the sibling nodes. Below the reserve the REST inventory
// waits for the reset rather than taking the last of it.
const budgetReserveDivisor = 10

// Scarce reports whether an observed budget has fallen into its reserve. An
// unobserved budget is not scarce: nothing is known, and the caller keeps its
// ordinary pace rather than inventing a limit.
func (b Budget) Scarce() bool {
	return b.Observed && b.Remaining <= b.Limit/budgetReserveDivisor
}

// ParseBudget reads GitHub's primary rate-limit headers. The budget is observed
// only when both the remaining count and the reset instant parse; a limit
// header is optional because the reserve degrades to zero without it.
func ParseBudget(header http.Header) Budget {
	remaining, err := strconv.Atoi(strings.TrimSpace(header.Get("X-RateLimit-Remaining")))
	if err != nil || remaining < 0 {
		return Budget{}
	}
	reset, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64)
	if err != nil || reset <= 0 {
		return Budget{}
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(header.Get("X-RateLimit-Limit")))
	return Budget{Limit: max(limit, 0), Remaining: remaining, Reset: time.Unix(reset, 0).UTC(), Observed: true}
}

type TokenSource interface {
	Token(context.Context) (string, error)
}
type TokenSourceFunc func(context.Context) (string, error)

func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

type Clock interface{ Now() time.Time }
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// JITSecret deliberately has no serializable exported fields. Formatting is
// always redacted; Reveal should only be used at the runner process boundary.
type JITSecret struct {
	mu      sync.Mutex
	encoded string
}

func NewJITSecret(encoded string) *JITSecret { return &JITSecret{encoded: encoded} }
func (s *JITSecret) Reveal() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoded
}
func (s *JITSecret) Destroy() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.encoded = ""
	s.mu.Unlock()
}
func (*JITSecret) String() string   { return "[REDACTED]" }
func (*JITSecret) GoString() string { return "[REDACTED]" }
func (*JITSecret) MarshalJSON() ([]byte, error) {
	return nil, errors.New("JIT configuration must not be persisted")
}
func (*JITSecret) MarshalText() ([]byte, error) {
	return nil, errors.New("JIT configuration must not be persisted")
}
func (*JITSecret) MarshalBinary() ([]byte, error) {
	return nil, errors.New("JIT configuration must not be persisted")
}
func (*JITSecret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

type Demand struct {
	MessageID  int
	Statistics DemandStatistics
	// Assigned and Running remain for source compatibility. New admission
	// decisions use the complete Statistics snapshot.
	Assigned int
	Running  int
	Events   []JobEvent
}

type DemandStatistics struct {
	MessageID  int
	Available  int
	Acquired   int
	Assigned   int
	Running    int
	Registered int
	Busy       int
	Idle       int
}

type JobEventKind string

const (
	JobAvailable JobEventKind = "JobAvailable"
	JobAssigned  JobEventKind = "JobAssigned"
	JobStarted   JobEventKind = "JobStarted"
	JobCompleted JobEventKind = "JobCompleted"
)

// JobEvent preserves stable demand identity without retaining AcquireJobURL,
// which carries protocol details that must never enter persistence or logs.
type JobEvent struct {
	Kind            JobEventKind
	RunnerRequestID int64
	Owner           string
	Repository      string
	WorkflowRunID   int64
	JobID           string
	DisplayName     string
	WorkflowRef     string
	EventName       string
	Labels          []string
	QueueTime       time.Time
	RunnerID        int
	RunnerName      string
	Result          string
}

type AcquireResult struct{ Acquired, Rejected []int64 }

var ErrJobNotAcquired = errors.New("job was not acquired")

var ErrUnackedMessage = errors.New("previous scale-set message is not acknowledged")

type PendingMessage struct {
	Demand Demand
	ack    func(context.Context) error
	nack   func()
	mu     sync.Mutex
	done   bool
	err    error
}

// Nack abandons local ownership without deleting the remote message, allowing
// the same cursor/message to be long-polled again.
func (m *PendingMessage) Nack() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return
	}
	if m.nack != nil {
		m.nack()
	}
	m.done = true
}

func (m *PendingMessage) Ack(ctx context.Context) error {
	if m == nil || m.ack == nil {
		return fmt.Errorf("acknowledge message: %w", errors.New("nil message"))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return m.err
	}
	m.err = m.ack(ctx)
	m.done = m.err == nil
	return m.err
}
