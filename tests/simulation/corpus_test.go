package simulation_test

import (
	"flag"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// The corpus is the whole simulated history a seed sweep produces: every tick,
// every plan, every operation, every finding. ADR 0031 makes that history a pure
// function of (seed, world), so the corpus is a fingerprint of the fleet's
// behaviour rather than of the run that observed it.
//
// This file turns that fingerprint into a number an operator can compare. A
// refactor that claims zero behaviour change — the executor port extraction of
// issue #137, the Linux host support of the multi-node plan's chunk 2c, the
// container adapter of chunk 2d — proves the claim by producing an identical
// corpus before and after:
//
//	go test ./tests/simulation -run TestSimCorpus -corpus-seeds=64 -corpus-ticks=200 -v
//
// The digest folds every plan identity and application outcome in tick order,
// so it changes if the scheduler admits one different demand on one tick of one
// seed. The counts beside it say WHERE a divergence is, which a digest alone
// cannot.
var (
	corpusSeedsFlag = flag.Int("corpus-seeds", 4, "number of seeds each corpus arm explores")
	corpusTicksFlag = flag.Int("corpus-ticks", 60, "reconciliation ticks per corpus run")
	corpusRunsFlag  = flag.Int("corpus-runs", 3, "how many times each arm is swept")
)

// corpusCounts is one arm's whole sweep, reduced to comparable facts.
type corpusCounts struct {
	Arm       string
	Seeds     int
	Ticks     int
	Plans     int
	Applied   int
	Spawns    int
	Drains    int
	Instances int
	Findings  int
	Digest    string
}

func (c corpusCounts) String() string {
	return fmt.Sprintf("%-24s seeds=%d ticks=%d plans=%d applied=%d spawns=%d drains=%d instances=%d findings=%d digest=%s",
		c.Arm, c.Seeds, c.Ticks, c.Plans, c.Applied, c.Spawns, c.Drains, c.Instances, c.Findings, c.Digest)
}

// corpusSweep replays one arm end to end and reduces it. It deliberately uses
// the same generator, world, and oracles the fuzz sweep uses, so the corpus is
// the sweep rather than a parallel model of it.
func corpusSweep(t *testing.T, cfg worldConfig, seeds, ticks int) corpusCounts {
	t.Helper()
	counts := corpusCounts{Arm: cfg.Name, Seeds: seeds, Ticks: ticks}
	digest := fnv.New64a()
	instances := map[string]struct{}{}
	for offset := range seeds {
		seed := int64(1 + offset)
		trace := generateTrace(seed, ticks, cfg)
		world := newWorld(t, cfg, trace)
		findings := world.run()
		counts.Findings += len(findings)
		for _, observation := range world.observations {
			counts.Plans++
			if observation.Applied {
				counts.Applied++
			}
			for _, operation := range observation.Plan.Operations {
				switch operation.Kind {
				case scheduler.OperationSpawn:
					counts.Spawns++
					instances[fmt.Sprintf("%d/%s", seed, operation.Demand)] = struct{}{}
				case scheduler.OperationDrain:
					counts.Drains++
				}
			}
			fmt.Fprintf(digest, "%d|%d|%s|%t|%d|%d\n",
				seed, observation.Tick, observation.Plan.ID, observation.Applied, len(observation.Demands), observation.Queued)
		}
		for _, item := range sortedFindings(world.known) {
			fmt.Fprintf(digest, "known|%d|%s\n", seed, item)
		}
		world.close()
	}
	counts.Instances = len(instances)
	counts.Digest = fmt.Sprintf("%016x", digest.Sum64())
	return counts
}

// sortedFindings orders the documented defects a run met, because they are
// collected in a map and a corpus digest may not depend on map iteration order.
func sortedFindings(known map[string]finding) []string {
	items := make([]string, 0, len(known))
	for key, item := range known {
		items = append(items, key+"="+string(item.Kind))
	}
	sort.Strings(items)
	return items
}

// TestSimCorpusIsIdenticalAcrossRuns is the instrument's own contract and the
// no-op refactor gate in one: every arm is swept -corpus-runs times, and all of
// them must reduce to the same counts and the same digest. A harness that
// depended on wall-clock time, map ordering, or goroutine scheduling would fail
// here, and a refactor that changed one admission would fail against the numbers
// this test prints.
func TestSimCorpusIsIdenticalAcrossRuns(t *testing.T) {
	t.Parallel()
	for _, cfg := range []worldConfig{defaultWorld(), budgetedWorld()} {
		t.Run(cfg.Name, func(t *testing.T) {
			t.Parallel()
			var first corpusCounts
			var lines []string
			for run := range max(1, *corpusRunsFlag) {
				counts := corpusSweep(t, cfg, *corpusSeedsFlag, *corpusTicksFlag)
				lines = append(lines, fmt.Sprintf("run %d: %s", run+1, counts))
				if run == 0 {
					first = counts
					continue
				}
				if counts != first {
					t.Fatalf("corpus diverged between runs:\n%s", strings.Join(lines, "\n"))
				}
			}
			t.Logf("corpus:\n%s", strings.Join(lines, "\n"))
		})
	}
}
