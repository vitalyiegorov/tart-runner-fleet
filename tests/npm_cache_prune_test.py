"""The npm cache's limits: stay inside storage, spare what is active, tell the truth."""
import hashlib
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
WRITABLE = 0o700
FROZEN = 0o500


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
        self.data.chmod(WRITABLE)
        self.addCleanup(lambda: self.data.is_dir() and not self.data.is_symlink() and self.data.chmod(WRITABLE))
        self.outside = self.root / "outside"
        self.outside.mkdir()
        self.count = 0

    def stored(self, age_days, kib=64, read_days_ago=None):
        """One stored object as the gate leaves it: a file named for the hash of its key."""
        self.count += 1
        path = self.data / hashlib.sha256(str(self.count).encode()).hexdigest()
        path.write_bytes(os.urandom(kib * KIB))
        when = time.time() - age_days * 86400
        read = when if read_days_ago is None else time.time() - read_days_ago * 86400
        os.utime(path, (read, when))
        return path

    def temp(self, age_days, kib=64):
        path = self.data / f".tmp-{self.count + 1000}"
        self.count += 1
        path.write_bytes(os.urandom(kib * KIB))
        when = time.time() - age_days * 86400
        os.utime(path, (when, when))
        return path

    def arguments(self, high_kib=1024, low_kib=512, reserve_gib=0.0, data=None):
        return ["--data", str(self.data if data is None else data), "--high-gib", str(gib(high_kib)),
                "--low-gib", str(gib(low_kib)), "--reserve-gib", str(reserve_gib), "--grace-minutes", "15",
                "--settle-seconds", "0"]

    def run_prune(self, home=None, **limits):
        env = {**os.environ, "HOME": str(self.home if home is None else home)}
        return subprocess.run(["python3", "-I", str(SCRIPT), *self.arguments(**limits)], env=env,
                              capture_output=True, text=True, check=False)

    def run_in_process(self, *, free=None, mounts=None, fchmod=None, **limits):
        """The real script in this process, so a test can act at the moment it measures free space,
        which falls after it has listed objects and before it deletes any."""
        spec = importlib.util.spec_from_file_location("prune_npm_cache", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        argv = ["prune-npm-cache.py", *self.arguments(**limits)]
        patches = [mock.patch.dict(os.environ, {"HOME": str(self.home)}), mock.patch.object(sys, "argv", argv)]
        if fchmod is not None:
            patches.append(mock.patch.object(module.os, "fchmod", fchmod))
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        if free is not None:
            module.free_bytes = free
        if mounts is not None:
            module.mount_points = mounts
        with self.assertRaises(SystemExit) as finished:
            module.main()
        return finished.exception.code

    def frozen(self):
        return stat.S_IMODE(os.stat(self.data).st_mode) & 0o222 == 0


class Limits(Fixture):
    def test_under_the_limits_nothing_is_touched(self):
        kept = self.stored(age_days=90)
        result = self.run_prune()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(kept.exists())
        self.assertFalse(self.frozen())

    def test_over_the_limit_evicts_the_oldest_down_to_the_low_mark(self):
        oldest = self.stored(age_days=90, kib=512)
        older = self.stored(age_days=60, kib=512)
        recent = self.stored(age_days=2, kib=512)
        result = self.run_prune(high_kib=1024, low_kib=600)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(oldest.exists())
        self.assertFalse(older.exists())
        self.assertTrue(recent.exists(), "evicted past the low-water mark")
        self.assertFalse(self.frozen())

    def test_a_read_keeps_an_object_alive(self):
        read_lately = self.stored(age_days=90, kib=512, read_days_ago=1)
        older = self.stored(age_days=30, kib=512)
        self.stored(age_days=20, kib=512)
        self.run_prune(high_kib=1024, low_kib=900)
        self.assertFalse(older.exists())
        self.assertTrue(read_lately.exists(), "last use must be the newest access, not the file's age")

    def test_a_breached_reserve_evicts_even_under_the_size_limit_and_then_freezes(self):
        old = self.stored(age_days=90)
        result = self.run_prune(high_kib=1024 * 1024, low_kib=512 * 1024, reserve_gib=1e9)
        self.assertEqual(result.returncode, 1)
        self.assertFalse(old.exists(), "the cache kept data while the disk was under its reserve")
        self.assertTrue(self.frozen(), "it must stop the cache growing rather than let it refill a full disk")
        self.assertIn("reserve", result.stderr)

    def test_a_missing_data_directory_is_not_a_fault(self):
        self.data.rmdir()
        result = self.run_prune()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_it_says_what_it_did(self):
        self.stored(age_days=90, kib=1024)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertIn("evicted=1", result.stdout)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_a_crashed_gates_temp_is_removed_even_under_the_limits(self):
        old = self.temp(age_days=2)
        young = self.temp(age_days=0)
        kept = self.stored(age_days=90)
        result = self.run_prune()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(old.exists(), "a temp a dead gate left behind must not stay forever")
        self.assertTrue(young.exists(), "that temp is a download in progress")
        self.assertTrue(kept.exists())

    def test_a_frozen_cache_thaws_itself_once_back_inside_the_limits(self):
        self.stored(age_days=90, kib=1024)
        self.data.chmod(FROZEN)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.frozen())
        self.assertEqual(list(self.data.iterdir()), [], "it must be able to evict from a frozen cache")


class StaysInsideStorage(Fixture):
    """Nothing outside the pinned storage directory may ever be deleted."""

    def precious(self):
        path = self.outside / hashlib.sha256(b"precious").hexdigest()
        path.write_bytes(os.urandom(1024 * KIB))
        os.utime(path, (1, 1))
        return path

    def assert_refused(self, result, kept):
        self.assertEqual(result.returncode, 3, f"it ran on a path it should have refused: {result.stderr}")
        self.assertTrue(kept.exists(), "it deleted outside storage")

    def test_a_symlinked_ancestor_is_refused(self):
        real_storage = self.outside / "real-storage"
        (real_storage / "data").mkdir(parents=True)
        kept = real_storage / "data" / hashlib.sha256(b"kept").hexdigest()
        kept.write_bytes(os.urandom(1024 * KIB))
        os.utime(kept, (1, 1))
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
        kept = self.stored(age_days=90, kib=1024)
        for path in ("", "data", str(self.data) + "/../data", str(self.data) + "/"):
            result = self.run_prune(high_kib=512, low_kib=256, data=path)
            self.assertEqual(result.returncode, 3, f"accepted {path!r}")
            self.assertTrue(kept.exists(), f"deleted through {path!r}")

    def test_an_unusable_home_is_refused(self):
        kept = self.stored(age_days=90, kib=1024)
        for home in ("", "relative/home", str(self.outside)):
            result = self.run_prune(high_kib=512, low_kib=256, home=home)
            self.assertEqual(result.returncode, 3, f"accepted HOME={home!r}")
            self.assertTrue(kept.exists(), f"deleted under HOME={home!r}")

    def test_an_object_swapped_for_a_symlink_mid_run_redirects_nothing(self):
        victim = self.stored(age_days=90, kib=1024)
        kept = self.precious()

        def swap(*_):
            if not victim.is_symlink():
                victim.unlink()
                victim.symlink_to(kept)
            return 10**15

        self.run_in_process(free=swap, high_kib=512, low_kib=256)
        self.assertTrue(kept.exists(), "the swap redirected the delete outside storage")
        self.assertEqual(kept.stat().st_size, 1024 * KIB)

    def test_anything_that_does_not_belong_refuses_and_freezes(self):
        link = self.data / hashlib.sha256(b"link").hexdigest()
        link.symlink_to(self.outside, target_is_directory=True)
        kept = self.precious()
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.frozen())
        self.assertTrue(link.is_symlink())
        self.assertTrue(kept.exists())

    def test_verdaccios_old_data_refuses_and_freezes_rather_than_being_deleted(self):
        package = self.data / "lodash"
        package.mkdir()
        (package / "package.json").write_text("{}")
        database = self.data / ".verdaccio-db.json"
        database.write_text("{}")
        result = self.run_prune()
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.frozen())
        self.assertTrue((package / "package.json").exists())
        self.assertTrue(database.exists())

    def test_a_mount_inside_storage_is_refused(self):
        kept = self.stored(age_days=90, kib=1024)
        code = self.run_in_process(mounts=lambda: {str(kept)}, high_kib=512, low_kib=256)
        self.assertEqual(code, 3)
        self.assertTrue(kept.exists(), "it would have deleted across a mount")

    def test_the_units_are_confined_to_their_storage(self):
        for name in ("tart-runner-fleet-npm-cache-prune.service", "tart-runner-fleet-npm-cache-freeze.service"):
            unit = (DEPLOY / name).read_text()
            for line in ("ProtectSystem=strict", "ProtectHome=read-only", "NoNewPrivileges=yes", "PrivateTmp=yes",
                         "RestrictAddressFamilies=none", "ReadWritePaths=%h/.local/share/tart-runner-fleet-npm-cache\n"):
                self.assertIn(line, unit, name)
            self.assertEqual(unit.count("ReadWritePaths="), 1, name)


class SparesWhatIsActive(Fixture):
    def test_a_read_after_the_listing_survives(self):
        busy = self.stored(age_days=90, kib=1024)

        def reads(*_):
            if busy.exists():
                os.utime(busy)
            return 10**15

        self.run_in_process(free=reads, high_kib=512, low_kib=256)
        self.assertTrue(busy.exists(), "evicted an object read after the listing")

    def test_what_was_touched_inside_the_grace_period_survives_and_a_stuck_limit_freezes(self):
        in_flight = self.stored(age_days=0, kib=1024)
        result = self.run_prune(high_kib=512, low_kib=256)
        self.assertTrue(in_flight.exists())
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.frozen())


class TellsTheTruth(Fixture):
    """Recovery is measured, and a failed freeze is never quiet."""

    def test_free_space_that_did_not_come_back_is_not_recovery(self):
        self.stored(age_days=90, kib=1024)
        reserve = 1024**3 * 10.0
        free_now = int(reserve * 1024**3) - 1024 * KIB
        code = self.run_in_process(free=lambda *_: free_now, high_kib=1024 * 1024, low_kib=512 * 1024, reserve_gib=reserve)
        self.assertEqual(code, 1, "it reported recovery the filesystem never showed")
        self.assertTrue(self.frozen())

    def test_a_failed_freeze_is_its_own_visible_failure(self):
        self.stored(age_days=0, kib=1024)

        def fails(*_):
            raise OSError("no")

        self.assertEqual(self.run_in_process(fchmod=fails, high_kib=512, low_kib=256), 3)

    def test_a_freeze_that_reports_success_is_still_verified(self):
        self.stored(age_days=0, kib=1024)
        self.assertEqual(self.run_in_process(fchmod=lambda *_: None, high_kib=512, low_kib=256), 3)

    def test_a_filesystem_error_freezes_the_cache(self):
        self.stored(age_days=90, kib=1024)

        def fails(*_):
            raise OSError("the filesystem went away")

        self.assertEqual(self.run_in_process(free=fails, high_kib=512, low_kib=256), 1)
        self.assertTrue(self.frozen(), "an enforcement failure left the cache growing")

    def test_nonsense_limits_are_refused(self):
        self.assertEqual(self.run_prune(high_kib=256, low_kib=512).returncode, 3)


class OutsideTheTimedProcess(Fixture):
    """If systemd kills or never starts the prune, nothing in the script can freeze the cache."""

    freeze_unit = DEPLOY / "tart-runner-fleet-npm-cache-freeze.service"

    def run_freeze_unit(self):
        command = re.search(r"^ExecStart=/bin/sh -c '(.*)'$", self.freeze_unit.read_text(), re.M).group(1)
        return subprocess.run(["sh", "-c", command.replace("%h", str(self.home))], capture_output=True, text=True, check=False)

    def test_a_failed_prune_hands_over_to_the_freeze_unit(self):
        prune = (DEPLOY / "tart-runner-fleet-npm-cache-prune.service").read_text()
        self.assertIn(f"\nOnFailure={self.freeze_unit.name}\n", prune.split("[Service]")[0])

    def test_the_freeze_unit_freezes_the_cache_and_confirms_it(self):
        result = self.run_freeze_unit()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.frozen())

    def test_the_freeze_unit_fails_visibly_if_it_cannot(self):
        self.data.rmdir()
        self.assertNotEqual(self.run_freeze_unit().returncode, 0)

    def test_the_freeze_unit_has_no_failure_handler_of_its_own(self):
        self.assertNotIn("\nOnFailure=", self.freeze_unit.read_text(), "a failing freeze must not loop")


class InstallAfterRollback(Fixture):
    """Rollback leaves the storage in place, so the documented install must cope with it."""

    def run_snippet(self, inode="256", filesystem="btrfs"):
        operations = (DEPLOY.parent.parent / "docs" / "OPERATIONS.md").read_text()
        snippet = re.search(r"^STORAGE=~/\.local/share/tart-runner-fleet-npm-cache\n.*?^fi$", operations, re.M | re.S).group(0)
        bin_dir = self.root / "bin"
        bin_dir.mkdir(exist_ok=True)
        for name, body in (("btrfs", f'echo "$@" >> {self.root}/btrfs.calls\nmkdir -p "$3"'),
                           ("stat", f'case "$*" in *%T*) echo {filesystem} ;; *) echo {inode} ;; esac')):
            stand_in = bin_dir / name
            stand_in.write_text(f"#!/bin/sh\n{body}\n")
            stand_in.chmod(0o755)
        env = {**os.environ, "PATH": f"{bin_dir}:{os.environ['PATH']}", "HOME": str(self.home)}
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
        kept = self.stored(age_days=1)
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

    def test_the_timer_prunes_the_directory_the_gate_writes(self):
        service = (DEPLOY / "tart-runner-fleet-npm-cache-prune.service").read_text()
        drop_in = (DEPLOY / "tart-runner-fleet-cache-gate.service.d" / "npm-cache.conf").read_text()
        self.assertIn("--npm-cache %h/.local/share/tart-runner-fleet-npm-cache/data", drop_in)
        self.assertIn("--data %h/.local/share/tart-runner-fleet-npm-cache/data ", service)

    def test_the_limits_keep_their_order(self):
        service = (DEPLOY / "tart-runner-fleet-npm-cache-prune.service").read_text()
        limit = lambda flag: float(service.split(f"--{flag} ")[1].split()[0])
        self.assertLess(limit("low-gib"), limit("high-gib"))
        self.assertGreater(limit("reserve-gib"), 60, "the reserve must sit above minFreeDiskGb")


if __name__ == "__main__":
    unittest.main()
