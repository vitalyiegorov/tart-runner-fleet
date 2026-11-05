"""The npm cache's enforcement: stay inside storage, spare what is active, tell the truth."""
import importlib.util
import os
import pathlib
import re
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

DEPLOY = pathlib.Path(__file__).resolve().parent.parent / "deploy" / "npm-cache"
SCRIPT = DEPLOY / "prune-npm-cache.py"
KIB = 1024
SERVICE = "tart-runner-fleet-npm-cache.service"


def gib(kib):
    return kib / (1024 * 1024)


class Fixture(unittest.TestCase):
    def setUp(self):
        scratch = tempfile.TemporaryDirectory()
        self.addCleanup(scratch.cleanup)
        self.root = pathlib.Path(os.path.realpath(scratch.name))
        self.home = self.root / "home"
        self.storage = self.home / ".local/share/tart-runner-fleet-npm-cache"
        self.data = self.storage / "data"
        self.data.mkdir(parents=True)
        self.outside = self.root / "outside"
        self.outside.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.calls = self.root / "systemctl.calls"
        self.stand_in_systemctl()

    def stand_in_systemctl(self, stop_exit=0, still_active=False):
        """Records its calls. `is-active` answers as a stopped unit does, unless told otherwise."""
        active = "echo active; exit 0" if still_active else "echo inactive; exit 3"
        body = (f'#!/bin/sh\necho "$@" >> {self.calls}\n'
                f'case "$*" in *is-active*) {active} ;; *stop*) exit {stop_exit} ;; esac\n')
        stand_in = self.bin / "systemctl"
        stand_in.write_text(body)
        stand_in.chmod(stand_in.stat().st_mode | stat.S_IEXEC)

    def package(self, name, age_days, kib=64, root=None):
        """A package directory as Verdaccio leaves it: metadata beside tarballs, all aged."""
        directory = (root or self.data) / name
        directory.mkdir(parents=True)
        when = time.time() - age_days * 86400
        for file, size in (("package.json", kib), (f"{directory.name}-1.0.0.tgz", kib)):
            path = directory / file
            path.write_bytes(os.urandom(size * KIB))
            os.utime(path, (when, when))
        os.utime(directory, (when, when))
        return directory

    def arguments(self, high_kib=1024, low_kib=512, reserve_gib=0.0, data=None):
        return ["--data", str(self.data if data is None else data), "--high-gib", str(gib(high_kib)),
                "--low-gib", str(gib(low_kib)), "--reserve-gib", str(reserve_gib), "--grace-minutes", "15",
                "--settle-seconds", "0"]

    def run_prune(self, home=None, **limits):
        env = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", "HOME": str(self.home if home is None else home)}
        return subprocess.run(["python3", "-I", str(SCRIPT), *self.arguments(**limits)], env=env,
                              capture_output=True, text=True, check=False)

    def run_in_process(self, *, free=None, mounts=None, **limits):
        """The real script in this process, so a test can act at the moment it measures free space,
        which falls after it has listed packages and before it deletes any."""
        spec = importlib.util.spec_from_file_location("prune_npm_cache", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        patches = {"HOME": str(self.home), "PATH": f"{self.bin}:{os.environ['PATH']}"}
        argv = ["prune-npm-cache.py", *self.arguments(**limits)]
        with mock.patch.dict(os.environ, patches), mock.patch.object(sys, "argv", argv):
            if free is not None:
                module.free_bytes = free
            if mounts is not None:
                module.mount_points = mounts
            with self.assertRaises(SystemExit) as finished:
                module.main()
        return finished.exception.code

    def stopped(self):
        return self.calls.exists() and f"stop {SERVICE}" in self.calls.read_text()

    def verified_stop(self):
        return self.calls.exists() and re.search(rf"is-active( --quiet)? {SERVICE}", self.calls.read_text()) is not None


class Limits(Fixture):
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
        os.utime(tarball, (time.time() - 86400, os.stat(tarball).st_mtime))
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

    def test_a_missing_data_directory_is_not_a_fault(self):
        self.data.rmdir()
        result = self.run_prune()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.stopped())

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


class StaysInsideStorage(Fixture):
    """Finding 1: nothing outside the pinned storage directory may ever be deleted."""

    def precious(self, name="old"):
        directory = self.package(name, age_days=90, kib=1024, root=self.outside)
        return directory

    def assert_refused(self, result, kept):
        self.assertNotEqual(result.returncode, 0, "it ran on a path it should have refused")
        self.assertTrue(kept.exists(), "it deleted outside storage")
        self.assertTrue(self.stopped(), "a refusal is an enforcement failure: the cache must stop")

    def test_a_symlinked_ancestor_is_refused(self):
        # What the review reproduced: the final component is a real directory, an ancestor is not.
        real_storage = self.outside / "real-storage"
        real_storage.mkdir()
        kept = self.package("old", age_days=90, kib=1024, root=real_storage / "data")
        self.data.rmdir()
        self.storage.rmdir()
        self.storage.symlink_to(real_storage, target_is_directory=True)
        self.assert_refused(self.run_prune(high_kib=512, low_kib=256), kept)

    def test_a_data_path_that_is_a_symlink_is_refused(self):
        kept = self.precious()
        self.data.rmdir()
        self.data.symlink_to(self.outside, target_is_directory=True)
        self.assert_refused(self.run_prune(high_kib=512, low_kib=256), kept)

    def test_an_arbitrary_path_is_refused(self):
        kept = self.precious()
        self.assert_refused(self.run_prune(high_kib=512, low_kib=256, data=self.outside), kept)

    def test_a_relative_and_an_empty_path_are_refused(self):
        kept = self.package("kept", age_days=90, kib=1024)
        for path in ("", "data", str(self.data) + "/../data", str(self.data) + "/"):
            result = self.run_prune(high_kib=512, low_kib=256, data=path)
            self.assertNotEqual(result.returncode, 0, f"accepted {path!r}")
            self.assertTrue(kept.exists(), f"deleted through {path!r}")

    def test_an_unusable_home_is_refused(self):
        kept = self.package("kept", age_days=90, kib=1024)
        for home in ("", "relative/home", str(self.outside)):
            result = self.run_prune(high_kib=512, low_kib=256, home=home)
            self.assertNotEqual(result.returncode, 0, f"accepted HOME={home!r}")
            self.assertTrue(kept.exists(), f"deleted under HOME={home!r}")

    def test_a_scope_swapped_for_a_symlink_mid_run_redirects_nothing(self):
        # What the review reproduced: enumerate `@scope`, then replace it before the delete.
        self.package("@scope/old", age_days=90, kib=1024)
        kept = self.package("old", age_days=90, kib=1024, root=self.outside)

        def swap(*_):
            import shutil
            shutil.rmtree(self.data / "@scope")
            (self.data / "@scope").symlink_to(self.outside, target_is_directory=True)
            return 10**15

        self.run_in_process(free=swap, high_kib=512, low_kib=256)
        self.assertTrue(kept.exists(), "the swap redirected the delete outside storage")

    def test_a_symlink_is_never_followed_out_of_the_cache(self):
        kept = self.outside / "precious"
        kept.write_bytes(b"x" * KIB)
        self.package("old", age_days=90, kib=1024)
        (self.data / "link").symlink_to(self.outside, target_is_directory=True)
        scope = self.data / "@linked"
        scope.mkdir()
        (scope / "inner").symlink_to(self.outside, target_is_directory=True)
        self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(kept.exists())

    def test_a_mount_inside_storage_is_refused(self):
        kept = self.package("mounted", age_days=90, kib=1024)
        code = self.run_in_process(mounts=lambda: {str(kept)}, high_kib=512, low_kib=256)
        self.assertNotEqual(code, 0)
        self.assertTrue(kept.exists(), "it would have deleted across a mount")
        self.assertTrue(self.stopped())

    def test_the_unit_is_confined_to_its_storage(self):
        unit = (DEPLOY / "tart-runner-fleet-npm-cache-prune.service").read_text()
        for line in ("ProtectSystem=strict", "ProtectHome=read-only", "NoNewPrivileges=yes", "PrivateTmp=yes",
                     "ReadWritePaths=%h/.local/share/tart-runner-fleet-npm-cache\n"):
            self.assertIn(line, unit)
        self.assertEqual(unit.count("ReadWritePaths="), 1)


class SparesWhatIsActive(Fixture):
    """Finding 2: a running install has no fallback, so an active package must survive."""

    def over_limit_with(self, side_effect):
        package = self.package("busy", age_days=90, kib=1024)

        def act(*_):
            side_effect(package)
            return 10**15

        self.run_in_process(free=act, high_kib=512, low_kib=256)
        return package

    def test_a_download_that_starts_after_the_listing_survives(self):
        # What the review reproduced: the directory and its temp tarball were deleted mid-download.
        def starts(package):
            (package / "busy-2.0.0.tgz.tmp-download").write_bytes(b"partial")

        package = self.over_limit_with(starts)
        self.assertTrue(package.exists(), "evicted a package that started downloading after the listing")
        self.assertTrue((package / "busy-2.0.0.tgz.tmp-download").exists())

    def test_a_read_after_the_listing_survives(self):
        def reads(package):
            os.utime(package / "package.json")

        self.assertTrue(self.over_limit_with(reads).exists(), "evicted a package read after the listing")

    def test_a_temp_file_protects_a_package_however_old_it_looks(self):
        package = self.package("stale-looking", age_days=90, kib=1024)
        temp = package / "stale-looking-1.0.0.tgz.tmp12345"
        temp.write_bytes(b"partial")
        os.utime(temp, (1, 1))
        os.utime(package, (1, 1))
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(package.exists(), "evicted a package holding a temp file")
        self.assertEqual(result.returncode, 1, "over the limit with nothing evictable must stop, not grow")
        self.assertTrue(self.stopped())

    def test_what_was_touched_inside_the_grace_period_survives_and_a_stuck_limit_stops(self):
        in_flight = self.package("in-flight", age_days=0, kib=1024)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(in_flight.exists(), "evicted a package that may be mid-download")
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.stopped())


class TellsTheTruth(Fixture):
    """Findings 3 and 4: recovery is measured, and a failed stop is never quiet."""

    def test_free_space_that_did_not_come_back_is_not_recovery(self):
        # What the review reproduced: scanned allocation credited as free, exit 0 under the reserve.
        self.package("old", age_days=90, kib=1024)
        reserve = 1024**3 * 10.0
        free_now = int(reserve * 1024**3) - 1024 * KIB
        code = self.run_in_process(free=lambda *_: free_now, high_kib=1024 * 1024, low_kib=512 * 1024, reserve_gib=reserve)
        self.assertEqual(code, 1, "it reported recovery the filesystem never showed")
        self.assertTrue(self.stopped())

    def test_a_failed_stop_is_its_own_visible_failure(self):
        self.stand_in_systemctl(stop_exit=1, still_active=True)
        self.package("in-flight", age_days=0, kib=1024)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertEqual(result.returncode, 3)
        self.assertIn("could not stop", result.stderr)

    def test_a_stop_that_reports_success_is_still_verified(self):
        self.stand_in_systemctl(stop_exit=0, still_active=True)
        self.package("in-flight", age_days=0, kib=1024)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(self.verified_stop(), "it never checked the cache had really stopped")
        self.assertEqual(result.returncode, 3)

    def test_a_filesystem_error_stops_the_cache(self):
        # What the review reproduced: an exception bypassed the stop entirely.
        self.package("old", age_days=90, kib=1024)

        def fails(*_):
            raise OSError("the filesystem went away")

        code = self.run_in_process(free=fails, high_kib=512, low_kib=256)
        self.assertEqual(code, 1)
        self.assertTrue(self.stopped(), "an enforcement failure left the cache running")

    def test_nonsense_limits_are_refused(self):
        result = self.run_prune(high_kib=256, low_kib=512)
        self.assertNotEqual(result.returncode, 0)


class OutsideTheTimedProcess(Fixture):
    """If systemd kills or never starts the prune, nothing in the script can stop the cache."""

    stop_unit = DEPLOY / "tart-runner-fleet-npm-cache-stop.service"

    def run_stop_unit(self):
        command = re.search(r"^ExecStart=/bin/sh -c '(.*)'$", self.stop_unit.read_text(), re.M).group(1)
        env = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}"}
        return subprocess.run(["sh", "-c", command], env=env, capture_output=True, text=True, check=False)

    def test_a_failed_prune_hands_over_to_the_stop_unit(self):
        prune = (DEPLOY / "tart-runner-fleet-npm-cache-prune.service").read_text()
        self.assertIn(f"\nOnFailure={self.stop_unit.name}\n", prune.split("[Service]")[0])

    def test_the_stop_unit_stops_the_cache_and_confirms_it(self):
        result = self.run_stop_unit()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.stopped())
        self.assertTrue(self.verified_stop())

    def test_the_stop_unit_fails_visibly_if_the_cache_is_still_up(self):
        self.stand_in_systemctl(stop_exit=1, still_active=True)
        self.assertNotEqual(self.run_stop_unit().returncode, 0)
        self.stand_in_systemctl(stop_exit=0, still_active=True)
        self.assertNotEqual(self.run_stop_unit().returncode, 0, "a stop that reported success must still be confirmed")

    def test_the_stop_unit_has_no_failure_handler_of_its_own(self):
        self.assertNotIn("\nOnFailure=", self.stop_unit.read_text(), "a failing stop must not loop")


class InstallAfterRollback(Fixture):
    """Rollback leaves the storage in place, so the documented install must cope with it."""

    def run_snippet(self, inode="256", filesystem="btrfs"):
        operations = (DEPLOY.parent.parent / "docs" / "OPERATIONS.md").read_text()
        snippet = re.search(r"^STORAGE=~/\.local/share/tart-runner-fleet-npm-cache\n.*?^fi$", operations, re.M | re.S).group(0)
        for name, body in (("btrfs", f'echo "$@" >> {self.root}/btrfs.calls\nmkdir -p "$3"'),
                           ("stat", f'case "$*" in *%T*) echo {filesystem} ;; *) echo {inode} ;; esac')):
            stand_in = self.bin / name
            stand_in.write_text(f"#!/bin/sh\n{body}\n")
            stand_in.chmod(0o755)
        env = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", "HOME": str(self.home)}
        return subprocess.run(["bash", "-euc", snippet], env=env, capture_output=True, text=True, check=False)

    def created(self):
        return (self.root / "btrfs.calls").exists()

    def test_a_first_install_creates_the_subvolume(self):
        self.data.rmdir()
        self.storage.rmdir()
        result = self.run_snippet()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.created())

    def test_an_install_after_rollback_reuses_the_subvolume_and_its_data(self):
        kept = self.package("kept", age_days=1)
        result = self.run_snippet()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.created())
        self.assertTrue(kept.exists())

    def test_anything_else_at_that_path_is_refused_untouched(self):
        for inode, filesystem in (("12345", "btrfs"), ("256", "ext2/ext3")):
            result = self.run_snippet(inode=inode, filesystem=filesystem)
            self.assertNotEqual(result.returncode, 0, f"accepted inode {inode} on {filesystem}")
            self.assertFalse(self.created())
            self.assertIn("migrate", result.stderr)
        self.data.rmdir()
        self.storage.rmdir()
        self.storage.symlink_to(self.outside, target_is_directory=True)
        self.assertNotEqual(self.run_snippet().returncode, 0, "accepted a symlink")


class Deploy(unittest.TestCase):
    """The units agree with each other: a prune of the wrong path enforces nothing."""

    def test_the_timer_prunes_the_storage_the_quadlet_mounts(self):
        service = (DEPLOY / "tart-runner-fleet-npm-cache-prune.service").read_text()
        quadlet = (DEPLOY / "tart-runner-fleet-npm-cache.container").read_text()
        config = (DEPLOY / "verdaccio.yaml").read_text()
        self.assertIn("Volume=%h/.local/share/tart-runner-fleet-npm-cache:/verdaccio/storage", quadlet)
        self.assertIn("storage: /verdaccio/storage/data", config)
        self.assertIn("--data %h/.local/share/tart-runner-fleet-npm-cache/data ", service)

    def test_the_limits_keep_their_order(self):
        service = (DEPLOY / "tart-runner-fleet-npm-cache-prune.service").read_text()
        limit = lambda flag: float(service.split(f"--{flag} ")[1].split()[0])
        self.assertLess(limit("low-gib"), limit("high-gib"))
        self.assertGreater(limit("reserve-gib"), 60, "the reserve must sit above minFreeDiskGb")


if __name__ == "__main__":
    unittest.main()
