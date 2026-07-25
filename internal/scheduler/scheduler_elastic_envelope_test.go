package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The second-pilot scenarios all run on the production host shape: a Mac16,10
// with 10 physical cores and 24 GiB, an 8-vCPU/16-GiB configured Linux cap, and
// one live 7-vCPU macOS builder. Under the static envelope that residual is
// 1 vCPU, so nothing but a `small` can ever be admitted no matter how idle the
// machine is. Under the elastic envelope the bound becomes the real host.

const (
	hostPhysicalCPU      = 10
	hostPhysicalMemoryMB = 24_576
)

// elasticHost builds the host observation for a given measured idle percentage.
// Capacity is the physical machine; Available is the measured residual, with CPU
// derived as floor(cores x idle%) and memory as the pressure-derived figure.
func elasticHost(idlePercent float64, availableMemoryMB int) domain.Observation[domain.Host] {
	idleCores := int(float64(hostPhysicalCPU) * idlePercent / 100)
	return domain.Fresh(domain.Host{
		Capacity:  domain.Resources{CPU: hostPhysicalCPU, MemoryMB: hostPhysicalMemoryMB - 1_024, Slots: 4},
		Available: domain.Resources{CPU: idleCores, MemoryMB: availableMemoryMB, Slots: 4},
		Pressure: domain.HostPressure{AvailableMemoryMB: int64(availableMemoryMB), FreeDiskGB: 152,
			CPUIdlePercent: idlePercent, LoadAverage: 3.37, AdmissionAllowed: true, AdmissionReason: "capacity available"},
	}, testNow)
}

// elasticInput queues Linux mediums behind the same aged macOS builder head and
// aged larges seen in the incident, so admission has to come from the residual.
func elasticInput(idlePercent float64, availableMemoryMB int, elastic bool) Input {
	in := incidentInput(
		demand("b/repo", 7, 11*time.Minute, "medium"),
		demand("c/repo", 8, 11*time.Minute, "medium"),
	)
	in.Config.ElasticHostEnvelope = elastic
	in.Host = elasticHost(idlePercent, availableMemoryMB)
	return in
}

// admittedVector totals the resource vectors a plan spawns.
func admittedVector(in Input, plan Plan) domain.Resources {
	total := domain.Resources{}
	for _, op := range plan.Operations {
		if op.Kind != OperationSpawn {
			continue
		}
		r := in.Config.Profiles[op.Profile].Resources
		total = domain.Resources{CPU: total.CPU + r.CPU, MemoryMB: total.MemoryMB + r.MemoryMB, Slots: total.Slots + r.Slots}
	}
	return total
}

func liveVector(instances []domain.Instance) domain.Resources {
	total := domain.Resources{}
	for _, instance := range instances {
		if !instance.ConsumesHostResources() {
			continue
		}
		total = domain.Resources{CPU: total.CPU + instance.Resources.CPU,
			MemoryMB: total.MemoryMB + instance.Resources.MemoryMB, Slots: total.Slots + instance.Resources.Slots}
	}
	return total
}

// TestElasticEnvelopeExpandsIntoIdleHost is the RED-first second-pilot case. The
// host is 62% idle with 12 GiB available and only a 7-vCPU builder live, so
// three cores and gigabytes of memory are genuinely free. A queued medium
// (2 vCPU / 4096 MiB) fits the real machine. The static envelope refuses it
// because 8-7=1 configured vCPU remains; the elastic envelope must admit it.
func TestElasticEnvelopeExpandsIntoIdleHost(t *testing.T) {
	staticPlan := PlanTick(elasticInput(62.4, 12_288, false))
	if got := spawnedKeys(staticPlan); len(got) != 0 {
		t.Fatalf("static envelope must document today's behavior (nothing fits 1 residual vCPU); got %#v", got)
	}

	in := elasticInput(62.4, 12_288, true)
	plan := PlanTick(in)
	if plan.Status != PlanReady {
		t.Fatalf("plan status = %s, want ready", plan.Status)
	}
	admitted := admittedVector(in, plan)
	if admitted.CPU == 0 {
		t.Fatalf("elastic envelope admitted nothing on a 62%%-idle host: %#v", plan.Operations)
	}
	for _, op := range plan.Operations {
		if op.Kind == OperationDrain {
			t.Fatalf("second-pilot admission must never drain: %#v", plan.Operations)
		}
	}
	// Total vCPU must stay within the physical machine.
	total := liveVector(in.Instances.Value).CPU + admitted.CPU
	if total > hostPhysicalCPU {
		t.Fatalf("admitted %d vCPU on top of %d live, exceeding %d physical cores",
			admitted.CPU, liveVector(in.Instances.Value).CPU, hostPhysicalCPU)
	}
}

// TestElasticEnvelopeYieldsToBusyHost is the other half of second pilot: when
// the host's own tenant is consuming the machine the fleet must wait for its
// slice rather than compete for it. At 8% idle no measured core is free, so
// nothing may be admitted even though the configured envelope and the memory
// residual would both still allow it.
func TestElasticEnvelopeYieldsToBusyHost(t *testing.T) {
	in := elasticInput(8, 12_288, true)
	plan := PlanTick(in)
	if got := spawnedKeys(plan); len(got) != 0 {
		t.Fatalf("spawns = %#v, want none: the fleet must yield while the host is busy", got)
	}
}

// TestElasticEnvelopeScalesWithMeasuredIdle proves admission tracks the measured
// host share monotonically rather than flipping between all and nothing.
func TestElasticEnvelopeScalesWithMeasuredIdle(t *testing.T) {
	previous := -1
	for _, idle := range []float64{8, 20, 40, 62.4, 90} {
		in := elasticInput(idle, 12_288, true)
		admitted := admittedVector(in, PlanTick(in)).CPU
		if admitted < previous {
			t.Fatalf("idle %.1f%% admitted %d vCPU, less than %d at a busier host: admission must not shrink as the host frees up",
				idle, admitted, previous)
		}
		if live := liveVector(in.Instances.Value).CPU; live+admitted > hostPhysicalCPU {
			t.Fatalf("idle %.1f%%: %d live + %d admitted vCPU exceeds %d physical cores", idle, live, admitted, hostPhysicalCPU)
		}
		previous = admitted
	}
	if previous <= 0 {
		t.Fatalf("a 90%%-idle host admitted nothing; the elastic envelope never expanded")
	}
}

// TestElasticEnvelopeNeverExceedsPhysicalMemory proves the physical Capacity
// bound applies to memory too, so a boot burst cannot over-commit RAM before the
// new VMs' usage shows up in the measured residual.
func TestElasticEnvelopeNeverExceedsPhysicalMemory(t *testing.T) {
	// A wide-open memory residual: only the physical Capacity bound can hold the
	// line once the live builder's 12 GiB reservation is accounted for.
	in := elasticInput(90, hostPhysicalMemoryMB, true)
	plan := PlanTick(in)
	live := liveVector(in.Instances.Value)
	admitted := admittedVector(in, plan)
	if live.MemoryMB+admitted.MemoryMB > in.Host.Value.Capacity.MemoryMB {
		t.Fatalf("%d live + %d admitted MiB exceeds the %d MiB physical capacity bound",
			live.MemoryMB, admitted.MemoryMB, in.Host.Value.Capacity.MemoryMB)
	}
}

// TestElasticEnvelopeRespectsLinuxCap proves LinuxCapacity still caps Linux in
// elastic mode: it becomes a per-platform limit, not a discarded one.
func TestElasticEnvelopeRespectsLinuxCap(t *testing.T) {
	in := elasticInput(90, hostPhysicalMemoryMB, true)
	// No macOS instance at all, so only the Linux cap and the host bound apply.
	in.Instances = domain.Fresh([]domain.Instance(nil), testNow)
	in.Config.LinuxCapacity = domain.Resources{CPU: 3, MemoryMB: 16_384, Slots: 4}
	admitted := admittedVector(in, PlanTick(in))
	if admitted.CPU > 3 {
		t.Fatalf("admitted %d vCPU, exceeding the 3-vCPU Linux cap", admitted.CPU)
	}
}

// TestElasticEnvelopeChargesOnlyConsumingLinux proves the two accounting rules
// that decide what the Linux cap has left: a live Linux VM is charged against it,
// and a VM that has released its host resources (drained and powered off) is not.
// Getting the second wrong would strand capacity that no longer exists.
func TestElasticEnvelopeChargesOnlyConsumingLinux(t *testing.T) {
	consuming := liveLinux("trf-linux-live", "a/repo", "medium")
	released := liveLinux("trf-linux-drained", "b/repo", "large")
	released.State = domain.InstanceDraining
	released.Power = domain.InstancePowerStopped
	if released.ConsumesHostResources() {
		t.Fatal("precondition: a drained, powered-off instance must not consume host resources")
	}

	in := elasticInput(90, hostPhysicalMemoryMB, true)
	in.Instances = domain.Fresh([]domain.Instance{consuming, released}, testNow)

	free := linuxFree(in)
	// The 8-vCPU Linux cap is charged for the live medium only: 8-2 = 6.
	if free.CPU != 6 {
		t.Fatalf("free CPU = %d, want 6: only the consuming medium may be charged against the 8-vCPU Linux cap", free.CPU)
	}
	// The physical bound is likewise charged for the live medium only: 10-2 = 8.
	if free.CPU > hostPhysicalCPU-consuming.Resources.CPU {
		t.Fatalf("free CPU = %d exceeds the physical remainder", free.CPU)
	}
}

// TestElasticEnvelopeClosesOnOversubscribedLinuxCap proves the fail-closed leaf:
// if live Linux instances already exceed the Linux cap (a transient a config
// change can produce) the envelope is empty rather than negative, and admission
// stays shut.
func TestElasticEnvelopeClosesOnOversubscribedLinuxCap(t *testing.T) {
	in := elasticInput(90, hostPhysicalMemoryMB, true)
	in.Config.LinuxCapacity = domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}
	in.Instances = domain.Fresh([]domain.Instance{
		liveLinux("trf-linux-1", "a/repo", "large"),
	}, testNow)

	if free := linuxFree(in); free != (domain.Resources{}) {
		t.Fatalf("free = %+v, want empty when live Linux exceeds the Linux cap", free)
	}
	if got := spawnedKeys(PlanTick(in)); len(got) != 0 {
		t.Fatalf("spawns = %#v, want none on an over-subscribed Linux cap", got)
	}
}

// TestElasticEnvelopeIgnoresUnobservedCapacity proves an unobserved physical
// total imposes no bound and never masquerades as a measurement: with Capacity
// zero the planner must fall back to the configured envelope rather than treat
// the host as having no resources at all.
func TestElasticEnvelopeIgnoresUnobservedCapacity(t *testing.T) {
	in := elasticInput(90, 12_288, true)
	in.Instances = domain.Fresh([]domain.Instance(nil), testNow)
	in.Host = domain.Fresh(domain.Host{
		Available: domain.Resources{CPU: 8, MemoryMB: 12_288, Slots: 4},
	}, testNow)
	if admitted := admittedVector(in, PlanTick(in)); admitted.CPU == 0 {
		t.Fatal("unobserved physical capacity must impose no bound, not block all admission")
	}
}
