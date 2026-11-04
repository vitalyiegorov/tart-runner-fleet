"""The runner image's local layer is built from files already on the node, so it cannot reach the WAN."""
from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[1]
LAYER = ROOT / "deploy/actions-cache/Containerfile.local"
RUNNER = ROOT / "deploy/actions-cache/Containerfile.runner"
SMOKE = ROOT / "scripts/check-runner-image.sh"


def instructions(path):
    text = re.sub(r"\\\n", " ", path.read_text())
    return [line.split()[0] for line in text.splitlines() if line.strip() and not line.startswith("#")]


class LocalLayerTest(unittest.TestCase):
    def test_nothing_in_the_layer_can_download(self):
        # A RUN could reach apt or curl, and ADD can fetch a URL. COPY cannot.
        self.assertEqual(sorted(set(instructions(LAYER))), ["ARG", "COPY", "FROM"])

    def test_copied_tools_stay_writable_by_the_runner_at_the_cache_path(self):
        self.assertIn("COPY --chown=runner:runner tool/ /home/runner/actions-runner/_work/_tool/", LAYER.read_text())

    def test_the_requested_codeql_bundle_is_the_recipe_default(self):
        # budgie job 126702403160: "Did not find CodeQL tools version 2.27.4 in the toolcache."
        self.assertRegex(RUNNER.read_text(), r'ARG CODEQL_VERSIONS="[^"]*\b2\.27\.4\b')

    def test_the_offline_smoke_requires_the_version_the_recipe_requests(self):
        # A glob over what is baked would pass on an image holding only an old bundle.
        smoke = SMOKE.read_text()
        self.assertIn("ARG CODEQL_VERSIONS=", smoke)
        self.assertIn("for version in $CODEQL_VERSIONS", smoke)
        self.assertNotIn("CodeQL/*", smoke)


if __name__ == "__main__":
    unittest.main()
