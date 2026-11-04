"""The runner image carries the Android SDK packages real builds installed."""
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "deploy/actions-cache/android-sdk-packages.txt"
SCRIPT = ROOT / "deploy/actions-cache/check-android-sdk.sh"
LAYER = ROOT / "deploy/actions-cache/Containerfile.local"
SMOKE = ROOT / "scripts/check-runner-image.sh"

# The output of sdkmanager 23.0 --list_installed in the image, recorded under
# --network=none: plain columns, and ids spelled with a slash.
SDKMANAGER = """#!/bin/sh
printf '%s\\n' "$*" >> "$SDK/calls"
echo 'Error: play.google.com' >&2
echo 'Installed packages:'
tr ';' / < "$SDK/installed" | sed 's/$/   1.0.0   Some description   somewhere/; s/^/  /'
"""


def packages():
    rows = [line.split("#")[0].split() for line in MANIFEST.read_text().splitlines()]
    return [row[0] for row in rows if row]


class CheckTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.sdk = Path(tmp.name)
        tool = self.sdk / "cmdline-tools/latest/bin/sdkmanager"
        tool.parent.mkdir(parents=True)
        tool.write_text(SDKMANAGER)
        tool.chmod(tool.stat().st_mode | stat.S_IXUSR)
        (self.sdk / "licenses").mkdir()
        (self.sdk / "licenses/android-sdk-license").write_text("hash\n")

    def check(self, installed="ndk;27.1.12297006\ncmake;3.22.1\n",
              manifest="ndk;27.1.12297006\ncmake;3.22.1 # why\n\n"):
        (self.sdk / "installed").write_text(installed)
        return subprocess.run(["sh", str(SCRIPT)], input=manifest, text=True, capture_output=True, check=False,
                              env=dict(os.environ, ANDROID_SDK_ROOT=str(self.sdk), SDK=str(self.sdk)))

    def calls(self):
        calls = self.sdk / "calls"
        return calls.read_text() if calls.exists() else ""

    def test_passes_on_a_complete_sdk_without_installing(self):
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls(), f"--sdk_root={self.sdk} --list_installed\n")

    def test_names_a_missing_package(self):
        result = self.check(installed="cmake;3.22.1\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ndk;27.1.12297006", result.stderr)

    def test_matches_a_version_exactly(self):
        self.assertNotEqual(self.check(installed="ndk;27.1.12297006-rc1\ncmake;3.22.1\n").returncode, 0)

    def test_fails_without_an_accepted_licence(self):
        (self.sdk / "licenses/android-sdk-license").unlink()
        self.assertNotEqual(self.check().returncode, 0)

    def test_a_malformed_package_reaches_no_sdkmanager(self):
        for manifest in ("", "ndk;27 extra\n", "ndk;$(id)\n", "--licenses\n"):
            with self.subTest(manifest):
                self.assertNotEqual(self.check(manifest=manifest).returncode, 0)
                self.assertEqual(self.calls(), "")


class ManifestTest(unittest.TestCase):
    def test_the_packages_builds_installed_are_listed_exactly(self):
        # #414: 66 of 168 Android builds installed all of these.
        self.assertEqual(sorted(packages()), [
            "build-tools;36.0.0", "build-tools;37.0.0", "cmake;3.22.1", "cmdline-tools;16.0",
            "ndk;27.1.12297006", "platforms;android-37.0",
        ])

    def test_the_layer_copies_the_sdk_as_its_owner_and_ships_the_check(self):
        layer = LAYER.read_text()
        self.assertIn("COPY --chown=runner:runner android-sdk/ /opt/android-sdk/", layer)
        self.assertRegex(layer, r"COPY android-sdk-packages\.txt check-android-sdk\.sh /usr/local/libexec/trf-android-sdk/")

    def test_the_offline_smoke_checks_the_sdk(self):
        self.assertRegex(SMOKE.read_text(), r"check-android-sdk\.sh\s*<\s*\S*android-sdk-packages\.txt")


if __name__ == "__main__":
    unittest.main()
