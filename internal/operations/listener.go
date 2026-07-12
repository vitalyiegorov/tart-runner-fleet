package operations

import "time"

type ListenerObservation struct {
	Fresh      bool
	Age        time.Duration
	Registered bool
	Assigned   bool
}

type ListenerDecision string

const (
	ListenerPreserve   ListenerDecision = "preserve"
	ListenerCleanup    ListenerDecision = "cleanup"
	ListenerFailClosed ListenerDecision = "fail_closed"
)

func DecideListenerCleanup(observation ListenerObservation, bootstrapGrace time.Duration) ListenerDecision {
	if !observation.Fresh || observation.Age < 0 || bootstrapGrace < 0 {
		return ListenerFailClosed
	}
	if observation.Age < bootstrapGrace || observation.Registered || observation.Assigned {
		return ListenerPreserve
	}
	return ListenerCleanup
}

var runnerLocaleEnvironment = [...]string{"LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8"}

func RunnerLocaleEnvironment() []string {
	return append([]string(nil), runnerLocaleEnvironment[:]...)
}
