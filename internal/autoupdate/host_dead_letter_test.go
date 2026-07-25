package autoupdate

import (
	"context"
	"errors"
	"testing"
)

// TestDeadLetteredCleanupDoesNotBlockProductionUpdates replays the 2026-07-25
// production deadlock. A drain deregister operation dead-lettered against a
// GitHub registration that can never be deleted (`deregister:runner_busy`,
// attempts 835). Treating that parked operation as "busy" made the fleet
// permanently non-quiescent, so the automatic updater logged
// "apply production release: prepare update: autoupdate: fleet is not quiescent"
// every 300s for hours and refused to install the very release that bounds the
// phantom. A dead letter is parked awaiting an operator: it holds no lease, is
// not claimable, and cannot be interrupted by a generation swap.
func TestDeadLetteredCleanupDoesNotBlockProductionUpdates(t *testing.T) {
	host, command, current, _, _ := hostFixture(t)
	command.current = `{"data":{"controllerVersion":"v1","controllerMode":"authority","queues":[],"instances":[],` +
		`"operations":{"retrying":0,"dead":1,"failures":[{"kind":"deregister","code":"deregister:runner_busy","count":1,"attempts":835}]}}}`
	if err := host.ensureQuiescent(context.Background(), current); err != nil {
		t.Fatalf("dead-lettered cleanup blocked the update: %v", err)
	}
}

// TestParkedInstanceRowDoesNotBlockProductionUpdates covers the other half of
// the same deadlock: the phantom's durable instance row published
// `fleet_instances{profile="maestro"} 1`, so the instance gate kept returning
// ErrBusy even once the dead letter stopped counting. An instance whose only
// outstanding work is a dead letter and whose VM is not running is parked too.
func TestParkedInstanceRowDoesNotBlockProductionUpdates(t *testing.T) {
	host, command, current, _, _ := hostFixture(t)
	command.current = `{"data":{"controllerVersion":"v1","controllerMode":"authority","queues":[],` +
		`"instances":[{"profile":"maestro","count":1}],"operations":{"retrying":0,"dead":1,` +
		`"deadLetters":[{"operationId":"op-ea9b705d234ad29f14e79b6d","kind":"deregister","code":"deregister:runner_busy",` +
		`"resourceId":"trf-maestro-096ffcb3a52d8624","attempts":835,"parked":true}]}}}`
	if err := host.ensureQuiescent(context.Background(), current); err != nil {
		t.Fatalf("parked instance row blocked the update: %v", err)
	}
}

// TestRealCapacityStillBlocksProductionUpdates pins the conservative half of the
// same decision. ADR 0011 exists so a release can never interrupt live work, so
// every instance the fleet cannot prove is parked keeps deferring activation.
func TestRealCapacityStillBlocksProductionUpdates(t *testing.T) {
	for name, status := range map[string]string{
		"retrying operation": `{"data":{"controllerVersion":"v1","controllerMode":"authority","operations":{"retrying":1}}}`,
		"queued job":         `{"data":{"controllerVersion":"v1","controllerMode":"authority","queues":[{"jobs":1}]}}`,
		"live vm":            `{"data":{"controllerVersion":"v1","controllerMode":"authority","instances":[{"count":1}]}}`,
		"dead letter that is not parked": `{"data":{"controllerVersion":"v1","controllerMode":"authority","instances":[{"count":1}],` +
			`"operations":{"dead":1,"deadLetters":[{"operationId":"op-1","resourceId":"trf-a","parked":false}]}}}`,
		"one parked row beside one live vm": `{"data":{"controllerVersion":"v1","controllerMode":"authority","instances":[{"count":2}],` +
			`"operations":{"dead":1,"deadLetters":[{"operationId":"op-1","resourceId":"trf-a","parked":true}]}}}`,
		"two dead letters for one parked row": `{"data":{"controllerVersion":"v1","controllerMode":"authority","instances":[{"count":2}],` +
			`"operations":{"dead":2,"deadLetters":[{"operationId":"op-1","resourceId":"trf-a","parked":true},` +
			`{"operationId":"op-2","resourceId":"trf-a","parked":true}]}}}`,
	} {
		host, command, current, _, _ := hostFixture(t)
		command.current = status
		if err := host.ensureQuiescent(context.Background(), current); !errors.Is(err, ErrBusy) {
			t.Fatalf("%s: quiescence error=%v want ErrBusy", name, err)
		}
	}
}
