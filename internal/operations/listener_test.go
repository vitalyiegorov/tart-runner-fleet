package operations

import (
	"reflect"
	"testing"
	"time"
)

func TestEphemeralListenerCleanupPolicy(t *testing.T) {
	grace := 2 * time.Minute
	tests := []struct {
		name        string
		observation ListenerObservation
		want        ListenerDecision
	}{
		{name: "aged unregistered listener is cleaned", observation: ListenerObservation{Fresh: true, Age: 3 * time.Minute}, want: ListenerCleanup},
		{name: "registered listener is preserved", observation: ListenerObservation{Fresh: true, Age: 3 * time.Minute, Registered: true}, want: ListenerPreserve},
		{name: "assigned listener is preserved", observation: ListenerObservation{Fresh: true, Age: 3 * time.Minute, Assigned: true}, want: ListenerPreserve},
		{name: "young listener is preserved during bootstrap", observation: ListenerObservation{Fresh: true, Age: time.Minute}, want: ListenerPreserve},
		{name: "uncertain observation fails closed", observation: ListenerObservation{Age: 3 * time.Minute}, want: ListenerFailClosed},
		{name: "future age fails closed", observation: ListenerObservation{Fresh: true, Age: -time.Second}, want: ListenerFailClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideListenerCleanup(tt.observation, grace); got != tt.want {
				t.Fatalf("decision=%s", got)
			}
		})
	}
}

func TestRunnerLocaleEnvironmentIsUTF8AndCopied(t *testing.T) {
	first := RunnerLocaleEnvironment()
	want := []string{"LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("environment=%#v", first)
	}
	first[0] = "mutated"
	if reflect.DeepEqual(RunnerLocaleEnvironment(), first) {
		t.Fatal("environment aliases caller")
	}
}

func TestNegativeBootstrapGraceFailsClosed(t *testing.T) {
	if got := DecideListenerCleanup(ListenerObservation{Fresh: true}, -time.Second); got != ListenerFailClosed {
		t.Fatalf("decision=%s", got)
	}
}
