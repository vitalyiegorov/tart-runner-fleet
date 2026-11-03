"""Both Linux runner images take their tools from one manifest, and only from it."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "deploy/actions-cache/runner-tools.txt"
SCRIPT = ROOT / "deploy/actions-cache/install-runner-tools.sh"
BUILDS = {
    "amd64 podman image": ROOT / "deploy/actions-cache/Containerfile.runner",
    "arm64 Tart base": ROOT / "docs/LINUX_BASE_IMAGE.md",
}


def manifest():
    rows = [line.split("#")[0].split() for line in MANIFEST.read_text().splitlines()]
    return [row for row in rows if row]


def apt_installs(text):
    """Every package named by an `apt-get install` in a build, continuations joined."""
    joined = re.sub(r"\\\n", " ", text)
    for match in re.finditer(r"apt-get install([^\n&;|']*)", joined):
        yield from (word for word in match.group(1).split() if not word.startswith("-"))


class OneManifestTest(unittest.TestCase):
    def test_every_build_installs_from_the_manifest(self):
        for name, build in BUILDS.items():
            with self.subTest(name):
                text = build.read_text()
                self.assertIn("runner-tools.txt", text)
                self.assertIn("install-runner-tools.sh", text)

    def test_no_build_installs_a_manifest_package_on_its_own(self):
        packages = {package for _, package in manifest()}
        for name, build in BUILDS.items():
            with self.subTest(name):
                self.assertEqual(sorted(packages & set(apt_installs(build.read_text()))), [])

    def test_the_tools_jobs_failed_without_are_listed(self):
        # unzip: rnw-community job 125785846761 (oven-sh/setup-bun) on arm64.
        # gh: #467's release publish on the arm64 linux-go guest.
        commands = {command for command, _ in manifest()}
        self.assertLessEqual({"unzip", "gh"}, commands)


class InstallerTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.runner_path = self.root / "runner-path"
        self.runner_path.mkdir()
        stubs = self.root / "stubs"
        stubs.mkdir()
        apt = stubs / "apt-get"
        apt.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CALLS"\n')
        apt.chmod(0o755)
        self.calls = self.root / "calls"
        self.env = dict(os.environ, PATH=f"{stubs}:{os.environ['PATH']}",
                        RUNNER_PATH=str(self.runner_path), CALLS=str(self.calls))

    def provide(self, *commands):
        for command in commands:
            tool = self.runner_path / command
            tool.write_text("#!/bin/sh\n")
            tool.chmod(0o755)

    def run_script(self, text, *args):
        return subprocess.run(["sh", str(SCRIPT), *args], input=text, env=self.env,
                              text=True, capture_output=True, check=False)

    def test_check_names_every_missing_command(self):
        self.provide("zip")
        result = self.run_script("unzip unzip\nzip zip\nxz xz-utils\n", "--check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("unzip(unzip) xz(xz-utils)", result.stderr)
        self.assertFalse(self.calls.exists())

    def test_check_passes_when_every_command_resolves(self):
        self.provide("unzip", "zip")
        result = self.run_script("# command package\nunzip unzip\n\nzip zip  # trailing\n", "--check")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_install_names_each_package_once_then_checks(self):
        self.provide("gcc", "make")
        result = self.run_script("gcc build-essential\nmake build-essential\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls.read_text().splitlines(),
                         ["update", "install -y --no-install-recommends build-essential"])

    def test_install_still_fails_when_a_package_lacks_its_command(self):
        result = self.run_script("unzip unzip\n")
        self.assertEqual(result.returncode, 1)
        self.assertIn("unzip(unzip)", result.stderr)

    def test_malformed_or_empty_manifest_is_refused_before_apt(self):
        for text in ["unzip\n", "unzip unzip extra\n", "$(id) unzip\n", "# only a comment\n", ""]:
            with self.subTest(text=text):
                self.assertNotEqual(self.run_script(text).returncode, 0)
                self.assertFalse(self.calls.exists())

    def test_the_shipped_manifest_parses(self):
        self.provide(*{command for command, _ in manifest()})
        result = self.run_script(MANIFEST.read_text(), "--check")
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
