package reconcile

type Mode string

const (
	Observe   Mode = "observe"
	Shadow    Mode = "shadow"
	Canary    Mode = "canary"
	Authority Mode = "authority"
)

func (m Mode) Valid() bool {
	switch m {
	case Observe, Shadow, Canary, Authority:
		return true
	default:
		return false
	}
}

type Evidence struct {
	FreshObservations bool
	ShadowMatches     bool
	CanaryPassed      bool
	RollbackReady     bool
}

func CanTransition(from, to Mode, evidence Evidence) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to || to == Observe {
		return true
	}
	if !evidence.FreshObservations || !evidence.RollbackReady {
		return false
	}
	switch {
	case from == Observe && to == Shadow:
		return true
	case from == Shadow && to == Canary:
		return evidence.ShadowMatches
	case from == Canary && to == Authority:
		return evidence.ShadowMatches && evidence.CanaryPassed
	default:
		return false
	}
}
