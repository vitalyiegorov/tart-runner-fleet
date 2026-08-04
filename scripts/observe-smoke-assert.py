"""Assert that one `fleet status --output json` document is the observe steady state.

Read by scripts/observe-smoke.sh, which boots the daemon that produced it. The
assertions live here rather than inline so that a failure prints the whole
document an operator would otherwise have to reproduce by hand.
"""

import json
import sys


def main(path: str) -> int:
    with open(path, encoding="utf-8") as handle:
        data = json.load(handle)["data"]

    failures = []
    if data.get("controllerMode") != "observe":
        failures.append(f'controllerMode={data.get("controllerMode")!r}, want "observe"')
    if not data.get("ready", {}).get("ok"):
        failures.append(f'ready={data.get("ready")!r}')

    # The scheduler observation is fresh only when the host and instance
    # observations feeding it were both fresh, so it is the single signal that
    # says this node measured the machine it runs on. An unavailable one on a
    # machine that is plainly running is the failure this smoke test catches.
    observations = {row["name"]: row for row in data.get("observations") or []}
    scheduler = observations.get("scheduler")
    if scheduler is None:
        failures.append(f"no scheduler observation among {sorted(observations)}")
    elif scheduler.get("freshness") != "fresh":
        failures.append(f"scheduler observation {scheduler!r}")

    # Part A of docs/MULTI_NODE_PLAN.md asks for plausible CPU, memory, disk,
    # load and swap. Whether admission is *allowed* is deliberately not asserted:
    # a busy build host may legitimately be over its swap or load guard, and that
    # is a correct measurement rather than a broken probe. What must hold is that
    # every figure was measured at all.
    pressure = data.get("hostPressure") or {}
    for measurement in ("availableMemoryMiB", "freeDiskGiB"):
        value = pressure.get(measurement)
        if not isinstance(value, int) or value <= 0:
            failures.append(f"implausible {measurement}={value!r}")
    for measurement in ("swapUsedMiB", "swapOuts"):
        value = pressure.get(measurement)
        if not isinstance(value, int) or value < 0:
            failures.append(f"implausible {measurement}={value!r}")
    idle, load = pressure.get("cpuIdlePercent"), pressure.get("loadAverage")
    if not isinstance(idle, (int, float)) or not 0 <= idle <= 100:
        failures.append(f"implausible cpuIdlePercent={idle!r}")
    if not isinstance(load, (int, float)) or load < 0:
        failures.append(f"implausible loadAverage={load!r}")
    if not pressure.get("admissionReason"):
        failures.append(f"host admission was not decided: {pressure!r}")

    # Every profile is reported with a count, so an empty list is not the
    # expectation; a node with no executor holds no instance of any profile.
    held = [row for row in data.get("instances") or [] if row.get("count")]
    if held:
        failures.append(f"instances={held!r}, want none on a node with no executor")

    if failures:
        print("observe steady state not reached:", file=sys.stderr)
        for failure in failures:
            print("  " + failure, file=sys.stderr)
        print(json.dumps(data, indent=2, sort_keys=True), file=sys.stderr)
        return 1

    print("observe steady state reached")
    print(f'  version         {data.get("controllerVersion")}')
    print(f'  mode            {data.get("controllerMode")}')
    print(f'  scheduler       {scheduler.get("freshness")}')
    print(f'  admission       {pressure.get("admissionReason")} (allowed={pressure.get("admissionAllowed")})')
    print(f'  available RAM   {pressure["availableMemoryMiB"]} MiB')
    print(f'  free disk       {pressure["freeDiskGiB"]} GiB')
    print(f'  cpu idle        {pressure.get("cpuIdlePercent")}%')
    print(f'  load            {pressure.get("loadAverage")}')
    print(f'  swap used       {pressure.get("swapUsedMiB")} MiB')
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: observe-smoke-assert.py STATUS_JSON", file=sys.stderr)
        raise SystemExit(2)
    raise SystemExit(main(sys.argv[1]))
