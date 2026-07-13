package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

const controlPlaneClass = "control-plane"

func setRepoSchedulingClass(t *testing.T, cfg *Config, repo, class string) {
	t.Helper()
	field := reflect.ValueOf(cfg).Elem().FieldByName("RepoSchedulingClasses")
	if !field.IsValid() {
		t.Fatal("scheduler.Config is missing RepoSchedulingClasses")
	}
	if field.IsNil() {
		field.Set(reflect.MakeMap(field.Type()))
	}
	field.SetMapIndex(reflect.ValueOf(repo), reflect.ValueOf(class).Convert(field.Type().Elem()))
}

func TestControlPlaneYoungDemandWinsCrossPlatformArbitration(t *testing.T) {
	standardBuilder := demand("a/application", 1, 2*time.Minute, "builder")
	controlPlaneGate := demand("z/control-plane", 2, time.Minute, "small")
	in := input([]domain.Demand{standardBuilder, controlPlaneGate}, nil, State{})
	in.Config.LinuxCapacity = domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}
	setRepoSchedulingClass(t, &in.Config, controlPlaneGate.Key.Repo, controlPlaneClass)

	plan := PlanTick(in)
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{controlPlaneGate.Key}) {
		t.Fatalf("control-plane arbitration = %#v, want %#v", got, controlPlaneGate.Key)
	}
}

func TestControlPlaneLaneLeavesFeasibleFirstWaveCapacityForStandardWork(t *testing.T) {
	controlOne := demand("z/control-plane", 1, time.Minute, "small")
	controlTwo := demand("z/control-plane", 2, 50*time.Second, "small")
	standard := demand("a/application", 3, 2*time.Minute, "small")
	in := input([]domain.Demand{standard, controlTwo, controlOne}, nil, State{})
	in.Config.LinuxCapacity = domain.Resources{CPU: 3, MemoryMB: 6_144, Slots: 3}
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
	setRepoSchedulingClass(t, &in.Config, controlOne.Key.Repo, controlPlaneClass)

	want := []domain.DemandKey{controlOne.Key, controlTwo.Key, standard.Key}
	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, want) {
		t.Fatalf("first wave = %#v, want %#v", got, want)
	}
}

func TestAgedStandardDemandOverridesYoungControlPlane(t *testing.T) {
	agedStandard := demand("a/application", 1, 10*time.Minute, "small")
	youngControl := demand("z/control-plane", 2, time.Minute, "small")
	in := input([]domain.Demand{youngControl, agedStandard}, nil, State{})
	in.Config.LinuxCapacity = domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
	setRepoSchedulingClass(t, &in.Config, youngControl.Key.Repo, controlPlaneClass)

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{agedStandard.Key}) {
		t.Fatalf("aged override = %#v, want %#v", got, agedStandard.Key)
	}
}

func TestCursorRemainsFairWithinControlPlaneLane(t *testing.T) {
	a := demand("a/control", 1, time.Minute, "small")
	b := demand("b/control", 2, time.Minute, "small")
	in := input([]domain.Demand{a, b}, nil, State{DRRCursor: a.Key.Repo})
	in.Config.LinuxCapacity = domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
	setRepoSchedulingClass(t, &in.Config, a.Key.Repo, controlPlaneClass)
	setRepoSchedulingClass(t, &in.Config, b.Key.Repo, controlPlaneClass)

	if got := spawnedKeys(PlanTick(in)); !reflect.DeepEqual(got, []domain.DemandKey{b.Key}) {
		t.Fatalf("control-plane cursor order = %#v, want %#v", got, b.Key)
	}
}

func TestControlPlaneMacProfileSelectionIsDeterministic(t *testing.T) {
	standardMaestro := demand("a/application", 1, 2*time.Minute, "maestro")
	controlBuilder := demand("z/control-plane", 2, time.Minute, "builder")
	setUp := func(demands []domain.Demand) Input {
		in := input(demands, nil, State{})
		setRepoSchedulingClass(t, &in.Config, controlBuilder.Key.Repo, controlPlaneClass)
		return in
	}

	first := PlanTick(setUp([]domain.Demand{standardMaestro, controlBuilder}))
	second := PlanTick(setUp([]domain.Demand{controlBuilder, standardMaestro}))
	if got := spawnedKeys(first); !reflect.DeepEqual(got, []domain.DemandKey{controlBuilder.Key}) {
		t.Fatalf("mac profile priority = %#v, want %#v", got, controlBuilder.Key)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("priority plan depends on observation order: %#v / %#v", first, second)
	}
}
