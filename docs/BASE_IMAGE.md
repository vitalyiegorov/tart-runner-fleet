# macOS base image: provenance and reproduction

[ADR 0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md) makes each node
responsible for its own guests, and [`MULTI_NODE_PLAN.md`](MULTI_NODE_PLAN.md)
assigns node C — a remote Mac Studio — the `maestro` scale set of one scope.
Neither record says where a macOS base image comes from. This document closes
that gap: it reconstructs how node A's image was actually built, and gives a
node C operator a recipe that produces an equivalent Maestro-only image locally
instead of copying node A's 91 GB disk across a residential uplink.

The fleet never builds images. It clones a stopped base and sizes the clone
([ADR 0006](adr/0006-per-profile-disk-floors.md)). Image construction is an
operator activity, performed once per node and repeated only for maintenance.

## Contents

- [Provenance of node A's image](#provenance-of-node-as-image)
- [What a Maestro job actually needs](#what-a-maestro-job-actually-needs)
- [What node C does not need](#what-node-c-does-not-need)
- [Build the node C image](#build-the-node-c-image)
- [Seal and verify](#seal-and-verify)
- [Wire it to the node C configuration](#wire-it-to-the-node-c-configuration)
- [Size expectations](#size-expectations)
- [Known unknowns](#known-unknowns)

## Provenance of node A's image

Node A's `macos-tartelet-base-go` was never produced by a checked-in script.
It is the end of a hand-applied clone chain that begins at one public Cirrus
Labs image. The chain is recoverable because every layer was snapshotted under
a dated name before it was replaced, and those snapshots are still on disk.

| Step | Date | Result | Used GB |
| --- | --- | --- | ---: |
| Pull `ghcr.io/cirruslabs/macos-sequoia-xcode` | 2026-06-10 | OCI image in `~/.tart/cache` | 84 |
| `tart clone` to `macos-tartelet-base` | 2026-06-12 | Tartelet-era base | 84 |
| `tart clone` to `macos-tartelet-base-go` | 2026-07-13 | Go-daemon lineage forked | — |
| Simulator prewarm and host hygiene | 2026-07-19 | snapshot `…-pre-prewarm-20260719` | 82 → 87 |
| Android build SDK, NDK, Temurin JDK 17 | 2026-07-20 | snapshot `…-pre-androidsdk-20260720` | 87 → 91 |

### The source image is pinned exactly

The pull landed in `~/.tart/cache/OCIs/ghcr.io/cirruslabs/macos-sequoia-xcode`,
which still exists on node A (emptied by a later prune, but the path is the
record). It was pulled as `:latest` at digest
`sha256:31413f28df83c37b94e76f8feea8046fb1950b3ed42195523408477189a3f76d`.

That digest is still resolvable, and it is **the tag `26.4.1`**:

```
26.4.1 => sha256:31413f28df83c37b94e76f8feea8046fb1950b3ed42195523408477189a3f76d
latest => sha256:985623eba2a05fd455527464229f41ddccf7875b8fcd7a9f04712cbbc19acc2d
```

So node A's ancestor is byte-exactly reproducible today, and `:latest` has since
moved on. Any node C recipe that says `:latest` would install a different Xcode
and break the consumer workflow's version assertion on the first job. **Pin the
digest.**

### The guest is Sequoia, the host is Tahoe

The guest reports `ProductVersion: 15.7.3`, `BuildVersion: 24G419`, Darwin
24.6.0, arm64 — macOS 15 Sequoia — and it never changed across the whole chain.
macOS 26 is the *host* on node A. Do not read `macos-sequoia-*` as stale: it is
the guest OS, deliberately, and Xcode 26.4.1 runs on it.

### Xcode was never installed; it shipped in the image

`/Applications/Xcode_26.4.1.app` (build `17E202`) was present in the pulled
image before any provisioning ran, and it is the only Xcode in `/Applications`.
No `xcodes` CLI, no `.xip`, and no manual install appears anywhere in the legacy
tree. Every consumer only *asserts* the version; `suuudokuuu`'s
`mobile-build.yml` even encodes it in a cache key (`…-xcode26_4_1-17E202-…`).
The workflow follows the image, not the reverse.

### `-go` means the Go daemon's lineage, not the Go toolchain

On 2026-07-13 the incumbent bases were forked with
`tart clone macos-tartelet-base macos-tartelet-base-go` so that the shell-era
`linux-burst-manager.sh` bases stayed intact as a rollback. The suffix records
which control plane owns the image. **No Go toolchain was ever added to the
macOS image**, so there is nothing Go-related for node C to leave out.

### The one checked-in script never ran against this image

`runner-infra/scripts/update-macos-runner-base.sh` in the legacy tree is a
complete, well-formed candidate/verify/promote updater, and it is the best
single description of the *intended* provisioning. It is not the provenance of
the current image: its 2026-07-18 rollout was rejected before it changed any
Tart state, and the 07-19 and 07-20 layers were applied by hand instead. The
recipe below is derived from that script's logic plus what the hand-applied
layers actually did, not from either alone.

## What a Maestro job actually needs

Audited from `vitalyiegorov/suuudokuuu` — the scope
[`MULTI_NODE_PLAN.md`](MULTI_NODE_PLAN.md) assigns to node C.

`mobile-e2e.yml` builds both embedded apps on `macos-builder` first, then
`ios-maestro.yml` runs two shards on `macos-maestro`. The Maestro guest
**downloads a prebuilt `.app` artifact**; it never compiles the application.
That is what makes a lean node C image possible.

The job supplies for itself: the Maestro CLI (installs pinned `2.6.1` if absent
or mismatched), the app under test, and its own flows.

The job assumes preinstalled, and will fail without:

| Requirement | Evidence |
| --- | --- |
| `/Applications/Xcode_26.4.1.app` | `test -d "$xcode_app"` in the "Select Xcode" step — a hard failure |
| A settled, available `iPhone 17 Pro` simulator | the step selects one by exact name and exits non-zero if none is available |
| `python3` | inline heredoc that picks the simulator UDID |
| `node` **on `PATH`** | `run-maestro-suite.sh` merges shard reports with `node - "$output_path" …`; `ios-maestro.yml` has no `setup-node` step |
| A JVM | Maestro is JVM-based; the workflow sets `JAVA_TOOL_OPTIONS` for it |
| `$HOME/actions-runner/run.sh` | `cmd/tart-runner-fleet-bootstrap/main.go:25-27` — the fleet's only runner entry point |
| `/usr/local/libexec/tart-runner-fleet-bootstrap` | `internal/lifecycle/executor.go:25` |

The last two are fleet contracts rather than workflow ones, and they are the
easiest to get wrong: the Tartelet-era image kept its runner under
`$HOME/actions-runner-cache/<version>`, which the Go daemon cannot start.

## What node C does not need

- **The Android build SDK, NDK, and its Temurin JDK.** This is the 2026-07-20
  layer, present only so `suuudokuuu`'s APK build could occupy node A's macOS
  builder in the absence of an arm64-Linux NDK. `MULTI_NODE_PLAN.md` moves that
  build to node B. Measured cost on node A: about 4 GB.
- **Anything `builder`-only** — EAS CLI, CocoaPods, the Expo toolchain, build
  caches. Node C's budget is 4 vCPU / 10240 MiB and `builder` is 6 vCPU /
  12288 MiB, so **no builder job can ever be admitted on node C**. The profile
  is configured out of reach deliberately.
- **A Go toolchain.** It was never in the image; see the `-go` note above.
- **`ccache`.** Node C compiles nothing.
- **A second prewarmed simulator, strictly speaking.** Node C's budget admits
  one 4-vCPU `maestro` guest at a time, and each guest boots exactly one device.
  Keep two anyway for parity with node A and with the workflow comment that
  describes two; the marginal cost is small and the behavioural risk of
  diverging is not.

## Build the node C image

Run these on the Mac Studio. Nothing here contacts node A.

### 0. Preconditions

- Tart installed, and at least **200 GB free** — the image needs roughly 90 GB,
  the fleet's `minFreeDiskGb` guard defaults to 60 GB, and clones grow.
- No fleet daemon in authority yet, or the fleet idle. Building an image while
  guests run competes for the host and for Apple's 2-concurrent-macOS-VM limit.

### 1. Pull the pinned ancestor

```sh
IMAGE=ghcr.io/cirruslabs/macos-sequoia-xcode@sha256:31413f28df83c37b94e76f8feea8046fb1950b3ed42195523408477189a3f76d
BASE=macos-maestro-base

tart pull "$IMAGE"
tart clone "$IMAGE" "$BASE"
```

The download is about 64.6 GB across 263 layers, from GitHub's CDN. Size it
against the alternative in [Size expectations](#size-expectations).

### 2. Size it for the build, and boot it normally

Build-time sizing is unrelated to production sizing: the fleet issues `tart set`
against each clone before boot, so the base's own CPU and memory only affect how
fast this build runs.

```sh
tart set "$BASE" --cpu 6 --memory 12288
tart run "$BASE" --no-graphics --no-audio --no-clipboard \
  >"$HOME/Library/Logs/$BASE-build.log" 2>&1 &
until tart exec "$BASE" true 2>/dev/null; do sleep 2; done
```

Boot the base with **normal disk synchronization**.
[ADR 0013](adr/0013-ephemeral-macos-io.md) confines `sync=none` to disposable
one-job clones and states that the immutable base is never run with it during
construction or maintenance — an interrupted `sync=none` build can leave a
silently corrupt image that every future clone inherits.

The image's default credentials are `admin` / `admin`.

### 3. Provision

```sh
tart exec "$BASE" env MAESTRO_VERSION=2.6.1 SIMULATOR_DEVICE_TYPE='iPhone 17 Pro' \
  SIMULATOR_DEVICE_COUNT=2 SIMULATOR_PREWARM_SETTLE_SECONDS=180 bash -lc '
set -euo pipefail

# --- Xcode first launch, and prove the guest has a GPU ------------------------
xcode_app=/Applications/Xcode_26.4.1.app
test -d "$xcode_app"
sudo xcode-select -s "$xcode_app/Contents/Developer"
export DEVELOPER_DIR="$xcode_app/Contents/Developer"
sudo xcodebuild -runFirstLaunch
xcrun swift -e "import Metal; precondition(MTLCreateSystemDefaultDevice() != nil)"
xcrun simctl runtime dyld_shared_cache update --all

# --- Node on PATH, and in the runner tool cache ------------------------------
# run-maestro-suite.sh calls bare `node`; ios-maestro.yml has no setup-node step.
brew list --versions node@22 >/dev/null 2>&1 || brew install node@22
sudo ln -sfn /opt/homebrew/opt/node@22/bin/node /usr/local/bin/node
sudo ln -sfn /opt/homebrew/opt/node@22/bin/npx  /usr/local/bin/npx
node --version

# --- A JVM for the Maestro CLI ------------------------------------------------
brew list --versions openjdk@17 >/dev/null 2>&1 || brew install openjdk@17
sudo mkdir -p /Library/Java/JavaVirtualMachines
sudo ln -sfn /opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk \
  /Library/Java/JavaVirtualMachines/openjdk-17.jdk
java -version

# --- Maestro, pinned to what the workflow asserts ----------------------------
curl -fsSL https://get.maestro.mobile.dev | env MAESTRO_VERSION="$MAESTRO_VERSION" bash
test "$(MAESTRO_CLI_NO_ANALYTICS=true "$HOME/.maestro/bin/maestro" --version \
  2>&1 | tail -n 1 | tr -d "\r")" = "$MAESTRO_VERSION"
'
```

Then the runner payload, at the one path the fleet will start:

```sh
tart exec "$BASE" bash -lc '
set -euo pipefail
version=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest |
  python3 -c "import sys,json;print(json.load(sys.stdin)[\"tag_name\"].lstrip(\"v\"))")
rm -rf "$HOME/actions-runner"
mkdir -p "$HOME/actions-runner"
curl --retry 3 --retry-all-errors -fsSL -o /tmp/runner.tar.gz \
  "https://github.com/actions/runner/releases/download/v$version/actions-runner-osx-arm64-$version.tar.gz"
tar xzf /tmp/runner.tar.gz -C "$HOME/actions-runner"
rm /tmp/runner.tar.gz
test -x "$HOME/actions-runner/run.sh"
# A sealed base carries a pristine payload only. Registration state and job
# workspaces must stay unique to each ephemeral clone.
test ! -e "$HOME/actions-runner/.runner"
test ! -e "$HOME/actions-runner/.credentials"
'
```

And the bootstrap helper from the same release the node will run — see
[`INSTALL.md`](../INSTALL.md) for downloading and verifying that release:

```sh
base64 < "$RELEASE_DIR/tart-runner-fleet-bootstrap" |
  tart exec -i "$BASE" bash -lc '
set -euo pipefail
base64 -d > /tmp/bootstrap
sudo install -m 0755 -o root -g wheel /tmp/bootstrap \
  /usr/local/libexec/tart-runner-fleet-bootstrap
rm /tmp/bootstrap
'
```

### 4. Prewarm the simulators

This is the 2026-07-19 layer, and on node A it was worth more than everything
else combined: a Maestro VM went from 91 minutes cold to 51 minutes prewarmed.
Without it, each clone creates a fresh device on first use and pays an iOS 26
first-boot asset, extension, and accessibility-indexing storm that also breaks
the XCUITest driver.

A Tart clone preserves these on-disk device caches. It is **not** a RAM
snapshot: every clone still performs a normal macOS and Simulator boot.

```sh
tart exec "$BASE" env SIMULATOR_DEVICE_TYPE='iPhone 17 Pro' \
  SIMULATOR_DEVICE_COUNT=2 SETTLE=180 bash -lc '
set -euo pipefail
export DEVELOPER_DIR=/Applications/Xcode_26.4.1.app/Contents/Developer
runtime=$(xcrun simctl list runtimes -j | python3 -c "
import sys, json
rs = [r for r in json.load(sys.stdin)[\"runtimes\"]
      if r.get(\"isAvailable\", True) and \".SimRuntime.iOS-\" in r[\"identifier\"]]
rs.sort(key=lambda r: [int(p) for p in r[\"version\"].split(\".\")], reverse=True)
print(rs[0][\"identifier\"])")
device_type=$(xcrun simctl list devicetypes -j | python3 -c "
import sys, json, os
name = os.environ[\"SIMULATOR_DEVICE_TYPE\"]
print(next(d[\"identifier\"] for d in json.load(sys.stdin)[\"devicetypes\"]
           if d[\"name\"] == name))")
test -n "$runtime" && test -n "$device_type"

xcrun simctl shutdown all >/dev/null 2>&1 || true
mkdir -p "$HOME/.ci-base-manifest"
: > "$HOME/.ci-base-manifest/macos-simulator-udids"
for lane in $(seq 1 "$SIMULATOR_DEVICE_COUNT"); do
  udid=$(xcrun simctl create "$SIMULATOR_DEVICE_TYPE" "$device_type" "$runtime")
  echo "$udid" >> "$HOME/.ci-base-manifest/macos-simulator-udids"
  xcrun simctl boot "$udid"
  xcrun simctl bootstatus "$udid" -b
  sleep "$SETTLE"          # let indexing and first-launch work drain
  xcrun simctl shutdown "$udid"
done
test "$(wc -l < "$HOME/.ci-base-manifest/macos-simulator-udids" | tr -d " ")" \
  = "$SIMULATOR_DEVICE_COUNT"
'
```

### 5. Host hygiene

A sealed CI guest must not start an OS update, sleep, or index in the middle of
a job. Unified logging and crash reports stay on: workflows rely on simulator
and crash logs to diagnose failures, and ADR 0013 explicitly refuses to trade
diagnostics for throughput.

```sh
tart exec "$BASE" bash -lc '
set -euo pipefail
sudo mdutil -i off / || true
sudo mdutil -d / || true
sudo softwareupdate --schedule off || true
sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticCheckEnabled -bool false
sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload -bool false
sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates -bool false
defaults write NSGlobalDomain NSAppSleepDisabled -bool true
defaults write com.apple.CoreSimulator FBLaunchWatchdogScale -int 2
sudo pmset -a sleep 0 disksleep 0 displaysleep 0 powernap 0 || true
brew cleanup
'
```

## Seal and verify

Stop the base from inside, so the guest filesystem is consistent:

```sh
tart exec "$BASE" sync || true
tart exec "$BASE" sudo shutdown -h now || true
```

Then boot once more and verify — a reboot check is what catches provisioning
that only worked in the session that applied it:

```sh
tart run "$BASE" --no-graphics --no-audio --no-clipboard \
  >"$HOME/Library/Logs/$BASE-verify.log" 2>&1 &
until tart exec "$BASE" true 2>/dev/null; do sleep 2; done

tart exec "$BASE" bash -lc '
set -euo pipefail
test -d /Applications/Xcode_26.4.1.app
xcodebuild -version
test -x "$HOME/actions-runner/run.sh"
test ! -e "$HOME/actions-runner/.runner"
test -x /usr/local/libexec/tart-runner-fleet-bootstrap
command -v node && node --version
command -v python3
java -version
"$HOME/.maestro/bin/maestro" --version
xcrun swift -e "import Metal; precondition(MTLCreateSystemDefaultDevice() != nil)"
while IFS= read -r udid; do xcrun simctl boot "$udid"; done \
  < "$HOME/.ci-base-manifest/macos-simulator-udids"
while IFS= read -r udid; do xcrun simctl bootstatus "$udid" -b; done \
  < "$HOME/.ci-base-manifest/macos-simulator-udids"
xcrun simctl shutdown all
'

tart exec "$BASE" sync || true
tart exec "$BASE" sudo shutdown -h now || true
```

From here the base stays **stopped forever**. Maintenance follows node A's
discipline, which is also [ADR 0011](adr/0011-atomic-production-updates.md)'s
shape: clone a dated candidate, change the candidate, verify it, then promote by
rename and keep the outgoing image as `<base>-pre-<change>-<yyyymmdd>`. Never
edit a live base in place — node A's recoverable history exists only because
every layer was renamed rather than overwritten.

## Wire it to the node C configuration

`macosBurst.baseVm` must equal the Tart name exactly:

```json
{
  "hostBudget": { "cpu": 4, "memoryMb": 10240 },
  "macosBurst": {
    "enabled": true,
    "baseVm": "macos-maestro-base",
    "vmPrefix": "trf-macos",
    "maestro": { "id": "maestro", "label": "trf-macos-arm64-4x7",
                 "cpu": 4, "memoryMb": 7168, "maxActive": 2,
                 "aliases": ["macos-maestro"] }
  }
}
```

Use a distinct name. Node A's `macos-tartelet-base-go` carries the Android
toolchain and the Tartelet-era history; a node C image that answered to the same
name would make two materially different images indistinguishable in an
incident.

`builder` is still required by config validation, so configure it out of reach
of the 4 vCPU / 10240 MiB budget as `MULTI_NODE_PLAN.md` specifies, and confirm
the budget binds: with one `maestro` guest live, `fleet status` must report no
remaining envelope.

Do **not** set `diskGb` on a macOS profile. ADR 0006 requires a positive floor
only for Linux profiles in authority mode; macOS guests keep the base's
immutable 140 GB virtual capacity, which is sparse on APFS and consumes host
storage only as written. Tart cannot shrink a disk, so 140 GB is inherited from
the Cirrus image whether or not it is wanted.

## Size expectations

Measured on node A with `tart list` (`Size` is GB actually consumed; every image
in the chain has the same 140 GB virtual disk):

| Image | Used GB |
| --- | ---: |
| `macos-tartelet-base-go` (node A today) | 91 |
| `…-pre-androidsdk-20260720` (before the Android layer) | 87 |
| `…-pre-prewarm-20260719` | 82 |
| `ghcr.io/cirruslabs/macos-sequoia-xcode:26.4.1` as pulled | 84 |

A Maestro-only node C image should land near **85–89 GB** — the Cirrus baseline,
plus roughly 5 GB of settled simulators and hygiene, plus about 1.5 GB of Node,
JDK, Maestro, and the runner payload, minus the roughly 4 GB Android layer.

**State the saving honestly: it is small, and it is not the point.** Xcode and
the bundled simulator runtimes dominate the image, and node C needs both. The
reason to build rather than transfer is *where the bytes come from*:

| | Transfer node A's image | Build on node C |
| --- | --- | --- |
| Bytes | ~91 GB of raw disk | 64.6 GB compressed |
| Source | node A's residential uplink | GitHub's CDN, at node C's downlink |
| Realistic time | days | under an hour on a fast link |
| Effect on production | competes with live job traffic for the whole transfer | none — node A is not involved |
| Reproducible later | no, it is a copy of a hand-built artifact | yes, the digest is pinned |

If the image must be smaller, the only large remaining target is the non-iOS
simulator runtimes the Cirrus image ships. Removing tvOS, watchOS, and visionOS
runtimes with `xcrun simctl runtime delete` plausibly recovers meaningful space.
This is **untested here** — measure before and after with `tart list`, and treat
it as an optional trim on a candidate clone, never on a promoted base.

## Known unknowns

Stated so a future operator does not mistake inference for measurement.

- **The literal 2026-06-10 `tart pull` command is not recorded.** No transcript
  from that day survives. The cache path, the digest, and the 2026-06-12
  `tart clone` are the evidence; the pull is inferred from them. The digest
  match is exact, so this does not weaken the recipe.
- **What the Cirrus image already ships is not fully enumerated.** The
  provisioning steps for Node and the JDK are written to be idempotent for that
  reason. Verify inside the guest before assuming either is missing or present,
  and adjust rather than reinstall.
- **Whether Maestro 2.6.1 still needs a separate JVM is unverified.** The legacy
  updater installed `openjdk@17` explicitly and the workflow sets
  `JAVA_TOOL_OPTIONS`, so a JDK is included here. Recent Maestro releases may
  bundle a runtime; if a guest test proves it does, the JDK can be dropped.
- **The image's exact `xcrun simctl` runtime version is inferred**, not read.
  The prewarm step selects the newest available iOS runtime rather than pinning
  one, matching what the consumer workflow does.
- **The 84 → 82 GB dip** between the pulled image and the pre-prewarm snapshot
  is unexplained. It is most likely `brew cleanup` plus sparse-file accounting,
  but no measurement confirms it.
- **The optional simulator-runtime trim is unquantified**, as noted above.
- **Node C's `fleet.json` does not exist yet.** The fragment above follows
  `MULTI_NODE_PLAN.md` and node A's live configuration; the authoritative
  values are whatever that node's rendered config ends up carrying.
