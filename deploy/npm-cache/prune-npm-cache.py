#!/usr/bin/env python3
"""Keep a node's npm cache inside its limits (ADR 0093). Verdaccio does not prune.

Run by tart-runner-fleet-npm-cache-prune.timer. Past --high-gib, or with the filesystem
under --reserve-gib free, it evicts whole packages, least recently used first, down to
--low-gib. A whole package, because metadata is the larger part of this cache and a
tarball-only prune would never reclaim it. The next read fetches an evicted package again.

If it cannot get back inside the limits it stops the cache and exits 1, rather than let
it fill the disk. New guests then go direct and `fleet doctor` says `npm cache not ready`.
Bringing the cache back is an operator's `systemctl --user start`.

This bounds growth between runs, not a single write: only a kernel quota could do that.
"""
import argparse
import os
import shutil
import stat
import subprocess
import sys
import time

GIB = 1024**3


def tree_usage(directory):
    """Allocated bytes and the newest access or change under one package, links unfollowed."""
    used, last_used = 0, 0.0
    for parent, _, files in os.walk(directory):
        for name in files:
            try:
                info = os.lstat(os.path.join(parent, name))
            except FileNotFoundError:  # Verdaccio renamed or removed it meanwhile.
                continue
            used += info.st_blocks * 512
            last_used = max(last_used, info.st_atime, info.st_mtime)
    return used, last_used


def real_directories(path):
    with os.scandir(path) as entries:
        return [entry for entry in entries if entry.is_dir(follow_symlinks=False)]


def packages(data):
    """Each package directory: a top-level one, or one inside an `@scope`."""
    for entry in real_directories(data):
        if entry.name.startswith("@"):
            yield from (child.path for child in real_directories(entry.path))
        else:
            yield entry.path


def free_bytes(path):
    stats = os.statvfs(path)
    return stats.f_bavail * stats.f_frsize


def enforce(args):
    data, high, low, reserve = args.data, args.high_gib * GIB, args.low_gib * GIB, args.reserve_gib * GIB
    if not os.path.lexists(data):
        return 0
    if not stat.S_ISDIR(os.lstat(data).st_mode):
        print(f"npm-cache: {data} is not a real directory; refusing to prune", file=sys.stderr)
        return 2
    usage = {package: tree_usage(package) for package in packages(data)}
    size = sum(used for used, _ in usage.values())
    free = free_bytes(data)
    evicted, reclaimed = 0, 0
    if size > high or free < reserve:
        newest_allowed = time.time() - args.grace_minutes * 60
        # A package touched within the grace period may be mid-download.
        for package in sorted((p for p, u in usage.items() if u[1] < newest_allowed), key=lambda p: usage[p][1]):
            if size - reclaimed <= low and free + reclaimed >= reserve:
                break
            shutil.rmtree(package, ignore_errors=True)
            if not os.path.lexists(package):
                evicted += 1
                reclaimed += usage[package][0]
    size -= reclaimed
    # btrfs frees asynchronously, so what was just reclaimed counts as free already.
    free += reclaimed
    print(f"npm-cache: size={size / GIB:.2f}GiB free={free / GIB:.1f}GiB evicted={evicted} reclaimed={reclaimed / GIB:.2f}GiB")
    faults = []
    if size > high:
        faults.append(f"still {size / GIB:.2f}GiB, over the {args.high_gib}GiB limit")
    if free < reserve:
        faults.append(f"{free / GIB:.1f}GiB free, under the {args.reserve_gib}GiB reserve")
    if not faults:
        return 0
    print(f"npm-cache: stopping {args.service}: {'; '.join(faults)}", file=sys.stderr)
    subprocess.run(["systemctl", "--user", "stop", args.service], check=False)
    return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--data", required=True, help="Verdaccio's storage directory")
    parser.add_argument("--service", default="tart-runner-fleet-npm-cache.service")
    parser.add_argument("--high-gib", type=float, default=40.0, help="evict past this size")
    parser.add_argument("--low-gib", type=float, default=32.0, help="evict down to this size")
    parser.add_argument("--reserve-gib", type=float, default=100.0, help="free space the filesystem must keep")
    parser.add_argument("--grace-minutes", type=float, default=15.0, help="never evict what was touched this recently")
    sys.exit(enforce(parser.parse_args()))


if __name__ == "__main__":
    main()
