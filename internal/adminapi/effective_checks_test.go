package adminapi

import "testing"

// fleet.v1 grows by addition, so a client always talks to daemons that publish
// fewer checks than it knows about. Each Effective* accessor encodes the same
// contract for that case: an absent check is a pass, because a controller that
// cannot report a condition has not reported it failing. Getting this backwards
// would make every older daemon look broken the day a new check ships.
func TestEffectiveChecksTreatAnAbsentCheckAsAPass(t *testing.T) {
	older := Status{}
	for name, check := range map[string]Check{
		"reservation":   older.EffectiveReservationCheck(),
		"guest console": older.EffectiveGuestConsoleCheck(),
		"session yield": older.EffectiveSessionYieldCheck(),
		"update drain":  older.EffectiveUpdateDrainCheck(),
		"admission":     older.EffectiveAdmissionCheck(),
	} {
		if !check.OK {
			t.Fatalf("%s: an unpublished check failed", name)
		}
		if check.Reasons == nil {
			t.Fatalf("%s: reasons are nil, which encodes differently from the empty set", name)
		}
		if len(check.Reasons) != 0 {
			t.Fatalf("%s: an unpublished check invented reasons %v", name, check.Reasons)
		}
	}
}

// A published check is returned verbatim: the accessor must never soften a
// failure a daemon actually reported.
func TestEffectiveChecksReturnAPublishedCheckUnchanged(t *testing.T) {
	failing := Check{Reasons: []string{"the node said so"}}
	published := Status{
		ReservationCheck:  &failing,
		GuestConsoleCheck: &failing,
		SessionYieldCheck: &failing,
		UpdateDrainCheck:  &failing,
		AdmissionCheck:    &failing,
	}
	for name, check := range map[string]Check{
		"reservation":   published.EffectiveReservationCheck(),
		"guest console": published.EffectiveGuestConsoleCheck(),
		"session yield": published.EffectiveSessionYieldCheck(),
		"update drain":  published.EffectiveUpdateDrainCheck(),
		"admission":     published.EffectiveAdmissionCheck(),
	} {
		if check.OK {
			t.Fatalf("%s: a reported failure was softened into a pass", name)
		}
		if len(check.Reasons) != 1 || check.Reasons[0] != "the node said so" {
			t.Fatalf("%s: reasons = %v, want the daemon's own", name, check.Reasons)
		}
	}
}
