# Four-Lane Maestro (Approach A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reland the 2 VM × 2 Simulator Maestro topology with the hardening that makes lane 2 robust: warm-clone lane-2 provisioning, guest image hygiene, per-lane JVM isolation, driver-failure recovery, a fleet host-quota watchdog, and a maximum-tuned experiment fleet config.

**Architecture:** Work spans two repos. The budgie app repo gets the relanded four-lane CI workflow plus new prewarm/hygiene/recycle scripts (shell, tested with standalone stub-based test scripts). The tart-runner-fleet repo gets a tart-adapter change that detects Apple's "VM quota exhausted" failure fast-fail (Go, TDD against the existing fakeRunner) and a validated experiment config. Base-image rollout and the A/B run are an operational checklist at the end, not code tasks.

**Tech Stack:** bash, GitHub Actions YAML, `xcrun simctl`, Maestro 2.6.1, Go 1.x (fleet daemon), Tart 2.32.1.

## Global Constraints

- Apple's kernel limits concurrent macOS guests to **2 per host**. Never plan more macOS VMs; 4 lanes = 2 VMs × 2 Simulators.
- VM envelope stays exactly: 4 vCPU / 9,216 MiB per macOS Maestro guest, `sync=none` root disk, headless flags, paravirtual GPU enabled.
- Device is always exact name `iPhone 17 Pro`, device type `com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro`, newest installed runtime.
- Maestro version `2.6.1`, Xcode `26.4.1`, app id `com.vitalyiegorov.budgie.e2e`.
- Budgie repo work happens in a NEW worktree `~/budgie/.worktrees/four-lane-maestro` on branch `claude/four-lane-maestro-reland` (do NOT touch the `~/budgie` main checkout, which sits on an unrelated branch).
- Fleet repo work happens in the existing worktree `/Users/vitalyiegorov/tart-runner-fleet/.claude/worktrees/macos-vm-ci-optimization-138794` (branch `agent/canonical-job-inventory`).
- Fleet repo CI is strict: `make ci` must pass (lint 0 issues, ~99% coverage, race suite). Never lower coverage.
- Do not modify the live production fleet config; the experiment config is a separate validated file rolled out via the existing config-only atomic rollout.
- Shell scripts follow the budgie repo pattern: `#!/bin/bash`, `set -euo pipefail`, standalone `test-*.sh` scripts that stub external binaries via a `PATH` shim.

---

### Task 1: Reland the codex four-lane experiment branch in budgie

The entire experiment (2×2 lane matrix, VirtioFS+SHA-256 handoff, headless AWT, lane barrier, deep-link priming rewrite, failure-only log export) lives on `origin/codex/macos-vm-performance-experiments` (16 commits, currently fast-forwardable from `origin/main`; its PR #599 was closed unmerged). Reland it on a fresh branch.

**Files:**
- No edits; branch + merge only.

**Interfaces:**
- Produces: worktree `~/budgie/.worktrees/four-lane-maestro` on branch `claude/four-lane-maestro-reland` containing all experiment files, for every later budgie task. Key files later tasks modify: `.github/workflows/pr.yml`, `tests/app-tests/scripts/run-ios-maestro-lanes.sh`, `tests/app-tests/scripts/run-maestro-suite.sh`.

- [ ] **Step 1: Create the worktree from origin/main**

```bash
cd ~/budgie
git fetch origin
git worktree add .worktrees/four-lane-maestro origin/main -b claude/four-lane-maestro-reland
```

Expected: worktree created; `git -C .worktrees/four-lane-maestro status` shows clean tree on `claude/four-lane-maestro-reland`.

- [ ] **Step 2: Verify the codex branch is still fast-forwardable, then merge**

```bash
cd ~/budgie/.worktrees/four-lane-maestro
git rev-list --left-right --count origin/main...origin/codex/macos-vm-performance-experiments
```

Expected: `0	16` (left number 0). If the left number is nonzero, main moved — use `git merge origin/codex/macos-vm-performance-experiments` (a real merge) instead of ff-only, resolve conflicts preserving the experiment's versions of `pr.yml` and `tests/app-tests/scripts/*`, and note the conflict files in the task report.

```bash
git merge --ff-only origin/codex/macos-vm-performance-experiments
```

Expected: `Fast-forward` output, HEAD at `a1a37de58`.

- [ ] **Step 3: Run the experiment's own shell test harnesses to prove the reland is green**

```bash
cd ~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts
bash test-run-ios-maestro-lanes.sh
bash test-prepare-ios-maestro-simulator.sh
bash test-app-data-container-cache.sh
bash test-run-maestro-suite.sh
```

Expected: each exits 0. If any fails, STOP and report — the reland base is broken and later tasks build on it.

- [ ] **Step 4: No commit needed** (merge commit / ff already recorded). Confirm log:

```bash
git log --oneline -3
```

Expected: top commit `a1a37de58 fix(e2e): recover iOS deep-link confirmation`.

---

### Task 2: Warm-clone lane-2 fallback, JVM heap cap, and provisioning evidence in pr.yml

Root cause of the failed A/B: when the image has only one iPhone 17 Pro, the workflow `simctl create`d lane 2 **cold**, paying the multi-minute first-boot indexing storm concurrently with lane 1's XCTest startup. `simctl clone` copies the settled device's data directory, so the clone starts with indexing already done (the CircleCI warm-snapshot pattern). Also cap each Maestro JVM heap so two JVMs cannot balloon inside a 9,216-MiB guest, and record which provisioning path ran so A/B runs have evidence.

**Files:**
- Modify: `~/budgie/.worktrees/four-lane-maestro/.github/workflows/pr.yml` — the `Start lane 1 iOS Simulator` step (the `simctl create` fallback block) and the `Run Maestro UI tests` step `env:` block.

**Interfaces:**
- Consumes: Task 1's worktree.
- Produces: env vars `SIMULATOR_UDID_LANE_1`, `SIMULATOR_UDID_LANE_2` (unchanged names, used by `run-ios-maestro-lanes.sh`); a `LANE_2_PROVISIONING` line in `$GITHUB_STEP_SUMMARY` with value `prewarmed` or `warm-clone`.

- [ ] **Step 1: Replace the `simctl create` fallback with `simctl clone`**

In `.github/workflows/pr.yml`, inside the `Start lane 1 iOS Simulator` step, find:

```bash
                  if [ -z "$SIMULATOR_UDID_LANE_2" ]; then
                    SIMULATOR_UDID_LANE_2="$(
                      xcrun simctl create \
                        "Budgie CI lane 2 run $GITHUB_RUN_ID attempt $GITHUB_RUN_ATTEMPT" \
                        com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro \
                        "$SIMULATOR_RUNTIME"
                    )"
                  fi
```

Replace with:

```bash
                  LANE_2_PROVISIONING=prewarmed
                  if [ -z "$SIMULATOR_UDID_LANE_2" ]; then
                    echo "::warning::Only one prewarmed iPhone 17 Pro exists; cloning lane 1's settled device for lane 2."
                    LANE_2_PROVISIONING=warm-clone
                    SIMULATOR_UDID_LANE_2="$(xcrun simctl clone "$SIMULATOR_UDID_LANE_1" "iPhone 17 Pro")"
                  fi
                  echo "LANE_2_PROVISIONING=$LANE_2_PROVISIONING" >> "$GITHUB_STEP_SUMMARY"
```

Rationale notes for the implementer: at this point in the job neither device is booted (`Shutdown stale simulators` ran `simctl shutdown all`, and lane 1 boots later in this same step), and `simctl clone` requires a shutdown source. Naming the clone `iPhone 17 Pro` keeps the selection filter (`name === "iPhone 17 Pro"`) consistent; the clone lives only inside the disposable VM, so no accumulation.

- [ ] **Step 2: Cap the Maestro JVM heap**

In the `Run Maestro UI tests` step `env:` block, change:

```yaml
                  JAVA_TOOL_OPTIONS: -Djava.awt.headless=true
```

to:

```yaml
                  JAVA_TOOL_OPTIONS: -Djava.awt.headless=true -Xmx1024m
```

Both lanes inherit this env; two capped JVMs bound worst-case Java heap at ~2 GiB/guest.

- [ ] **Step 3: Validate the workflow parses**

```bash
cd ~/budgie/.worktrees/four-lane-maestro
node -e "const fs=require('fs');const yaml=require('yaml');yaml.parse(fs.readFileSync('.github/workflows/pr.yml','utf8'));console.log('yaml ok')" \
  || npx --yes yaml-lint .github/workflows/pr.yml \
  || python3 -c "import yaml,sys;yaml.safe_load(open('.github/workflows/pr.yml'));print('yaml ok')"
```

Expected: `yaml ok` from whichever parser is available (try in order; the repo has `yaml` in node_modules via yarn — if none work, install nothing, use `python3`).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/pr.yml
git commit -m "ci: warm-clone lane 2 simulator and cap Maestro JVM heap"
```

---

### Task 3: Per-lane TMPDIR isolation in run-ios-maestro-lanes.sh

Maestro's own docs warn that concurrent Maestro JVMs on one machine collide on shared temp driver files (`java.io.IOException`). Give each lane a private `TMPDIR` under its artifact dir.

**Files:**
- Modify: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/run-ios-maestro-lanes.sh` — the `run_lane()` function.
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/test-lane-tmpdir-isolation.sh`

**Interfaces:**
- Consumes: `run_lane()` as merged in Task 1 (invokes `$MAESTRO_SUITE_RUNNER` with per-lane env).
- Produces: each suite-runner invocation sees `TMPDIR="$lane_artifact_dir/tmp"` (absolute, pre-created). No caller-visible signature changes.

- [ ] **Step 1: Write the failing test**

Create `tests/app-tests/scripts/test-lane-tmpdir-isolation.sh`:

```bash
#!/bin/bash
# Verifies run-ios-maestro-lanes.sh gives each lane a private TMPDIR.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# Minimal workspace layout the lanes script expects.
mkdir -p "$WORK_DIR/workspace/shards" "$WORK_DIR/workspace/flows" "$WORK_DIR/workspace/scripts"
printf 'a.flow.yaml\n' > "$WORK_DIR/workspace/shards/shard-1.txt"
printf 'b.flow.yaml\n' > "$WORK_DIR/workspace/shards/shard-2.txt"
touch "$WORK_DIR/workspace/flows/a.flow.yaml" "$WORK_DIR/workspace/flows/b.flow.yaml"

# Fake suite runner: records the TMPDIR each lane received, creates the
# first-flow marker so lane 2 unblocks.
cat > "$WORK_DIR/fake-suite.sh" <<'EOF'
#!/bin/bash
set -euo pipefail
printf '%s\n' "${TMPDIR-unset}" >> "${TMPDIR_LOG:?}"
if [ -n "${MAESTRO_FIRST_FLOW_PREPARED_PATH-}" ]; then
    mkdir -p "$MAESTRO_FIRST_FLOW_PREPARED_PATH"
fi
exit 0
EOF
chmod +x "$WORK_DIR/fake-suite.sh"

cp "$SCRIPT_DIR/run-ios-maestro-lanes.sh" "$WORK_DIR/workspace/scripts/run-ios-maestro-lanes.sh"
chmod +x "$WORK_DIR/workspace/scripts/run-ios-maestro-lanes.sh"

UDID_1='AAAAAAAA-1111-1111-1111-111111111111'
UDID_2='BBBBBBBB-2222-2222-2222-222222222222'

TMPDIR_LOG="$WORK_DIR/tmpdirs.log" \
MAESTRO_ARTIFACT_ROOT="$WORK_DIR/artifacts" \
MAESTRO_SUITE_RUNNER="$WORK_DIR/fake-suite.sh" \
E2E_RUN_TOKEN=test-token \
    "$WORK_DIR/workspace/scripts/run-ios-maestro-lanes.sh" \
    com.example.app "$UDID_1" 1 "$UDID_2" 2

sort "$WORK_DIR/tmpdirs.log" > "$WORK_DIR/tmpdirs.sorted"

expected_1="$WORK_DIR/artifacts/lane-1-shard-1/tmp"
expected_2="$WORK_DIR/artifacts/lane-2-shard-2/tmp"

if ! grep -qx "$expected_1" "$WORK_DIR/tmpdirs.sorted"; then
    echo "FAIL: lane 1 TMPDIR was not $expected_1" >&2
    cat "$WORK_DIR/tmpdirs.sorted" >&2
    exit 1
fi
if ! grep -qx "$expected_2" "$WORK_DIR/tmpdirs.sorted"; then
    echo "FAIL: lane 2 TMPDIR was not $expected_2" >&2
    cat "$WORK_DIR/tmpdirs.sorted" >&2
    exit 1
fi
if [ ! -d "$expected_1" ] || [ ! -d "$expected_2" ]; then
    echo "FAIL: per-lane tmp directories were not created" >&2
    exit 1
fi

echo "PASS: per-lane TMPDIR isolation"
```

```bash
chmod +x tests/app-tests/scripts/test-lane-tmpdir-isolation.sh
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd ~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts
bash test-lane-tmpdir-isolation.sh
```

Expected: `FAIL: lane 1 TMPDIR was not ...` (script currently passes the ambient TMPDIR through), exit 1.

- [ ] **Step 3: Implement — set per-lane TMPDIR in `run_lane()`**

In `run-ios-maestro-lanes.sh`, inside `run_lane()`, change the block that creates the artifact dir and invokes the runner. Current:

```bash
    mkdir -p "$lane_artifact_dir"
    printf 'Lane %s uses simulator %s for shard %s:\n%s\n' \
        "$lane_number" "$simulator_udid" "$shard_number" "${flows[*]}"

    SIMULATOR_UDID="$simulator_udid" \
        E2E_RUN_TOKEN="$RUN_TOKEN-lane-$lane_number" \
```

New:

```bash
    mkdir -p "$lane_artifact_dir/tmp"
    printf 'Lane %s uses simulator %s for shard %s:\n%s\n' \
        "$lane_number" "$simulator_udid" "$shard_number" "${flows[*]}"

    TMPDIR="$lane_artifact_dir/tmp" \
        SIMULATOR_UDID="$simulator_udid" \
        E2E_RUN_TOKEN="$RUN_TOKEN-lane-$lane_number" \
```

(The rest of the invocation — `MAESTRO_FIRST_FLOW_PREPARED_PATH`, analytics vars, `sh "$MAESTRO_SUITE_RUNNER" ...` — stays untouched.)

- [ ] **Step 4: Run the new test and the existing lane harness**

```bash
bash test-lane-tmpdir-isolation.sh
bash test-run-ios-maestro-lanes.sh
```

Expected: both PASS / exit 0.

- [ ] **Step 5: Commit**

```bash
git add tests/app-tests/scripts/run-ios-maestro-lanes.sh tests/app-tests/scripts/test-lane-tmpdir-isolation.sh
git commit -m "test(e2e): isolate Maestro lane temp directories"
```

---

### Task 4: Broaden driver-failure recovery and kill stale drivers before retry

Open Maestro-on-iOS-26 bugs (#3254 driver unresponsive after ~3 flows, #3318 driver port unreachable) mean the suite's existing recovery — which only matches `kAXErrorInvalidUIElement` and only reboots the Simulator — is too narrow. A rebooted Simulator with a **surviving stale `xcodebuild` driver process** is exactly the port-unreachable failure. Fix both: broaden the recoverable-failure pattern, and kill the lane's driver processes during reset.

**Files:**
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/driver-failure-pattern.sh` (sourceable constant)
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/recycle-ios-driver.sh`
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/test-driver-recovery.sh`
- Modify: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/run-maestro-suite.sh` — `is_ax_driver_failure()` and `reset_ios_simulator_after_ax_driver_failure()`.

**Interfaces:**
- Consumes: `run-maestro-suite.sh` retry structure from Task 1 (attempt-1 → `is_ax_driver_failure` → reset → attempt-2).
- Produces: `driver-failure-pattern.sh` exports `MAESTRO_RECOVERABLE_FAILURE_PATTERN` (an `grep -E` regex). `recycle-ios-driver.sh <udid>` kills host-side `xcodebuild` driver processes whose command line contains that UDID; exits 0 even when none found.

- [ ] **Step 1: Write the failing test**

Create `tests/app-tests/scripts/test-driver-recovery.sh`:

```bash
#!/bin/bash
# Verifies the recoverable-failure pattern and the stale-driver recycle helper.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# --- Part 1: pattern coverage -------------------------------------------
# shellcheck disable=SC1091
. "$SCRIPT_DIR/driver-failure-pattern.sh"

must_match=(
    'Error: kAXErrorInvalidUIElement while fetching hierarchy'
    'iOS driver not ready in time'
    'java.net.ConnectException: Connection refused'
    'java.net.SocketTimeoutException: timeout'
)
must_not_match=(
    'Assertion is false: id: TabBar.Home is visible'
    'Element not found: text matching Save'
)

for sample in "${must_match[@]}"; do
    if ! printf '%s\n' "$sample" | grep -Eiq "$MAESTRO_RECOVERABLE_FAILURE_PATTERN"; then
        echo "FAIL: pattern should match: $sample" >&2
        exit 1
    fi
done
for sample in "${must_not_match[@]}"; do
    if printf '%s\n' "$sample" | grep -Eiq "$MAESTRO_RECOVERABLE_FAILURE_PATTERN"; then
        echo "FAIL: pattern must NOT match plain test failures: $sample" >&2
        exit 1
    fi
done

# --- Part 2: recycle helper kills only this lane's xcodebuild ------------
STUB_BIN="$WORK_DIR/bin"
mkdir -p "$STUB_BIN"
UDID='AAAAAAAA-1111-1111-1111-111111111111'
OTHER_UDID='BBBBBBBB-2222-2222-2222-222222222222'

cat > "$STUB_BIN/pgrep" <<EOF
#!/bin/bash
# Stub: two xcodebuild drivers exist; only PID 101 belongs to our UDID.
if [[ "\$*" == *"$UDID"* ]]; then
    echo 101
    exit 0
fi
exit 1
EOF
cat > "$STUB_BIN/kill" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "${KILL_LOG:?}"
exit 0
EOF
chmod +x "$STUB_BIN/pgrep" "$STUB_BIN/kill"

KILL_LOG="$WORK_DIR/kill.log" PATH="$STUB_BIN:$PATH" \
    bash "$SCRIPT_DIR/recycle-ios-driver.sh" "$UDID"

if ! grep -q '101' "$WORK_DIR/kill.log"; then
    echo 'FAIL: recycle helper did not kill the matching driver PID' >&2
    exit 1
fi
if grep -q "$OTHER_UDID" "$WORK_DIR/kill.log"; then
    echo 'FAIL: recycle helper touched another lane' >&2
    exit 1
fi

# No-match case must succeed silently (helper is best-effort).
: > "$WORK_DIR/kill.log"
KILL_LOG="$WORK_DIR/kill.log" PATH="$STUB_BIN:$PATH" \
    bash "$SCRIPT_DIR/recycle-ios-driver.sh" "$OTHER_UDID"
if [ -s "$WORK_DIR/kill.log" ]; then
    echo 'FAIL: recycle helper killed something with no matching driver' >&2
    exit 1
fi

# --- Part 3: suite reset uses the helper and broadened pattern -----------
if ! grep -q 'driver-failure-pattern.sh' "$SCRIPT_DIR/run-maestro-suite.sh"; then
    echo 'FAIL: run-maestro-suite.sh does not source driver-failure-pattern.sh' >&2
    exit 1
fi
if ! grep -q 'recycle-ios-driver.sh' "$SCRIPT_DIR/run-maestro-suite.sh"; then
    echo 'FAIL: run-maestro-suite.sh reset path does not recycle the stale driver' >&2
    exit 1
fi

echo "PASS: driver recovery"
```

```bash
chmod +x tests/app-tests/scripts/test-driver-recovery.sh
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd ~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts
bash test-driver-recovery.sh
```

Expected: fails sourcing `driver-failure-pattern.sh` (file does not exist), exit non-zero.

- [ ] **Step 3: Implement the pattern file and recycle helper**

Create `tests/app-tests/scripts/driver-failure-pattern.sh`:

```bash
#!/bin/bash
# Recoverable Maestro iOS driver failures: accessibility snapshot corruption
# (kAXErrorInvalidUIElement), driver startup timeouts, and dead driver ports
# (Maestro issues #3254 / #3318 on iOS 26). Plain assertion/element failures
# must NOT match — those are real test results.
export MAESTRO_RECOVERABLE_FAILURE_PATTERN='kAXErrorInvalidUIElement|iOS driver not ready|Connection refused|SocketTimeoutException'
```

Create `tests/app-tests/scripts/recycle-ios-driver.sh`:

```bash
#!/bin/bash
# Kills host-side xcodebuild XCTest driver processes attached to one
# Simulator UDID. Best-effort: exits 0 when nothing matches. Never touches
# other lanes' drivers (match is scoped to the UDID in the command line).
set -euo pipefail

if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <simulator-udid>" >&2
    exit 1
fi

SIMULATOR_UDID="$1"
driver_pids="$(pgrep -f "xcodebuild.*$SIMULATOR_UDID" || true)"

if [ -z "$driver_pids" ]; then
    exit 0
fi

echo "Recycling stale iOS driver processes for $SIMULATOR_UDID: $driver_pids"
for pid in $driver_pids; do
    kill -TERM "$pid" 2>/dev/null || true
done
sleep 1
for pid in $driver_pids; do
    kill -KILL "$pid" 2>/dev/null || true
done
exit 0
```

```bash
chmod +x tests/app-tests/scripts/driver-failure-pattern.sh tests/app-tests/scripts/recycle-ios-driver.sh
```

- [ ] **Step 4: Wire both into run-maestro-suite.sh**

Near the top of `run-maestro-suite.sh` (after `SCRIPT_DIR`/`WORKSPACE_DIR` are computed — the script computes them in its first ~30 lines), add:

```bash
# shellcheck disable=SC1091
. "$SCRIPT_DIR/driver-failure-pattern.sh"
```

(If the script computes `SCRIPT_DIR` differently or not at all, derive it the same way `run-ios-maestro-lanes.sh` does: `SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)`.)

Change `is_ax_driver_failure()` from:

```bash
is_ax_driver_failure() {
    local output_path="$1"

    grep -Eiq 'kAXErrorInvalidUIElement' "$output_path"
}
```

to:

```bash
is_ax_driver_failure() {
    local output_path="$1"

    grep -Eiq "$MAESTRO_RECOVERABLE_FAILURE_PATTERN" "$output_path"
}
```

Change `reset_ios_simulator_after_ax_driver_failure()` from:

```bash
reset_ios_simulator_after_ax_driver_failure() {
    if [ -z "$DETECTED_SIMULATOR_UDID" ]; then
        return 0
    fi

    echo "Restarting iOS simulator after kAXErrorInvalidUIElement"
    xcrun simctl shutdown "$DETECTED_SIMULATOR_UDID" >/dev/null 2>&1 || true
    xcrun simctl boot "$DETECTED_SIMULATOR_UDID" >/dev/null 2>&1 || true
    xcrun simctl bootstatus "$DETECTED_SIMULATOR_UDID" -b >/dev/null
}
```

to:

```bash
reset_ios_simulator_after_ax_driver_failure() {
    if [ -z "$DETECTED_SIMULATOR_UDID" ]; then
        return 0
    fi

    echo "Restarting iOS simulator and driver after a recoverable driver failure"
    sh "$SCRIPT_DIR/recycle-ios-driver.sh" "$DETECTED_SIMULATOR_UDID" || true
    xcrun simctl shutdown "$DETECTED_SIMULATOR_UDID" >/dev/null 2>&1 || true
    xcrun simctl boot "$DETECTED_SIMULATOR_UDID" >/dev/null 2>&1 || true
    xcrun simctl bootstatus "$DETECTED_SIMULATOR_UDID" -b >/dev/null
}
```

- [ ] **Step 5: Run the new test plus the existing suite harness**

```bash
bash test-driver-recovery.sh
bash test-run-maestro-suite.sh
```

Expected: both PASS / exit 0. If `test-run-maestro-suite.sh` fails because its stubs don't provide `pgrep`, add a no-op `pgrep` stub to that harness's stub bin directory (returning exit 1 = "no matches") — that preserves its existing behavior.

- [ ] **Step 6: Commit**

```bash
git add tests/app-tests/scripts/driver-failure-pattern.sh tests/app-tests/scripts/recycle-ios-driver.sh \
        tests/app-tests/scripts/test-driver-recovery.sh tests/app-tests/scripts/run-maestro-suite.sh
git commit -m "fix(e2e): recover broader driver failures and kill stale drivers"
```

---

### Task 5: Two-Simulator prewarm script for the base image

The image builder must leave the base VM with **two settled iPhone 17 Pro devices** so no job ever creates or clones cold. This script is idempotent, runs inside the base VM during image maintenance, and is the one artifact the operational rollout (end of plan) installs.

**Files:**
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/prewarm-ios-simulators.sh`
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/test-prewarm-ios-simulators.sh`

**Interfaces:**
- Consumes: nothing from other tasks (standalone image-build tool).
- Produces: after a run, `xcrun simctl list` shows exactly ≥2 `iPhone 17 Pro` devices on the newest runtime, each booted once to settled state and shut down; UDIDs written to `$HOME/.budgie-ci/simulators.json` as `{"runtime": "...", "udids": ["...", "..."]}`. `SETTLE_SECONDS` env (default 180) controls the post-boot settle wait.

- [ ] **Step 1: Write the failing test**

Create `tests/app-tests/scripts/test-prewarm-ios-simulators.sh`:

```bash
#!/bin/bash
# Verifies prewarm-ios-simulators.sh creates the missing second device,
# boots+settles+shuts down both, and records UDIDs.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

STUB_BIN="$WORK_DIR/bin"
mkdir -p "$STUB_BIN"
EXISTING='AAAAAAAA-1111-1111-1111-111111111111'
CREATED='BBBBBBBB-2222-2222-2222-222222222222'
RUNTIME='com.apple.CoreSimulator.SimRuntime.iOS-26-5'

cat > "$STUB_BIN/xcrun" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${WORK_DIR}/xcrun.log"
case "\$*" in
    'simctl list devices available -j')
        if [ -f "$WORK_DIR/second-created" ]; then
            cat <<JSON
{"devices":{"$RUNTIME":[
  {"udid":"$EXISTING","name":"iPhone 17 Pro","isAvailable":true,"deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"},
  {"udid":"$CREATED","name":"iPhone 17 Pro","isAvailable":true,"deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"}
]}}
JSON
        else
            cat <<JSON
{"devices":{"$RUNTIME":[
  {"udid":"$EXISTING","name":"iPhone 17 Pro","isAvailable":true,"deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"}
]}}
JSON
        fi
        ;;
    'simctl clone '*)
        touch "$WORK_DIR/second-created"
        echo "$CREATED"
        ;;
    'simctl bootstatus '*) exit 0 ;;
    *) exit 0 ;;
esac
EOF
cat > "$STUB_BIN/defaults" <<EOF
#!/bin/bash
printf 'defaults %s\n' "\$*" >> "${WORK_DIR}/xcrun.log"
exit 0
EOF
chmod +x "$STUB_BIN/xcrun" "$STUB_BIN/defaults"

HOME="$WORK_DIR/home" SETTLE_SECONDS=0 PATH="$STUB_BIN:$PATH" \
    bash "$SCRIPT_DIR/prewarm-ios-simulators.sh"

LOG="$WORK_DIR/xcrun.log"

grep -q "simctl clone $EXISTING iPhone 17 Pro" "$LOG" \
    || { echo 'FAIL: second device was not warm-cloned from the first' >&2; exit 1; }
grep -q "simctl boot $EXISTING" "$LOG" \
    || { echo 'FAIL: existing device was not booted for settling' >&2; exit 1; }
grep -q "simctl boot $CREATED" "$LOG" \
    || { echo 'FAIL: created device was not booted for settling' >&2; exit 1; }
grep -q "simctl shutdown $EXISTING" "$LOG" \
    || { echo 'FAIL: existing device was not shut down after settling' >&2; exit 1; }
grep -q "simctl shutdown $CREATED" "$LOG" \
    || { echo 'FAIL: created device was not shut down after settling' >&2; exit 1; }
grep -q 'defaults write com.apple.iphonesimulator PasteboardAutomaticSync -bool false' "$LOG" \
    || { echo 'FAIL: pasteboard sync was not disabled' >&2; exit 1; }

RECORD="$WORK_DIR/home/.budgie-ci/simulators.json"
[ -f "$RECORD" ] || { echo 'FAIL: simulators.json was not written' >&2; exit 1; }
grep -q "$EXISTING" "$RECORD" && grep -q "$CREATED" "$RECORD" && grep -q "$RUNTIME" "$RECORD" \
    || { echo 'FAIL: simulators.json is missing UDIDs or runtime' >&2; cat "$RECORD" >&2; exit 1; }

echo "PASS: prewarm-ios-simulators"
```

```bash
chmod +x tests/app-tests/scripts/test-prewarm-ios-simulators.sh
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd ~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts
bash test-prewarm-ios-simulators.sh
```

Expected: fails because `prewarm-ios-simulators.sh` does not exist.

- [ ] **Step 3: Implement the prewarm script**

Create `tests/app-tests/scripts/prewarm-ios-simulators.sh`:

```bash
#!/bin/bash
# Image-build-time prewarm: leaves the guest with two settled iPhone 17 Pro
# Simulator devices on the newest runtime so CI jobs never pay a first-boot
# indexing storm. Idempotent. Run inside the macOS base VM as the CI user.
#
#   SETTLE_SECONDS  post-boot settle wait per device (default 180)
set -euo pipefail

DEVICE_TYPE='com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro'
DEVICE_NAME='iPhone 17 Pro'
SETTLE_SECONDS="${SETTLE_SECONDS:-180}"
RECORD_DIR="$HOME/.budgie-ci"

select_devices() {
    xcrun simctl list devices available -j | node -e '
        let input = "";
        process.stdin.on("data", chunk => input += chunk);
        process.stdin.on("end", () => {
            const deviceType = "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro";
            const inventory = JSON.parse(input).devices;
            const candidates = Object.entries(inventory)
                .map(([runtime, devices]) => [
                    runtime,
                    devices.filter(device =>
                        device.isAvailable !== false &&
                        device.name === "iPhone 17 Pro" &&
                        device.deviceTypeIdentifier === deviceType
                    ),
                ])
                .filter(([, devices]) => devices.length > 0)
                .sort(([left], [right]) =>
                    right.localeCompare(left, undefined, { numeric: true })
                );
            if (candidates.length === 0) return;
            const [runtime, devices] = candidates[0];
            process.stdout.write([runtime, ...devices.map(device => device.udid)].join("\n"));
        });
    '
}

settle_device() {
    local udid="$1"
    echo "Prewarming $udid (settle ${SETTLE_SECONDS}s)"
    xcrun simctl boot "$udid" 2>/dev/null || true
    xcrun simctl bootstatus "$udid" -b
    sleep "$SETTLE_SECONDS"
    xcrun simctl shutdown "$udid"
}

# Cross-Simulator pasteboard sync causes deadlocks between concurrent lanes.
defaults write com.apple.iphonesimulator PasteboardAutomaticSync -bool false

selection="$(select_devices)"
RUNTIME="$(printf '%s\n' "$selection" | sed -n '1p')"
UDID_1="$(printf '%s\n' "$selection" | sed -n '2p')"
UDID_2="$(printf '%s\n' "$selection" | sed -n '3p')"

if [ -z "$RUNTIME" ] || [ -z "$UDID_1" ]; then
    echo "No available $DEVICE_NAME device or runtime found; create one in Xcode first." >&2
    exit 1
fi

if [ -z "$UDID_2" ]; then
    echo "Creating second $DEVICE_NAME by warm-cloning $UDID_1"
    UDID_2="$(xcrun simctl clone "$UDID_1" "$DEVICE_NAME")"
fi

if [ -z "$UDID_2" ] || [ "$UDID_1" = "$UDID_2" ]; then
    echo "Failed to obtain two distinct $DEVICE_NAME devices." >&2
    exit 1
fi

settle_device "$UDID_1"
settle_device "$UDID_2"

mkdir -p "$RECORD_DIR"
printf '{"runtime":"%s","udids":["%s","%s"]}\n' "$RUNTIME" "$UDID_1" "$UDID_2" \
    > "$RECORD_DIR/simulators.json"

echo "Prewarmed devices on $RUNTIME:"
echo "  lane 1: $UDID_1"
echo "  lane 2: $UDID_2"
```

```bash
chmod +x tests/app-tests/scripts/prewarm-ios-simulators.sh
```

Implementation note: cloning **before** the first settle is fine — both devices then settle independently; on re-runs (2 devices already present) the clone branch is skipped and both settle again, which is harmless and keeps the script idempotent.

- [ ] **Step 4: Run the test**

```bash
bash test-prewarm-ios-simulators.sh
```

Expected: `PASS: prewarm-ios-simulators`.

- [ ] **Step 5: Commit**

```bash
git add tests/app-tests/scripts/prewarm-ios-simulators.sh tests/app-tests/scripts/test-prewarm-ios-simulators.sh
git commit -m "feat(e2e): add two-simulator image prewarm script"
```

---

### Task 6: Guest hygiene script for the base image

CircleCI's documented mitigation for simulator CPU storms: permanently unload `diagnosticd` (respawns if merely killed) and raise process/file-descriptor limits (each simulator adds ~150 processes / ~3,000 fds). Applied once at image-build time.

**Files:**
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/prepare-macos-ci-guest.sh`
- Create: `~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts/test-prepare-macos-ci-guest.sh`

**Interfaces:**
- Consumes: nothing (standalone image-build tool, run with sudo inside the base VM).
- Produces: `diagnosticd` unloaded (best-effort, non-fatal under SIP), `/Library/LaunchDaemons/ci.limit.maxfiles.plist` and `ci.limit.maxproc.plist` installed and loaded, Spotlight indexing off. Unified logging and guest swap are deliberately NOT touched.

- [ ] **Step 1: Write the failing test**

Create `tests/app-tests/scripts/test-prepare-macos-ci-guest.sh`:

```bash
#!/bin/bash
# Verifies prepare-macos-ci-guest.sh applies image hygiene idempotently.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

STUB_BIN="$WORK_DIR/bin"
mkdir -p "$STUB_BIN" "$WORK_DIR/launchdaemons"

for tool in launchctl mdutil; do
    cat > "$STUB_BIN/$tool" <<EOF
#!/bin/bash
printf '$tool %s\n' "\$*" >> "${WORK_DIR}/calls.log"
exit 0
EOF
    chmod +x "$STUB_BIN/$tool"
done

LAUNCH_DAEMONS_DIR="$WORK_DIR/launchdaemons" PATH="$STUB_BIN:$PATH" \
    bash "$SCRIPT_DIR/prepare-macos-ci-guest.sh"

LOG="$WORK_DIR/calls.log"

grep -q 'launchctl bootout system/com.apple.diagnosticd' "$LOG" \
    || { echo 'FAIL: diagnosticd was not booted out' >&2; exit 1; }
grep -q 'mdutil -a -i off' "$LOG" \
    || { echo 'FAIL: Spotlight indexing was not disabled' >&2; exit 1; }

for plist in ci.limit.maxfiles.plist ci.limit.maxproc.plist; do
    [ -f "$WORK_DIR/launchdaemons/$plist" ] \
        || { echo "FAIL: $plist was not installed" >&2; exit 1; }
    grep -q 'launchctl load -w '"$WORK_DIR/launchdaemons/$plist" "$LOG" \
        || { echo "FAIL: $plist was not loaded" >&2; exit 1; }
done

grep -q '300000' "$WORK_DIR/launchdaemons/ci.limit.maxfiles.plist" \
    || { echo 'FAIL: maxfiles limit value missing' >&2; exit 1; }
grep -q '4000' "$WORK_DIR/launchdaemons/ci.limit.maxproc.plist" \
    || { echo 'FAIL: maxproc limit value missing' >&2; exit 1; }

# Idempotency: second run must not fail.
LAUNCH_DAEMONS_DIR="$WORK_DIR/launchdaemons" PATH="$STUB_BIN:$PATH" \
    bash "$SCRIPT_DIR/prepare-macos-ci-guest.sh"

echo "PASS: prepare-macos-ci-guest"
```

```bash
chmod +x tests/app-tests/scripts/test-prepare-macos-ci-guest.sh
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd ~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts
bash test-prepare-macos-ci-guest.sh
```

Expected: fails because `prepare-macos-ci-guest.sh` does not exist.

- [ ] **Step 3: Implement the hygiene script**

Create `tests/app-tests/scripts/prepare-macos-ci-guest.sh`:

```bash
#!/bin/bash
# Image-build-time guest hygiene for macOS CI base VMs. Run once with sudo
# inside the base VM. Idempotent. Deliberately does NOT touch unified
# logging or guest swap (see docs: disabling swap converts pressure into
# process kills).
#
#   LAUNCH_DAEMONS_DIR  override for tests (default /Library/LaunchDaemons)
set -euo pipefail

LAUNCH_DAEMONS_DIR="${LAUNCH_DAEMONS_DIR:-/Library/LaunchDaemons}"

# 1. Permanently unload diagnosticd: a fresh simulator boot floods it, and
#    killing the process is useless because launchd respawns it. Best-effort
#    because SIP may deny this on some configurations.
if launchctl bootout system/com.apple.diagnosticd 2>/dev/null; then
    echo "diagnosticd booted out"
else
    echo "warning: could not boot out diagnosticd (SIP may block this); continuing" >&2
fi

# 2. Raise process/file-descriptor limits: each booted simulator adds ~150
#    processes and ~3000 file descriptors; two lanes plus Maestro JVMs
#    exceed the default 2666-process ceiling.
write_limit_plist() {
    local plist_path="$1" label="$2" limit_flag="$3" soft="$4" hard="$5"

    cat > "$plist_path" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$label</string>
    <key>ProgramArguments</key>
    <array>
        <string>launchctl</string>
        <string>limit</string>
        <string>$limit_flag</string>
        <string>$soft</string>
        <string>$hard</string>
    </array>
    <key>RunAtLoad</key><true/>
</dict>
</plist>
PLIST
    launchctl load -w "$plist_path" 2>/dev/null || true
}

write_limit_plist "$LAUNCH_DAEMONS_DIR/ci.limit.maxfiles.plist" ci.limit.maxfiles maxfiles 100000 300000
write_limit_plist "$LAUNCH_DAEMONS_DIR/ci.limit.maxproc.plist" ci.limit.maxproc maxproc 3500 4000

# 3. Spotlight off (idempotent; already policy for sealed job images).
mdutil -a -i off >/dev/null 2>&1 || mdutil -a -i off || true

echo "Guest hygiene applied."
```

```bash
chmod +x tests/app-tests/scripts/prepare-macos-ci-guest.sh
```

- [ ] **Step 4: Run the test**

```bash
bash test-prepare-macos-ci-guest.sh
```

Expected: `PASS: prepare-macos-ci-guest`.

- [ ] **Step 5: Commit**

```bash
git add tests/app-tests/scripts/prepare-macos-ci-guest.sh tests/app-tests/scripts/test-prepare-macos-ci-guest.sh
git commit -m "feat(e2e): add macOS CI guest hygiene script"
```

---

### Task 7: Push the budgie branch and open a draft PR

**Files:** none (git/gh only).

**Interfaces:**
- Consumes: all budgie commits from Tasks 1–6.
- Produces: pushed branch `claude/four-lane-maestro-reland`, draft PR against `main` in the budgie repo.

- [ ] **Step 1: Run every shell harness one final time**

```bash
cd ~/budgie/.worktrees/four-lane-maestro/tests/app-tests/scripts
for t in test-*.sh; do echo "== $t"; bash "$t" || exit 1; done
```

Expected: all PASS.

- [ ] **Step 2: Push and open a draft PR**

```bash
cd ~/budgie/.worktrees/four-lane-maestro
git push -u origin claude/four-lane-maestro-reland
gh pr create --draft --title "ci: reland hardened four-lane Maestro topology" --body "$(cat <<'EOF'
Relands the four-lane experiment (closed PR #599, branch codex/macos-vm-performance-experiments, head a1a37de58) with the hardening that addresses its measured failure causes:

- Lane 2 falls back to `simctl clone` of the settled lane-1 device (never a cold `simctl create`), and the provisioning path is recorded in the step summary.
- Per-lane `TMPDIR` isolation and a 1 GiB Maestro JVM heap cap.
- Driver recovery broadened beyond kAXErrorInvalidUIElement to driver-not-ready / dead-port failures, and the lane's stale `xcodebuild` driver is killed before retry.
- New image-build scripts: `prewarm-ios-simulators.sh` (two settled iPhone 17 Pro devices, pasteboard sync off) and `prepare-macos-ci-guest.sh` (diagnosticd bootout, maxproc/maxfiles, Spotlight off).

Do not merge before the two-device prewarmed base image is rolled out; the A/B protocol and promotion criteria are in tart-runner-fleet docs/superpowers/specs/2026-07-19-four-lane-maestro-topology-design.md.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR URL printed.

---

### Task 8: Fleet — detect macOS VM quota exhaustion fast and fail closed

On macOS 26, Tart sometimes cannot start a macOS VM because the kernel's 2-VM quota was not released by a previous clean stop (cirruslabs/tart #1217, #967); only a host reboot clears it. Today `tart run` is started detached with output discarded, so this failure surfaces only as a generic run timeout that the fleet retries uselessly. Capture the detached process's output, fail fast with a distinct error kind, and document the operator response.

**Files:**
- Modify: `internal/adapters/tart/adapter.go` (Runner interface, ExecRunner.Start, classify constants, Adapter run poll loop)
- Modify: `internal/adapters/tart/adapter_test.go` (fakeRunner.Start signature + new tests)
- Modify: `docs/OPERATIONS.md` (runbook entry)
- Check call sites: `grep -rn "\.Start(" internal cmd | grep -v _test` — update any other Runner implementations/uses the grep reveals.

**Interfaces:**
- Consumes: existing `Runner`, `fakeRunner`, `Adapter.Run` poll loop, `Error`/`ErrorKind`.
- Produces:
  - `type StartedCommand interface { Exited() bool; Output() []byte }`
  - `Runner.Start(context.Context, ...string) (StartedCommand, error)` (signature change)
  - `const ErrorHostQuota ErrorKind = "host_quota"`
  - `Adapter.Run` returns `*Error{Op: "run", Kind: ErrorHostQuota}` when the detached `tart run` exits with output containing `exceeds the system limit`, and `*Error{Op: "run", Kind: ErrorCommand}` when it exits early with any other output, instead of polling to timeout.

- [x] **Step 1: Write the failing tests**

Add to `internal/adapters/tart/adapter_test.go` (adjust `testAdapter` usage to match the file's existing helpers):

```go
func TestStartFailsClosedWhenMacOSQuotaExhausted(t *testing.T) {
	now := time.Now()
	adapter, runner, registry, ownership := testAdapter(now)
	runner.vms["gha-macos-a"] = VM{Name: "gha-macos-a", Source: "local"}
	if err := registry.PutOwnership(context.Background(), "gha-macos-a", ownership); err != nil {
		t.Fatal(err)
	}
	runner.startNoEffect = true
	runner.startExited = true
	runner.startOutput = []byte("Error: The number of VMs exceeds the system limit")

	err := adapter.Start(context.Background(), "gha-macos-a", ownership)

	var tartErr *Error
	if !errors.As(err, &tartErr) {
		t.Fatalf("expected *Error, got %v", err)
	}
	if tartErr.Kind != ErrorHostQuota {
		t.Fatalf("expected ErrorHostQuota, got %s", tartErr.Kind)
	}
}

func TestStartFailsFastWhenProcessExitsEarly(t *testing.T) {
	now := time.Now()
	adapter, runner, registry, ownership := testAdapter(now)
	runner.vms["gha-macos-a"] = VM{Name: "gha-macos-a", Source: "local"}
	if err := registry.PutOwnership(context.Background(), "gha-macos-a", ownership); err != nil {
		t.Fatal(err)
	}
	runner.startNoEffect = true
	runner.startExited = true
	runner.startOutput = []byte("some other startup failure")

	err := adapter.Start(context.Background(), "gha-macos-a", ownership)

	var tartErr *Error
	if !errors.As(err, &tartErr) {
		t.Fatalf("expected *Error, got %v", err)
	}
	if tartErr.Kind != ErrorCommand {
		t.Fatalf("expected ErrorCommand, got %s", tartErr.Kind)
	}
}
```

Naming note (verified against the file): the adapter's VM-run method is `Adapter.Start(ctx, name, ownership)` at `adapter.go:281` (the method building `args := []string{"run", ...}`), and `testAdapter(now)` at `adapter_test.go:191` returns `(*Adapter, *fakeRunner, *memoryOwnership, operations.Ownership)`. The new `Runner.Start` interface method and the existing `Adapter.Start` VM method share a name but live on different types — no conflict.

- [x] **Step 2: Make it compile-fail/red**

```bash
cd /Users/vitalyiegorov/tart-runner-fleet/.claude/worktrees/macos-vm-ci-optimization-138794
go test ./internal/adapters/tart/ -run 'TestRunFails' -v
```

Expected: compile error (`runner.startExited` undefined / `ErrorHostQuota` undefined). That is the red state.

- [x] **Step 3: Implement**

In `internal/adapters/tart/adapter.go`:

Add the error kind:

```go
const (
	ErrorTimeout      ErrorKind = "timeout"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorAlreadyExist ErrorKind = "already_exists"
	ErrorPermission   ErrorKind = "permission"
	ErrorCommand      ErrorKind = "command"
	ErrorUncertain    ErrorKind = "uncertain"
	ErrorHostQuota    ErrorKind = "host_quota"
)
```

Add the handle type and change the interface:

```go
type StartedCommand interface {
	Exited() bool
	Output() []byte
}

type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
	Start(context.Context, ...string) (StartedCommand, error)
}
```

Implement the real handle in ExecRunner (bounded output, race-safe):

```go
type startedCommand struct {
	mu     sync.Mutex
	output []byte
	exited bool
}

func (s *startedCommand) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

func (s *startedCommand) Output() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.output...)
}

// boundedWriter keeps the first N bytes of combined output; enough to
// classify startup failures without unbounded growth on long-lived runs.
type boundedWriter struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if remaining := w.limit - len(w.data); remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		w.data = append(w.data, p[:remaining]...)
	}
	return len(p), nil
}

func (r ExecRunner) Start(ctx context.Context, args ...string) (StartedCommand, error) {
	binary := r.Binary
	if binary == "" {
		binary = "tart"
	}
	// #nosec G204 -- Binary is a trusted adapter dependency; arguments never pass through a shell.
	command := exec.CommandContext(context.WithoutCancel(ctx), binary, args...)
	configureDetached(command)
	writer := &boundedWriter{limit: 8192}
	command.Stdout = writer
	command.Stderr = writer
	started := &startedCommand{}
	if err := command.Start(); err != nil {
		return nil, classify(args, nil, err, ctx.Err())
	}
	go func() {
		_ = command.Wait()
		writer.mu.Lock()
		output := append([]byte(nil), writer.data...)
		writer.mu.Unlock()
		started.mu.Lock()
		started.output = output
		started.exited = true
		started.mu.Unlock()
	}()
	return started, nil
}
```

In the Adapter's run method, capture the handle and check it inside the poll loop. Change:

```go
	if err := a.runner().Start(ctx, args...); err != nil {
		return err
	}
	deadline := a.poller().Now().Add(a.startTimeout())
	for a.poller().Now().Before(deadline) {
		vm, err = a.ownedVM(ctx, name, ownership)
		if err == nil && vm.Running {
			return nil
		}
		if err != nil {
			return err
		}
		if err := a.poller().Wait(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
	return &Error{Op: "run", Kind: ErrorTimeout, ExitCode: -1, Err: context.DeadlineExceeded}
```

to:

```go
	started, err := a.runner().Start(ctx, args...)
	if err != nil {
		return err
	}
	deadline := a.poller().Now().Add(a.startTimeout())
	for a.poller().Now().Before(deadline) {
		vm, err = a.ownedVM(ctx, name, ownership)
		if err == nil && vm.Running {
			return nil
		}
		if err != nil {
			return err
		}
		if started != nil && started.Exited() {
			output := started.Output()
			if strings.Contains(strings.ToLower(string(output)), "exceeds the system limit") {
				return &Error{Op: "run", Kind: ErrorHostQuota, ExitCode: -1, Stderr: string(output), Err: errors.New("macos vm quota exhausted; host reboot required (tart#1217)")}
			}
			return &Error{Op: "run", Kind: ErrorCommand, ExitCode: -1, Stderr: string(output), Err: errors.New("tart run exited before the vm was observed running")}
		}
		if err := a.poller().Wait(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
	return &Error{Op: "run", Kind: ErrorTimeout, ExitCode: -1, Err: context.DeadlineExceeded}
```

Update `fakeRunner` in `adapter_test.go`: add fields and change Start:

```go
	startExited bool
	startOutput []byte
```

```go
type fakeStarted struct {
	exited bool
	output []byte
}

func (f fakeStarted) Exited() bool   { return f.exited }
func (f fakeStarted) Output() []byte { return f.output }

func (f *fakeRunner) Start(_ context.Context, args ...string) (StartedCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, append([]string(nil), args...))
	if f.startError != nil {
		return nil, f.startError
	}
	if f.startHook != nil {
		f.startHook()
	}
	if f.startNoEffect {
		return fakeStarted{exited: f.startExited, output: f.startOutput}, nil
	}
	vm := f.vms[args[1]]
	vm.Running = true
	f.vms[args[1]] = vm
	return fakeStarted{exited: f.startExited, output: f.startOutput}, nil
}
```

Call-site survey (verified 2026-07-19): the only production use of `Runner.Start` is `adapter.go:301`; `internal/guestbootstrap`'s `Launcher.Start` is an unrelated interface. Fakes implementing `Runner` may also exist in `tests/integration`, `tests/chaos`, or `tests/replay` — run `grep -rn "Runner" tests/ --include='*.go' -l` and apply the same mechanical change (return `fakeStarted{}, nil` where they returned `nil`).

- [x] **Step 4: Run the package tests, then full CI**

```bash
go test ./internal/adapters/tart/ -v
make ci
```

Expected: new tests pass, all suites green, coverage not reduced (the new branches are covered by the two new tests; if the coverage gate still complains, cover `boundedWriter` overflow with a small direct test:

```go
func TestBoundedWriterTruncates(t *testing.T) {
	w := &boundedWriter{limit: 4}
	if n, err := w.Write([]byte("123456")); n != 6 || err != nil {
		t.Fatalf("write reported n=%d err=%v", n, err)
	}
	if string(w.data) != "1234" {
		t.Fatalf("expected truncation to 4 bytes, got %q", w.data)
	}
}
```

).

- [x] **Step 5: Add the operator runbook entry**

In `docs/OPERATIONS.md`, add under the macOS/incident section (near the existing macOS base-image contract section):

```markdown
### macOS VM quota exhaustion (`host_quota`)

Apple's kernel caps concurrent macOS guests at 2 per host and, on macOS 26,
sometimes fails to release a slot after a clean `tart stop`
(cirruslabs/tart#1217, #967). The tart adapter reports this as error kind
`host_quota` ("exceeds the system limit") and fails closed instead of
retrying. Operator response: verify no macOS VM is actually running
(`tart list`), then reboot the host — a reboot is the only known way to
clear a leaked quota slot. Do not raise `maxActive` or retry admission
until the reboot completes.
```

- [x] **Step 6: Commit**

```bash
git add internal/adapters/tart/adapter.go internal/adapters/tart/adapter_test.go docs/OPERATIONS.md
git commit -m "fix(tart): fail closed on macOS VM quota exhaustion

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: Fleet — validated four-lane experiment config

Capture the exact experiment topology as a repo-validated config so the config-only atomic rollout (#60/#61 machinery) can apply it, and the A/B is reproducible. This also carries the two "squeeze maximum" resource decisions: 9,216 MiB Maestro guests and the 3,072-MiB swap guard (stale encrypted-swap residency of ~2.43 GiB otherwise blocks an idle host; the guard must still fail closed above that).

**Files:**
- Create: `config/experiments/four-lane-maestro.json`
- Create: `internal/config/experiments_test.go`

**Interfaces:**
- Consumes: `config.Decode(io.Reader) (Config, error)` (`internal/config/config.go:201`) — the same mechanism `internal/config/self_hosting_test.go:13-19` uses to load `config/fleet.example.json`.
- Produces: a config file that passes the same validation as production config, with `macosBurst.maestro` = 4 vCPU / 9,216 MiB / maxActive 2, `admissionPolicy: "macos-exclusive"`, `rootDiskOptions: "sync=none"`, `maxSwapUsedMb: 3072`.

- [x] **Step 1: Write the failing test**

Create `internal/config/experiments_test.go` (in-package test, like `self_hosting_test.go`):

```go
package config

import (
	"os"
	"testing"
)

func TestFourLaneMaestroExperimentConfigIsValid(t *testing.T) {
	file, err := os.Open("../../config/experiments/four-lane-maestro.json") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	cfg, err := Decode(file)
	if err != nil {
		t.Fatalf("experiment config must decode and validate: %v", err)
	}

	if cfg.MacOS.Maestro.MemoryMiB != 9216 {
		t.Fatalf("maestro guests must be 9216 MiB, got %d", cfg.MacOS.Maestro.MemoryMiB)
	}
	if cfg.MacOS.Maestro.CPU != 4 {
		t.Fatalf("maestro guests must be 4 vCPU, got %d", cfg.MacOS.Maestro.CPU)
	}
	if cfg.MacOS.Maestro.MaxActive != 2 {
		t.Fatalf("exactly 2 macOS Maestro VMs (Apple kernel quota), got %d", cfg.MacOS.Maestro.MaxActive)
	}
	if cfg.MacOS.AdmissionPolicy != MacOSAdmissionExclusive {
		t.Fatalf("experiment runs macos-exclusive admission, got %q", cfg.MacOS.AdmissionPolicy)
	}
	if cfg.MacOS.RootDiskOptions != "sync=none" {
		t.Fatalf("disposable clones run sync=none, got %q", cfg.MacOS.RootDiskOptions)
	}
	if cfg.Guards.MaxSwapUsedMiB != 3072 {
		t.Fatalf("experiment swap guard is 3072 MiB, got %d", cfg.Guards.MaxSwapUsedMiB)
	}
}
```

(All selectors verified against `internal/config/config.go`: `MacOS` struct at :73 has `AdmissionPolicy`, `Maestro Profile`, `RootDiskOptions`; `Profile` has `CPU`, `MemoryMiB`, `MaxActive`; `Guards` struct at :90 has `MaxSwapUsedMiB`; `Decode` at :201.)

- [x] **Step 2: Run to verify it fails**

```bash
go test ./internal/config/ -run TestFourLaneMaestroExperimentConfig -v
```

Expected: FAIL — file does not exist.

- [x] **Step 3: Create the experiment config**

Create `config/experiments/four-lane-maestro.json` (copy of `config/fleet.example.json` with the six experiment deltas; keep every other field identical to the example so validation semantics don't drift):

```json
{
  "baseVm": "linux-runner-base",
  "vmPrefix": "gha-linux",
  "pollSeconds": 20,
  "maxLinuxWhenMacosIdle": 4,
  "maxLinuxCpu": 8,
  "maxLinuxMemoryMb": 16384,
  "linuxReservationAgeSeconds": 300,
  "minFreeDiskGb": 60,
  "minAvailableMemoryMb": 1024,
  "maxSwapUsedMb": 3072,
  "maxLoadAverage": 9,
  "minCpuIdlePercent": 5,
  "githubTimeoutSeconds": 15,
  "tartControlTimeoutSeconds": 45,
  "bootTimeoutSeconds": 180,
  "linuxProfiles": [
    { "id": "small", "label": "linux-small", "cpu": 1, "memoryMb": 2048, "diskGb": 50 },
    { "id": "medium", "label": "linux-medium", "cpu": 2, "memoryMb": 4096, "diskGb": 50 },
    { "id": "large", "label": "linux-large", "cpu": 4, "memoryMb": 8192, "diskGb": 50 }
  ],
  "macosBurst": {
    "enabled": true,
    "admissionPolicy": "macos-exclusive",
    "baseVm": "macos-tartelet-base",
    "vmPrefix": "gha-macos",
    "rootDiskOptions": "sync=none",
    "sharedDirectoryPath": "/Users/runner/ci-shared",
    "builder": { "id": "builder", "label": "macos-builder", "cpu": 8, "memoryMb": 12288, "maxActive": 1 },
    "maestro": { "id": "maestro", "label": "macos-maestro", "cpu": 4, "memoryMb": 9216, "maxActive": 2 }
  },
  "targets": [
    { "type": "repo", "slug": "vitalyiegorov/tart-runner-fleet", "maxActive": 3, "schedulingClass": "control-plane" },
    { "type": "repo", "slug": "vitalyiegorov/knee-doctor", "maxActive": 4 },
    { "type": "repo", "slug": "budgie-at/budgie", "maxActive": 4, "defaultLinuxProfile": "large", "runnerLabels": ["self-hosted", "linux-ci", "linux-burst"] },
    { "type": "repo", "slug": "vitalyiegorov/hotel-provence", "maxActive": 4 },
    { "type": "repo", "slug": "vitalyiegorov/suuudokuuu", "maxActive": 4 }
  ]
}
```

Sizing rationale (for reviewers): 2 × 9,216 MiB guests + 12 GiB builder never run together (`macos-exclusive` + existing envelope checks enforce it); 9,216 is the 24-GiB-host ceiling that still preserves ≥15% host free memory. Two guests at 4 vCPU each leave 2 host cores for the fleet daemon and VirtioFS.

- [x] **Step 4: Run the test, then full CI**

```bash
go test ./internal/config/ -run TestFourLaneMaestroExperimentConfig -v
make ci
```

Expected: PASS; full CI green. If the repo's validator rejects paths outside known config locations or CI has a docs/JSON contract check, follow the validator's error message — the config content above is schema-correct per `internal/config/config.go`.

- [x] **Step 5: Commit (include the spec and this plan)**

```bash
git add config/experiments/four-lane-maestro.json internal/config/experiments_test.go \
        docs/superpowers/specs/2026-07-19-four-lane-maestro-topology-design.md \
        docs/superpowers/plans/2026-07-19-four-lane-maestro-approach-a.md
git commit -m "feat(config): add validated four-lane Maestro experiment config

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Operational rollout & A/B (checklist — human/operator steps, not code tasks)

Run only after Tasks 1–9 are merged/approved. Order matters.

1. **Base image maintenance** (on the CI host, inside the macOS base VM, via the existing base-updater maintenance flow): run `prepare-macos-ci-guest.sh` with sudo, then `prewarm-ios-simulators.sh` as the CI user (default `SETTLE_SECONDS=180`), then one clean guest reboot, then boot both recorded devices once more and shut down. Verify `~/.budgie-ci/simulators.json` lists two distinct UDIDs. Seal the image.
2. **Fleet config rollout**: apply `config/experiments/four-lane-maestro.json` via the config-only atomic rollout (`fleetctl update apply-latest --config ...` per docs/OPERATIONS.md); confirm authority healthy, admission guard reports capacity.
3. **A/B run**: trigger the budgie draft PR workflow twice. Compare against run `29654440855` (commit `a1a37de58`): expect `LANE_2_PROVISIONING=prewarmed` in all four lanes, no driver-startup timeouts, first-flow AX waits back to lane-1-like durations.
4. **Promotion criteria** (all required, over repeated runs): lower median workflow wall time vs the 2-lane control, acceptable p95 shard time (measure on later runs — M4 thermal throttling sets in after 10–15 min), zero infrastructure failures, ≥15% host memory free, no material host-swap growth, no `host_quota` errors.
5. **Only after promotion**: consider the two follow-up A/Bs from the design doc — suspend/resume warm templates (two independently identified templates) and disk caching. Never both at once.

## Explicitly out of scope

- Anything raising macOS VM count above 2 (kernel-impossible).
- Bare-metal lanes (Approach B — only if prewarmed lanes still show AX timeouts).
- Hypervisor migration (assessed and rejected in the spec).
- Disabling guest swap or unified logging (rejected in the spec).
