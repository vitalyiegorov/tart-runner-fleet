package config

import (
	"reflect"
	"strings"
	"testing"
)

func targetSchedulingClass(t *testing.T, target Target) string {
	t.Helper()
	field := reflect.ValueOf(target).FieldByName("SchedulingClass")
	if !field.IsValid() {
		t.Fatal("config.Target is missing SchedulingClass")
	}
	return field.String()
}

func priorityConfig(class string) string {
	return `{
      "baseVm":"linux-runner-base", "vmPrefix":"gha-linux",
      "pollSeconds":20, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048}],
      "macosBurst":{"enabled":false},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":3` + class + `}]
    }`
}

func TestDecodeNormalizesBoundedSchedulingClasses(t *testing.T) {
	standard, err := Decode(strings.NewReader(priorityConfig("")))
	if err != nil {
		t.Fatalf("Decode(standard) error = %v", err)
	}
	control, err := Decode(strings.NewReader(priorityConfig(`,"schedulingClass":"control-plane"`)))
	if err != nil {
		t.Fatalf("Decode(control-plane) error = %v", err)
	}
	if got := targetSchedulingClass(t, standard.Targets[0]); got != "standard" {
		t.Fatalf("default scheduling class = %q, want standard", got)
	}
	if got := targetSchedulingClass(t, control.Targets[0]); got != "control-plane" {
		t.Fatalf("control-plane scheduling class = %q", got)
	}
}

func TestDecodeRejectsUnknownSchedulingClass(t *testing.T) {
	if _, err := Decode(strings.NewReader(priorityConfig(`,"schedulingClass":"urgent"`))); err == nil {
		t.Fatal("Decode() accepted an unbounded scheduling class")
	}
}
