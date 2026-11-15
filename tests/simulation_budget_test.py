"""Keep all simulation seeds and race packages inside explicit CI budgets."""
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[1]


class SimulationBudgetTest(unittest.TestCase):
    def test_race_simulation_has_its_own_required_job(self):
        workflow = (ROOT / '.github/workflows/ci.yml').read_text()
        self.assertIn('run: make race-simulation', workflow)
        self.assertIn('RACE_SIMULATION_RESULT: ${{ needs.race-simulation.result }}', workflow)
        self.assertIn('test "$RACE_SIMULATION_RESULT" = success', workflow)
        makefile = (ROOT / 'Makefile').read_text()
        self.assertIn('race: race-packages race-simulation', makefile)
        self.assertIn("grep -v '/tests/simulation$$'", makefile)

    def test_bounded_sweep_has_its_own_required_job(self):
        workflow = (ROOT / '.github/workflows/ci.yml').read_text()
        self.assertIn('  simulation:\n', workflow)
        self.assertIn('SIMULATION_RESULT: ${{ needs.simulation.result }}', workflow)
        self.assertIn('test "$SIMULATION_RESULT" = success', workflow)
        self.assertIn('-timeout 35m ./tests/simulation -run TestSimFuzz -seeds=80 -sim-ticks=200', workflow)

    def test_nightly_shards_preserve_all_500_seeds(self):
        workflow = (ROOT / '.github/workflows/nightly-simulation.yml').read_text()
        self.assertIn('shard: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]', workflow)
        self.assertIn('fail-fast: false', workflow)
        self.assertIn('-seed-base=$((1 + SHARD * 50)) -seeds=50 -sim-ticks=320', workflow)

    def test_publish_digest_is_strict(self):
        for workflow in (ROOT / '.github/workflows').glob('*.yml'):
            self.assertNotIn('digest-mismatch: warn', workflow.read_text(), str(workflow))
