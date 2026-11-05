#!/usr/bin/env python3
"""Keep a node's npm cache inside its limits (ADR 0093). Verdaccio does not prune.

Run by tart-runner-fleet-npm-cache-prune.timer. Past --high-gib, or with the filesystem
under --reserve-gib free, it evicts whole packages, least recently used first, down to
--low-gib. A whole package, because metadata is the larger part of this cache and a
tarball-only prune would never reclaim it. The next read fetches an evicted package again.

It deletes only inside the one directory it is pinned to, $HOME/.local/share/
tart-runner-fleet-npm-cache/data, reached with no symlink and no mount at or under it, and
every delete is relative to a directory it has verified. It never evicts a package touched
in the last --grace-minutes or holding a file that is not plainly finished, and it looks
again immediately before each delete, because a running install has no fallback.

Any enforcement failure stops the cache and exits 1, and exit 3 means it could not confirm
that stop. New guests then go direct and `fleet doctor` says `npm cache not ready`. Bringing
the cache back is an operator's `systemctl --user start`.

This checks growth between runs and has no strict bound on it: only a kernel quota could do that.
If systemd kills this script or never starts it, the unit's OnFailure= stops the cache instead.
"""
import argparse
import errno
import math
import os
import shutil
import stat
import subprocess
import sys
import time

GIB = 1024**3
STORAGE = ".local/share/tart-runner-fleet-npm-cache"
SERVICE = "tart-runner-fleet-npm-cache.service"
DIRECTORY = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
SETTLE_POLL_SECONDS = 5


class Refused(Exception):
    """This run cannot enforce anything safely."""


def mount_points():
    """Every mount point this process sees, unescaped."""
    with open("/proc/self/mountinfo", encoding="utf-8") as table:
        return {line.split()[4].encode().decode("unicode_escape") for line in table}


def free_bytes(fd):
    stats = os.fstatvfs(fd)
    return stats.f_bavail * stats.f_frsize


def pin_data(data):
    """An fd for the expected data directory, or None if Verdaccio has not created it yet."""
    home = os.environ.get("HOME", "")
    if not os.path.isabs(home) or os.path.normpath(home) != home:
        raise Refused(f"HOME {home!r} is not a clean absolute path")
    expected = os.path.join(home, STORAGE, "data")
    if data != expected:
        raise Refused(f"--data {data!r} is not the npm cache's storage, {expected}")
    if not getattr(shutil.rmtree, "avoids_symlink_attacks", False):
        raise Refused("this platform's rmtree cannot delete safely")
    for mount in mount_points():
        if mount == expected or mount.startswith(expected + "/"):
            raise Refused(f"{mount} is a mount at or under the npm cache's storage")
    parent = os.open("/", os.O_PATH | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        for part in os.path.dirname(expected).strip("/").split("/"):
            following = os.open(part, os.O_PATH | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
            os.close(parent)
            parent = following
        try:
            pinned = os.open("data", DIRECTORY, dir_fd=parent)
        except FileNotFoundError:
            return None
        if os.fstat(pinned).st_dev != os.fstat(parent).st_dev:
            os.close(pinned)
            raise Refused(f"{expected} is on another device than its parent")
        return pinned
    except OSError as error:
        if error.errno in (errno.ELOOP, errno.ENOTDIR):
            raise Refused(f"{expected} goes through a symlink or a non-directory") from error
        raise
    finally:
        os.close(parent)


def real_directories(fd):
    with os.scandir(fd) as entries:
        return [entry.name for entry in entries if entry.is_dir(follow_symlinks=False) and not entry.name.startswith(".")]


class Package:
    """What a package directory holds right now. Only a package that is plainly at rest,
    all on the cache's own device, may be evicted."""

    def __init__(self, parent_fd, name, device):
        self.used, self.last_used, self.at_rest = 0, 0.0, True
        fd = os.open(name, DIRECTORY, dir_fd=parent_fd)
        try:
            # Directories count by mtime only: a create or rename inside changes it, and this
            # very walk changes their atime.
            for _, _, files, root in os.fwalk(".", dir_fd=fd, follow_symlinks=False):
                info = os.fstat(root)
                self.used += info.st_blocks * 512
                self.last_used = max(self.last_used, info.st_mtime)
                self.at_rest = self.at_rest and info.st_dev == device
                for file in files:
                    self._count(root, file, device)
        finally:
            os.close(fd)

    def _count(self, root, file, device):
        try:
            info = os.lstat(file, dir_fd=root)
        except FileNotFoundError:  # Verdaccio renamed or removed it meanwhile.
            return
        self.used += info.st_blocks * 512
        self.last_used = max(self.last_used, info.st_atime, info.st_mtime)
        finished = stat.S_ISREG(info.st_mode) and (file == "package.json" or file.endswith(".tgz"))
        self.at_rest = self.at_rest and finished and info.st_dev == device


def open_scope(data_fd, scope, device):
    """The verified directory a package sits in: data itself, or one of its `@scope`s. None if it
    is not a real directory on this device any more, which is never followed."""
    if scope is None:
        return data_fd
    try:
        fd = os.open(scope, DIRECTORY, dir_fd=data_fd)
    except OSError:
        return None
    if os.fstat(fd).st_dev != device:
        os.close(fd)
        return None
    return fd


def listing(data_fd, device):
    """Every package, as {(scope, name): Package}."""
    packages = {}
    for entry in real_directories(data_fd):
        scope = entry if entry.startswith("@") else None
        parent = open_scope(data_fd, scope, device)
        if parent is None:
            continue
        try:
            for name in real_directories(parent) if scope else [entry]:
                try:
                    packages[(scope, name)] = Package(parent, name, device)
                except (FileNotFoundError, NotADirectoryError):
                    continue  # Gone or swapped since it was listed.
        finally:
            if scope:
                os.close(parent)
    return packages


def evict(data_fd, key, device, grace_seconds):
    """Delete one package if, looking again now, it is still idle. Returns what was reclaimed."""
    scope, name = key
    parent = open_scope(data_fd, scope, device)
    if parent is None:
        return 0
    try:
        try:
            now = Package(parent, name, device)
        except OSError:
            return 0  # Gone, or replaced by something that is not a real directory.
        if not now.at_rest or now.last_used >= time.time() - grace_seconds:
            return 0
        shutil.rmtree(name, dir_fd=parent)
        return now.used
    finally:
        if scope:
            os.close(parent)


def enforce(args):
    """None when inside the limits, otherwise what is wrong."""
    high, low, reserve = args.high_gib * GIB, args.low_gib * GIB, args.reserve_gib * GIB
    if not all(math.isfinite(limit) for limit in (high, low, reserve)) or not 0 < low < high or reserve < 0:
        raise Refused(f"limits make no sense: high {args.high_gib}, low {args.low_gib}, reserve {args.reserve_gib}")
    data_fd = pin_data(args.data)
    if data_fd is None:
        return None
    try:
        device = os.fstat(data_fd).st_dev
        grace_seconds = args.grace_minutes * 60
        packages = listing(data_fd, device)
        size = sum(package.used for package in packages.values())
        free = free_bytes(data_fd)
        evicted, reclaimed = 0, 0
        if size > high or free < reserve:
            for key in sorted(packages, key=lambda key: packages[key].last_used):
                if size - reclaimed <= low and free + reclaimed >= reserve:
                    break
                freed = evict(data_fd, key, device, grace_seconds)
                evicted += 1 if freed else 0
                reclaimed += freed
            # Measure again rather than trust the arithmetic. btrfs frees asynchronously, so wait
            # a little for space the deletes could have returned, and never claim it unseen.
            size = sum(package.used for package in listing(data_fd, device).values())
            free = free_bytes(data_fd)
            deadline = time.monotonic() + args.settle_seconds
            while reclaimed and free < reserve and time.monotonic() < deadline:
                time.sleep(SETTLE_POLL_SECONDS)
                free = free_bytes(data_fd)
        print(f"npm-cache: size={size / GIB:.2f}GiB free={free / GIB:.1f}GiB evicted={evicted} reclaimed={reclaimed / GIB:.2f}GiB")
        faults = []
        if size > high:
            faults.append(f"still {size / GIB:.2f}GiB, over the {args.high_gib}GiB limit")
        if free < reserve:
            faults.append(f"{free / GIB:.1f}GiB free, under the {args.reserve_gib}GiB reserve")
        return "; ".join(faults) or None
    finally:
        os.close(data_fd)


def stop_cache():
    """Stop the cache and confirm it. 1 when it is down, 3 when that cannot be confirmed."""
    try:
        subprocess.run(["systemctl", "--user", "stop", SERVICE], check=False, timeout=180)
        state = subprocess.run(["systemctl", "--user", "is-active", SERVICE], check=False, timeout=30,
                               capture_output=True, text=True).stdout.strip()
    except (OSError, subprocess.SubprocessError) as error:
        state = f"unknown ({error})"
    if state in ("inactive", "failed"):
        return 1
    print(f"npm-cache: could not stop {SERVICE}: it reads {state}", file=sys.stderr)
    return 3


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--data", required=True, help="Verdaccio's storage directory, which must be the expected one")
    parser.add_argument("--high-gib", type=float, default=40.0, help="evict past this size")
    parser.add_argument("--low-gib", type=float, default=32.0, help="evict down to this size")
    parser.add_argument("--reserve-gib", type=float, default=100.0, help="free space the filesystem must keep")
    parser.add_argument("--grace-minutes", type=float, default=15.0, help="never evict what was touched this recently")
    parser.add_argument("--settle-seconds", type=float, default=60.0, help="how long deleted space may take to come back")
    args = parser.parse_args()
    try:
        fault = enforce(args)
    except Exception as error:  # Every failure to enforce stops the cache, whatever it was.
        fault = f"{type(error).__name__}: {error}"
    if fault is None:
        sys.exit(0)
    print(f"npm-cache: stopping {SERVICE}: {fault}", file=sys.stderr)
    sys.exit(stop_cache())


if __name__ == "__main__":
    main()
