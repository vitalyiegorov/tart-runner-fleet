package operations

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("state conflict")
	ErrLeaseHeld = errors.New("lease held")
	ErrLeaseLost = errors.New("lease lost")
	ErrInvalid   = errors.New("invalid operation")
	ErrUncertain = errors.New("observation uncertain")
)

type State = domain.InstanceState

const (
	StatePlanned       = domain.InstancePlanned
	StateCloning       = domain.InstanceCloning
	StateBooting       = domain.InstanceBooting
	StateReachable     = domain.InstanceReachable
	StateRegistering   = domain.InstanceRegistering
	StateOnlineIdle    = domain.InstanceOnlineIdle
	StateAssigned      = domain.InstanceAssigned
	StateRunning       = domain.InstanceRunning
	StateDraining      = domain.InstanceDraining
	StateDeregistering = domain.InstanceDeregistering
	StateStopping      = domain.InstanceStopping
	StateDeleted       = domain.InstanceDeleted
	StateFailed        = domain.InstanceFailed
)

func ValidState(s State) bool {
	switch s {
	case StatePlanned, StateCloning, StateBooting, StateReachable, StateRegistering,
		StateOnlineIdle, StateAssigned, StateRunning, StateDraining, StateDeregistering,
		StateStopping, StateDeleted, StateFailed:
		return true
	default:
		return false
	}
}

type OperationStatus string

const (
	OperationPending   OperationStatus = "pending"
	OperationClaimed   OperationStatus = "claimed"
	OperationCompleted OperationStatus = "completed"
	OperationDead      OperationStatus = "dead"
)

type Ownership struct {
	ControllerID string `json:"controller_id"`
	ResourceID   string `json:"resource_id"`
	OperationID  string `json:"operation_id"`
}

func (o Ownership) Valid() bool {
	return o.ControllerID != "" && o.ResourceID != "" && o.OperationID != ""
}

type Instance struct {
	ID         string
	Repo       string
	Platform   domain.Platform
	Profile    domain.ProfileID
	Route      domain.Route
	Resources  domain.Resources
	Demand     domain.DemandKey
	State      State
	Version    int64
	DrainPhase int
	Ownership  Ownership
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (i Instance) SchedulingMetadataValid() bool {
	empty := i.Repo == "" && i.Platform == "" && i.Profile == "" && i.Route == "" && i.Resources == (domain.Resources{}) && i.Demand == (domain.DemandKey{})
	if empty {
		return true
	}
	if i.Repo == "" || (i.Platform != domain.PlatformLinux && i.Platform != domain.PlatformMacOS) || i.Profile == "" || i.Route == "" ||
		i.Resources.CPU <= 0 || i.Resources.MemoryMB <= 0 || i.Resources.Slots <= 0 || i.Demand.Validate() != nil {
		return false
	}
	return i.Demand.Repo == i.Repo
}

type Operation struct {
	ID             string
	IdempotencyKey string
	EffectKey      string
	Kind           string
	ResourceID     string
	Payload        json.RawMessage
	Status         OperationStatus
	Attempts       int
	AvailableAt    time.Time
	LeaseOwner     string
	LeaseUntil     time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DependsOn      []string
}

func (o Operation) Valid() bool {
	return o.ID != "" && o.IdempotencyKey != "" && o.EffectKey != "" &&
		o.Kind != "" && o.ResourceID != "" && !o.AvailableAt.IsZero() && o.DependenciesValid()
}

func (o Operation) DependenciesValid() bool {
	seen := make(map[string]struct{}, len(o.DependsOn))
	for _, dependency := range o.DependsOn {
		if dependency == "" || dependency == o.ID {
			return false
		}
		if _, duplicate := seen[dependency]; duplicate {
			return false
		}
		seen[dependency] = struct{}{}
	}
	return true
}

type Transition struct {
	InstanceID      string
	ExpectedState   State
	ExpectedVersion int64
	NextState       State
	DrainPhase      int
	LastError       string
	Operation       Operation
}

const (
	DrainPhaseStoppedRecovery  = 2
	DrainPhaseInactiveRecovery = 3
)

type Lease struct {
	Name      string
	Owner     string
	Token     int64
	ExpiresAt time.Time
}

type InstanceIntent struct {
	ExpectedVersion int64
	ExpectedState   State
	Instance        Instance
}

type SchedulerState struct {
	Version           int64
	Data              json.RawMessage
	Reservations      json.RawMessage
	DeficitRoundRobin json.RawMessage
	ObservationCursor string
}

type Plan struct {
	ID                       string
	ExpectedSchedulerVersion int64
	Scheduler                SchedulerState
	Instances                []InstanceIntent
	Operations               []Operation
	CreatedAt                time.Time
}

func (p Plan) Digest() ([32]byte, error) {
	encoded, err := json.Marshal(p)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func (p Plan) Valid() bool {
	if p.ID == "" || p.CreatedAt.IsZero() || p.Scheduler.Version != p.ExpectedSchedulerVersion+1 {
		return false
	}
	for _, intent := range p.Instances {
		if intent.Instance.ID == "" || !ValidState(intent.Instance.State) || !intent.Instance.Ownership.Valid() || !intent.Instance.SchedulingMetadataValid() {
			return false
		}
		if intent.ExpectedVersion >= 0 && (!ValidState(intent.ExpectedState) || !intent.ExpectedState.CanTransitionTo(intent.Instance.State)) {
			return false
		}
	}
	for _, operation := range p.Operations {
		if !operation.Valid() {
			return false
		}
	}
	return !p.hasDependencyCycleOrSelf()
}

func (p Plan) hasDependencyCycleOrSelf() bool {
	graph := make(map[string][]string, len(p.Operations))
	for _, operation := range p.Operations {
		seen := map[string]bool{}
		for _, dependency := range operation.DependsOn {
			if dependency == "" || dependency == operation.ID || seen[dependency] {
				return true
			}
			seen[dependency] = true
			graph[operation.ID] = append(graph[operation.ID], dependency)
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, dependency := range graph[id] {
			if _, local := graph[dependency]; local && visit(dependency) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	for id := range graph {
		if visit(id) {
			return true
		}
	}
	return false
}

type DeletionConfirmation struct {
	Fresh          bool
	RunnerInactive bool
	JobsInactive   bool
	ObservedAt     time.Time
}

type DemandEventKind string

const (
	DemandJobAvailable DemandEventKind = "JobAvailable"
	DemandJobAssigned  DemandEventKind = "JobAssigned"
	DemandJobStarted   DemandEventKind = "JobStarted"
	DemandJobCompleted DemandEventKind = "JobCompleted"
)

type DemandEvent struct {
	Kind            DemandEventKind `json:"kind"`
	RunnerRequestID int64           `json:"runner_request_id"`
	Owner           string          `json:"owner,omitempty"`
	Repository      string          `json:"repository,omitempty"`
	WorkflowRunID   int64           `json:"workflow_run_id,omitempty"`
	JobID           string          `json:"job_id,omitempty"`
	DisplayName     string          `json:"display_name,omitempty"`
	WorkflowRef     string          `json:"workflow_ref,omitempty"`
	EventName       string          `json:"event_name,omitempty"`
	Labels          []string        `json:"labels,omitempty"`
	QueueTime       time.Time       `json:"queue_time,omitempty"`
	RunnerID        int             `json:"runner_id,omitempty"`
	RunnerName      string          `json:"runner_name,omitempty"`
	Result          string          `json:"result,omitempty"`
}

func (e DemandEvent) Valid() bool {
	if e.RunnerRequestID <= 0 {
		return false
	}
	switch e.Kind {
	case DemandJobAvailable, DemandJobAssigned, DemandJobStarted, DemandJobCompleted:
		return true
	default:
		return false
	}
}

type DemandRecord struct {
	ScaleSetID      int64
	RunnerRequestID int64
	Status          DemandEventKind
	Owner           string
	Repository      string
	WorkflowRunID   int64
	JobID           string
	DisplayName     string
	WorkflowRef     string
	LogicalKey      string
	EventName       string
	Labels          []string
	QueueTime       time.Time
	FirstQueueTime  time.Time
	WorkflowJobID   int64
	RunAttempt      int
	RunnerID        int
	RunnerName      string
	Result          string
	UpdatedAt       time.Time
}

// DemandStatistics is GitHub's authoritative point-in-time view of one
// runner scale set. It bounds admission but never erases durable demand.
type DemandStatistics struct {
	MessageID  int64
	Available  int
	Acquired   int
	Assigned   int
	Running    int
	Registered int
	Busy       int
	Idle       int
	ObservedAt time.Time
}

func (s DemandStatistics) Valid() bool {
	return s.MessageID > 0 && s.Available >= 0 && s.Acquired >= 0 && s.Assigned >= 0 && s.Running >= 0 &&
		s.Registered >= 0 && s.Busy >= 0 && s.Idle >= 0
}

// GitHubJobObservation is the stable REST identity used to enrich broker
// events. A zero WorkflowJobID is never accepted, preventing guessed joins.
type GitHubJobObservation struct {
	WorkflowJobID  int64
	Owner          string
	Repository     string
	WorkflowRunID  int64
	RunAttempt     int
	DisplayName    string
	WorkflowRef    string
	Labels         []string
	Status         string
	CreatedAt      time.Time
	QueueTimeExact bool
}

func (j GitHubJobObservation) Valid() bool {
	return j.WorkflowJobID > 0 && j.Owner != "" && j.Repository != "" && j.WorkflowRunID > 0 &&
		j.RunAttempt > 0 && j.DisplayName != "" && !j.CreatedAt.IsZero()
}

func (c DeletionConfirmation) Safe(now time.Time, maxAge time.Duration) bool {
	return c.Fresh && c.RunnerInactive && c.JobsInactive && !c.ObservedAt.IsZero() &&
		!c.ObservedAt.After(now) && now.Sub(c.ObservedAt) <= maxAge
}
