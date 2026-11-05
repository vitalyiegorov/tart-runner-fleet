"""The npm cache's enforcement: evict whole packages, keep a reserve, stop when it cannot."""
import os
import pathlib
import stat
import subprocess
import tempfile
import time
import unittest

SCRIPT = pathlib.Path(__file__).resolve().parent.parent / "deploy" / "npm-cache" / "prune-npm-cache.py"
KIB = 1024
GIB = 1024**3


class PruneTest(unittest.TestCase):
    def setUp(self):
        scratch = tempfile.TemporaryDirectory()
        self.addCleanup(scratch.cleanup)
        self.root = pathlib.Path(scratch.name)
        self.data = self.root / "data"
        self.data.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.calls = self.root / "systemctl.calls"
        stand_in = self.bin / "systemctl"
        stand_in.write_text(f'#!/bin/sh\necho "$@" >> {self.calls}\n')
        stand_in.chmod(stand_in.stat().st_mode | stat.S_IEXEC)

    def package(self, name, age_days, kib=64):
        """A package directory as Verdaccio leaves it: metadata beside tarballs."""
        directory = self.data / name
        directory.mkdir(parents=True)
        when = time.time() - age_days * 86400
        for file, size in (("package.json", kib), (f"{directory.name}-1.0.0.tgz", kib)):
            path = directory / file
            path.write_bytes(os.urandom(size * KIB))
            os.utime(path, (when, when))
        return directory

    def run_prune(self, high_kib=1024, low_kib=512, reserve_gib=0.0):
        env = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}"}
        return subprocess.run(
            ["python3", "-I", str(SCRIPT), "--data", str(self.data), "--service", "fleet-test.service",
             "--high-gib", str(high_kib / (1024 * 1024)), "--low-gib", str(low_kib / (1024 * 1024)),
             "--reserve-gib", str(reserve_gib), "--grace-minutes", "15"],
            env=env, capture_output=True, text=True, check=False)

    def stopped(self):
        return self.calls.exists() and "stop fleet-test.service" in self.calls.read_text()

    def test_under_the_limits_nothing_is_touched(self):
        kept = self.package("kept", age_days=90)
        result = self.run_prune()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(kept.exists())
        self.assertFalse(self.stopped())

    def test_over_the_limit_evicts_oldest_packages_metadata_and_tarballs_together(self):
        oldest = self.package("oldest", age_days=90, kib=256)
        scoped = self.package("@scope/old", age_days=60, kib=256)
        recent = self.package("recent", age_days=2, kib=256)
        result = self.run_prune(high_kib=1024, low_kib=600)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(oldest.exists(), "the oldest package kept its metadata or tarball")
        self.assertFalse(scoped.exists(), "a scoped package was not eligible")
        self.assertTrue(recent.exists(), "evicted past the low-water mark")
        self.assertFalse(self.stopped())

    def test_a_read_keeps_a_package_alive(self):
        read_lately = self.package("read-lately", age_days=90, kib=256)
        tarball = read_lately / "read-lately-1.0.0.tgz"
        recent_read = time.time() - 86400
        os.utime(tarball, (recent_read, os.stat(tarball).st_mtime))
        older = self.package("older", age_days=30, kib=256)
        self.package("oldish", age_days=20, kib=256)
        self.run_prune(high_kib=1024, low_kib=900)
        self.assertFalse(older.exists())
        self.assertTrue(read_lately.exists(), "last use must be the newest access, not the file's age")

    def test_a_breached_reserve_evicts_even_under_the_size_limit_and_then_stops(self):
        old = self.package("old", age_days=90)
        result = self.run_prune(high_kib=1024 * 1024, low_kib=512 * 1024, reserve_gib=1e9)
        self.assertEqual(result.returncode, 1)
        self.assertFalse(old.exists(), "the cache kept data while the disk was under its reserve")
        self.assertTrue(self.stopped(), "it must stop the cache rather than let it refill a full disk")
        self.assertIn("reserve", result.stderr)

    def test_what_was_touched_inside_the_grace_period_survives_and_a_stuck_limit_stops(self):
        in_flight = self.package("in-flight", age_days=0, kib=1024)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(in_flight.exists(), "evicted a package that may be mid-download")
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.stopped(), "over the limit with nothing evictable must stop, not grow")

    def test_a_missing_data_directory_is_not_a_fault(self):
        self.data.rmdir()
        result = self.run_prune()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.stopped())

    def test_a_symlink_is_never_followed_out_of_the_cache(self):
        outside = self.root / "outside"
        outside.mkdir()
        precious = outside / "precious"
        precious.write_bytes(b"x" * KIB)
        self.package("old", age_days=90, kib=1024)
        (self.data / "link").symlink_to(outside, target_is_directory=True)
        scope = self.data / "@linked"
        scope.mkdir()
        (scope / "inner").symlink_to(outside, target_is_directory=True)
        self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(precious.exists())

    def test_a_data_path_that_is_a_symlink_is_refused(self):
        real = self.root / "real"
        self.data.rmdir()
        real.mkdir()
        self.data.symlink_to(real, target_is_directory=True)
        kept = real / "kept"
        kept.mkdir()
        (kept / "package.json").write_bytes(b"x" * (2048 * KIB))
        os.utime(kept / "package.json", (1, 1))
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertEqual(result.returncode, 2)
        self.assertTrue(kept.exists())

    def test_verdaccios_own_files_beside_the_packages_stay(self):
        database = self.data / ".verdaccio-db.json"
        database.write_text("{}")
        os.utime(database, (1, 1))
        self.package("old", age_days=90, kib=1024)
        self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(database.exists())
        self.assertFalse((self.data / "old").exists())

    def test_it_says_what_it_did(self):
        self.package("old", age_days=90, kib=1024)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertIn("evicted=1", result.stdout)
        self.assertEqual(result.returncode, 0, result.stderr)


class DeployTest(unittest.TestCase):
    """The units agree with each other: a prune of the wrong path enforces nothing."""

    deploy = SCRIPT.parent

    def test_the_timer_prunes_the_storage_the_quadlet_mounts(self):
        service = (self.deploy / "tart-runner-fleet-npm-cache-prune.service").read_text()
        quadlet = (self.deploy / "tart-runner-fleet-npm-cache.container").read_text()
        config = (self.deploy / "verdaccio.yaml").read_text()
        self.assertIn("Volume=%h/.local/share/tart-runner-fleet-npm-cache:/verdaccio/storage", quadlet)
        self.assertIn("storage: /verdaccio/storage/data", config)
        self.assertIn("--data %h/.local/share/tart-runner-fleet-npm-cache/data ", service)

    def test_the_limits_keep_their_order(self):
        service = (self.deploy / "tart-runner-fleet-npm-cache-prune.service").read_text()
        limit = lambda flag: float(service.split(f"--{flag} ")[1].split()[0])
        self.assertLess(limit("low-gib"), limit("high-gib"))
        self.assertGreater(limit("reserve-gib"), 60, "the reserve must sit above minFreeDiskGb")


if __name__ == "__main__":
    unittest.main()
