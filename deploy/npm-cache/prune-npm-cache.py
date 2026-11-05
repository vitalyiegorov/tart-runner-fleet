#!/usr/bin/env python3
"""Keep a node's npm cache inside its limits (ADR 0093).

Run by tart-runner-fleet-npm-cache-prune.timer. Past --high-gib, or with the filesystem under
--reserve-gib free, it evicts the least recently used objects down to --low-gib. The gate fetches
an evicted object again on its next read, and a reader that already has it open keeps its bytes.

It deletes only inside the one directory it is pinned to, $HOME/.local/share/
tart-runner-fleet-npm-cache/data, reached with no symlink and no mount at or under it, and every
delete is relative to that verified directory. It leaves alone anything touched in the last
--grace-minutes, and it looks again immediately before each delete.

The gate writes nothing it can't: a directory with no write permission is a cache that serves what
it holds and passes the rest straight to npmjs. So any failure to enforce freezes the directory
and exits 1, and the next run that gets back inside the limits thaws it. Exit 3 means it could not
confirm a freeze, or would not trust the path enough to try. The unit's OnFailure= then freezes it
from outside, as it does if systemd kills this script or never starts it.

This checks growth between runs and has no strict bound on it: only a kernel quota could do that.
"""
import argparse
import errno
import math
import os
import re
import stat
import sys
import time

GIB = 1024**3
STORAGE = ".local/share/tart-runner-fleet-npm-cache"
DIRECTORY = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
WRITABLE = 0o700
FROZEN = 0o500
OBJECT = re.compile(r"^[0-9a-f]{64}$")
TEMP = re.compile(r"^\.tmp-[0-9]+$")
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
    """An fd for the expected data directory, or None if the gate has not created it yet."""
    home = os.environ.get("HOME", "")
    if not os.path.isabs(home) or os.path.normpath(home) != home:
        raise Refused(f"HOME {home!r} is not a clean absolute path")
    expected = os.path.join(home, STORAGE, "data")
    if data != expected:
        raise Refused(f"--data {data!r} is not the npm cache's storage, {expected}")
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


def last_used(info):
    return max(info.st_atime, info.st_mtime)


def listing(fd, device):
    """Every stored object and temp as {name: stat}. Anything else means this is not what the gate
    wrote, so nothing here can be trusted to be deletable."""
    found = {}
    with os.scandir(fd) as entries:
        for entry in entries:
            try:
                info = entry.stat(follow_symlinks=False)
            except FileNotFoundError:  # The gate replaced it, or removed its temp, meanwhile.
                continue
            if not stat.S_ISREG(info.st_mode) or info.st_dev != device or not (OBJECT.match(entry.name) or TEMP.match(entry.name)):
                raise Refused(f"{entry.name!r} does not belong in a flat npm cache")
            found[entry.name] = info
    return found


def evict(fd, name, device, oldest_allowed):
    """Delete one file if, looking again now, it is still idle. Returns what was reclaimed."""
    try:
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        if not stat.S_ISREG(info.st_mode) or info.st_dev != device or last_used(info) >= oldest_allowed:
            return 0
        os.unlink(name, dir_fd=fd)
    except FileNotFoundError:
        return 0
    return info.st_blocks * 512


def allocated(found):
    return sum(info.st_blocks * 512 for info in found.values())


def enforce(fd, args):
    """None when inside the limits, otherwise what is wrong."""
    high, low, reserve = args.high_gib * GIB, args.low_gib * GIB, args.reserve_gib * GIB
    device = os.fstat(fd).st_dev
    found = listing(fd, device)
    free = free_bytes(fd)
    oldest_allowed = time.time() - args.grace_minutes * 60
    # Thaw first: it cannot evict from a frozen directory, and a frozen cache has nothing to protect.
    os.fchmod(fd, WRITABLE)
    reclaimed = sum(evict(fd, name, device, oldest_allowed) for name in found if TEMP.match(name))
    objects = {name: info for name, info in found.items() if OBJECT.match(name)}
    size = allocated(objects)
    evicted = 0
    if size > high or free < reserve:
        for name in sorted(objects, key=lambda name: last_used(objects[name])):
            if size - reclaimed <= low and free + reclaimed >= reserve:
                break
            freed = evict(fd, name, device, oldest_allowed)
            evicted += 1 if freed else 0
            reclaimed += freed
    # Measure again rather than trust the arithmetic. btrfs frees asynchronously, so wait a little
    # for space the deletes could have returned, and never claim it unseen.
    size = allocated(listing(fd, device))
    could_recover = free + reclaimed >= reserve
    free = free_bytes(fd)
    deadline = time.monotonic() + args.settle_seconds
    while reclaimed and could_recover and free < reserve and time.monotonic() < deadline:
        time.sleep(SETTLE_POLL_SECONDS)
        free = free_bytes(fd)
    print(f"npm-cache: size={size / GIB:.2f}GiB free={free / GIB:.1f}GiB evicted={evicted} reclaimed={reclaimed / GIB:.2f}GiB")
    faults = []
    if size > high:
        faults.append(f"still {size / GIB:.2f}GiB, over the {args.high_gib}GiB limit")
    if free < reserve:
        faults.append(f"{free / GIB:.1f}GiB free, under the {args.reserve_gib}GiB reserve")
    return "; ".join(faults) or None


def freeze(fd):
    """Stop the cache growing and confirm it. 1 when it cannot grow, 3 when that is unconfirmed."""
    try:
        os.fchmod(fd, FROZEN)
        if stat.S_IMODE(os.fstat(fd).st_mode) & 0o222 == 0:
            return 1
    except OSError as error:
        print(f"npm-cache: {error}", file=sys.stderr)
    print("npm-cache: could not freeze the cache", file=sys.stderr)
    return 3


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--data", required=True, help="the gate's --npm-cache directory, which must be the expected one")
    parser.add_argument("--high-gib", type=float, default=40.0, help="evict past this size")
    parser.add_argument("--low-gib", type=float, default=32.0, help="evict down to this size")
    parser.add_argument("--reserve-gib", type=float, default=100.0, help="free space the filesystem must keep")
    parser.add_argument("--grace-minutes", type=float, default=15.0, help="never evict what was touched this recently")
    parser.add_argument("--settle-seconds", type=float, default=60.0, help="how long deleted space may take to come back")
    args = parser.parse_args()
    try:
        limits = (args.high_gib, args.low_gib, args.reserve_gib)
        if not all(math.isfinite(limit) for limit in limits) or not 0 < args.low_gib < args.high_gib or args.reserve_gib < 0:
            raise Refused(f"limits make no sense: high {args.high_gib}, low {args.low_gib}, reserve {args.reserve_gib}")
        fd = pin_data(args.data)
    except Exception as error:  # Nothing is pinned, so nothing here can be trusted to freeze.
        print(f"npm-cache: {type(error).__name__}: {error}", file=sys.stderr)
        sys.exit(3)
    if fd is None:
        sys.exit(0)
    try:
        fault = enforce(fd, args)
    except Exception as error:  # Every failure to enforce freezes the cache, whatever it was.
        fault = f"{type(error).__name__}: {error}"
    if fault is None:
        sys.exit(0)
    print(f"npm-cache: freezing the cache: {fault}", file=sys.stderr)
    sys.exit(freeze(fd))


if __name__ == "__main__":
    main()
