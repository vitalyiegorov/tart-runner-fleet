package simulation_test

import (
	"flag"
	"fmt"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

var costSeeds = flag.Int("cost-seeds", 64, "seeds per arm for the cost measurement")
var costTicks = flag.Int("cost-ticks", 200, "ticks per cost run")

// TestCostOfHoldingTheVector measures what the release rule costs across the
// corpus: how long instances spend tearing down while still charging the host,
// and whether the queue behind them still drains.
func TestCostOfHoldingTheVector(t *testing.T) {
	for _, cfg := range []worldConfig{defaultWorld(), budgetedWorld(), containerNodeWorld(),
		federatedWorld(), sequenceResetWorld(), tieredWorld()} {
		var jobs, done, teardownTicks, releasedInstances, totalWait, waited int
		maxHold := 0
		firstTearing := map[string]int{}
		for offset := range *costSeeds {
			seed := int64(1 + offset)
			trace := generateTrace(seed, *costTicks, cfg)
			w := newWorld(t, cfg, trace)
			w.run()
			clear(firstTearing)
			for _, o := range w.observations {
				if !o.InstancesUsable {
					continue
				}
				live := map[string]bool{}
				for _, i := range o.Instances {
					if !i.State.TearingDown() {
						continue
					}
					live[i.ID] = true
					if _, seen := firstTearing[i.ID]; !seen {
						firstTearing[i.ID] = o.Tick
					}
					if i.ConsumesHostResources() {
						teardownTicks++
						if hold := o.Tick - firstTearing[i.ID] + 1; hold > maxHold {
							maxHold = hold
						}
					}
				}
				for id, start := range firstTearing {
					if !live[id] {
						releasedInstances++
						delete(firstTearing, id)
						_ = start
					}
				}
			}
			for _, job := range w.jobs {
				jobs++
				if job.status == jobDone {
					done++
				}
				if !job.startedAt.IsZero() {
					waited++
					totalWait += int(job.startedAt.Sub(job.queuedAt) / simTick)
				}
			}
			w.close()
		}
		wait := 0.0
		if waited > 0 {
			wait = float64(totalWait) / float64(waited)
		}
		fmt.Printf("%-26s jobs=%d done=%d teardown_charged_ticks=%d torn_down=%d max_hold=%d mean_wait_ticks=%.2f\n",
			cfg.Name, jobs, done, teardownTicks, releasedInstances, maxHold, wait)
	}
	_ = domain.InstanceStopping
}
