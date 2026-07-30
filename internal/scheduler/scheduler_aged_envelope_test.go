package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The elastic envelope clamps admission by floor(cores x idle%), measured at the
// instant of the tick. That makes instantaneous CPU idle a HARD gate: a demand
// needing R cores requires idle >= R/cores at some tick, so a 6-vCPU builder
// needs a 60%-idle moment that a host with an active interactive tenant may
// never produce. Observed in production on 2026-07-30: builder jobs starved for
// 23 hours across two scopes while smaller profiles kept flowing.
//
// That contradicts the fleet's own doctrine twice over. The host probe treats
// CPU idle, load, and swap as advisory throttles that must never fail-close
// admission; and ADR 0012 promises that aging promotes old work to global FIFO
// "so large jobs cannot starve" -- a guarantee the envelope silently voided,
// because FIFO position is worthless when the envelope never opens.
//
// The repair: an AGED demand escapes the advisory CPU-idle clamp. Every hard
// bound still applies -- the Linux-only cap, the physical total net of live
// reservations, the measured memory residual, the slot ceiling, repository caps,
// and profile MaxActive. Young work keeps the idle throttle: politeness for
// fresh work, a guaranteed floor for old work.

// agedBusyHostInput is the production shape: 10 physical cores, a host whose
// tenant keeps idle at 15% (1 measured free core), ample memory, and no live
// instances -- so the ONLY thing refusing a builder is the idle clamp.
func agedBusyHostInput(demandAge time.Duration) Input {
	in := input([]domain.Demand{demand("mac-a", 1, demandAge, "builder")},
		[]domain.Instance(nil), State{})
	in.Config = elasticConfig()
	in.Config.ElasticHostEnvelope = true
	in.Config.Profiles["builder"] = domain.Profile{ID: "builder", Platform: domain.PlatformMacOS,
		Route: "macos-builder", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, MaxActive: 1}
	in.Host = elasticHost(15, 20_480)
	return in
}

// TestAgedWorkEscapesTheAdvisoryIdleThrottle is the RED-first case: a builder
// past the fairness age must be admitted on a busy host when every hard bound
// has room, exactly as ADR 0012's starvation guard promises.
func TestAgedWorkEscapesTheAdvisoryIdleThrottle(t *testing.T) {
	in := agedBusyHostInput(23 * time.Hour)
	plan := PlanTick(in)
	spawns := spawnedKeys(plan)
	if len(spawns) != 1 {
		t.Fatalf("aged builder on a 15%%-idle host with 10 free physical cores was refused: %#v", plan.Operations)
	}
	for _, op := range plan.Operations {
		if op.Kind == OperationDrain {
			t.Fatalf("the starvation guard must not drain anything: %#v", plan.Operations)
		}
	}
}

// TestYoungWorkStillYieldsToTheBusyHost proves the throttle survives for fresh
// work: the identical demand below the fairness age keeps waiting for the
// host's tenant, which is the second-pilot politeness the throttle exists for.
func TestYoungWorkStillYieldsToTheBusyHost(t *testing.T) {
	in := agedBusyHostInput(time.Minute)
	if spawns := spawnedKeys(PlanTick(in)); len(spawns) != 0 {
		t.Fatalf("young builder must yield to a 15%%-idle host, got %#v", spawns)
	}
}

// TestAgedWorkStillBoundedByPhysicalCores proves the escape is from the
// advisory clamp only: an aged demand can never overcommit the machine.
func TestAgedWorkStillBoundedByPhysicalCores(t *testing.T) {
	in := agedBusyHostInput(23 * time.Hour)
	// A live 7-vCPU instance leaves 3 physical cores; the 6-vCPU builder must
	// keep waiting no matter how aged it is.
	in.Instances = domain.Fresh([]domain.Instance{elasticBuilder()}, testNow)
	if spawns := spawnedKeys(PlanTick(in)); len(spawns) != 0 {
		t.Fatalf("aged work overcommitted the physical cores: %#v", spawns)
	}
}

// TestAgedWorkStillBoundedByMemoryResidual proves memory stays hard. Memory is
// not time-shared the way vCPUs are: admitting past the measured residual risks
// real paging, so the pressure-derived clamp binds aged work too.
func TestAgedWorkStillBoundedByMemoryResidual(t *testing.T) {
	in := agedBusyHostInput(23 * time.Hour)
	// Plenty of idle CPU signal is irrelevant; only 4 GiB of memory remains and
	// the builder wants 12 GiB.
	in.Host = elasticHost(90, 4_096)
	if spawns := spawnedKeys(PlanTick(in)); len(spawns) != 0 {
		t.Fatalf("aged work was admitted past the memory residual: %#v", spawns)
	}
}

// TestAgedLinuxHeadUsesTheStarvationEnvelope proves the escape reaches the Linux
// aged-FIFO path too: an aged large that fits every hard bound spawns instead of
// reserving forever behind the idle clamp.
func TestAgedLinuxHeadUsesTheStarvationEnvelope(t *testing.T) {
	in := input([]domain.Demand{demand("a/repo", 1, time.Hour, "large")},
		[]domain.Instance(nil), State{})
	in.Config = elasticConfig()
	in.Config.ElasticHostEnvelope = true
	in.Host = elasticHost(15, 20_480) // 1 idle core; large needs 4
	plan := PlanTick(in)
	if spawns := spawnedKeys(plan); len(spawns) != 1 {
		t.Fatalf("aged large reserved instead of spawning: spawns=%#v reservation=%#v",
			spawns, plan.Next.Reservation)
	}
}

// TestStaticEnvelopeIgnoresAging proves the exemption is elastic-only: the
// static model never consulted idle, so aging must not change it byte-for-byte.
func TestStaticEnvelopeIgnoresAging(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, 23 * time.Hour} {
		in := agedBusyHostInput(age)
		in.Config.ElasticHostEnvelope = false
		// Static Host.Available echoes the configured envelope.
		in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
		if spawns := spawnedKeys(PlanTick(in)); len(spawns) != 1 {
			t.Fatalf("static envelope at age %v refused a fitting builder: %#v", age, spawns)
		}
	}
}
