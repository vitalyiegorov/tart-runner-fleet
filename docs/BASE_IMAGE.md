# macOS base image: provenance and reproduction

[ADR 0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md) makes each node
responsible for its own guests, and [`MULTI_NODE_PLAN.md`](MULTI_NODE_PLAN.md)
has mac-studio — a remote Mac Studio — share mac-mini's labels rather than
own one scope's scale set alone: both advertise `maestro`, and, per ADR
0034's shared-label amendment, `builder` and the Linux tiers too. Neither
record says where a macOS base image comes from. This document closes that
gap: it reconstructs how mac-mini's image was actually built, and gives a
mac-studio operator a recipe that produces an equivalent Maestro-only image
locally instead of copying mac-mini's 91 GB disk across a residential uplink.
It predates the shared-label amendment and still only covers `maestro`; see
[Known unknowns](#known-unknowns) for what that leaves open now that
mac-studio also declares `builder`.

The fleet never builds images. It clones a stopped base and sizes the clone
([ADR 0006](adr/0006-per-profile-disk-floors.md)). Image construction is an
operator activity, performed once per node and repeated only for maintenance.

## Contents

- [Provenance of mac-mini's image](#provenance-of-mac-minis-image)
- [What a Maestro job actually needs](#what-a-maestro-job-actually-needs)
- [What mac-studio does not need](#what-mac-studio-does-not-need)
- [Build the mac-studio image](#build-the-mac-studio-image)
- [Seal and verify](#seal-and-verify)
- [Wire it to the mac-studio configuration](#wire-it-to-the-mac-studio-configuration)
- [Size expectations](#size-expectations)
- [Known unknowns](#known-unknowns)

## Provenance of mac-mini's image

mac-mini's `macos-tartelet-base-go` was never produced by a checked-in script.
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
which still exists on mac-mini (emptied by a later prune, but the path is the
record). It was pulled as `:latest` at digest
`sha256:31413f28df83c37b94e76f8feea8046fb1950b3ed42195523408477189a3f76d`.

That digest is still resolvable, and it is **the tag `26.4.1`**:

```
26.4.1 => sha256:31413f28df83c37b94e76f8feea8046fb1950b3ed42195523408477189a3f76d
latest => sha256:985623eba2a05fd455527464229f41ddccf7875b8fcd7a9f04712cbbc19acc2d
```

So mac-mini's ancestor is byte-exactly reproducible today, and `:latest` has since
moved on. Any mac-studio recipe that says `:latest` would install a different Xcode
and break the consumer workflow's version assertion on the first job. **Pin the
digest.**

### The guest is Sequoia, the host is Tahoe

The guest reports `ProductVersion: 15.7.3`, `BuildVersion: 24G419`, Darwin
24.6.0, arm64 — macOS 15 Sequoia — and it never changed across the whole chain.
macOS 26 is the *host* on mac-mini. Do not read `macos-sequoia-*` as stale: it is
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
macOS image**, so there is nothing Go-related for mac-studio to leave out.

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
[`MULTI_NODE_PLAN.md`](MULTI_NODE_PLAN.md) assigns to mac-studio.

`mobile-e2e.yml` builds both embedded apps on `macos-builder` first, then
`ios-maestro.yml` runs two shards on `macos-maestro`. The Maestro guest
**downloads a prebuilt `.app` artifact**; it never compiles the application.
That is what makes a lean mac-studio image possible.

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

### The daemon replaces PATH and the Android environment outright

`internal/guestbootstrap/bootstrap.go` does not let the runner process inherit
the guest's login `PATH`. On darwin it substitutes exactly

```
/Users/admin/.rbenv/shims:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
```

(`runnerToolchainPath`), and it unconditionally exports
`ANDROID_HOME=/Users/admin/android-sdk` and `ANDROID_SDK_ROOT` to the same
path on every macOS job (`childEnvironmentForOS`, `darwinAndroidSDKPath`) —
whether or not an Android SDK is actually present. Two consequences for this
recipe:

- Anything a job needs on `PATH` — `node`, `python3`, anything else — must
  resolve on that exact list, not on whatever a login shell or
  `brew shellenv` would produce. `/usr/local/bin` is on it; `/opt/homebrew/bin`
  is too, but keg-only Homebrew formulae (which is how versioned formulae like
  `node@24` install) never symlink themselves there. That is why this recipe
  symlinks tools into `/usr/local/bin` explicitly rather than relying on Brew.
- The "no Android SDK needed" framing in the next section is a build-input
  claim, not an environment one. The Cirrus image ships an `android-sdk`
  directory at that exact path, and every macOS job — `maestro` included —
  gets `ANDROID_HOME`/`ANDROID_SDK_ROOT` pointed at it regardless. mac-studio not
  building Android artifacts means nothing on this node reads the variable,
  not that the variable is unset or the path absent.

## What mac-studio does not need

- **The Android build SDK, NDK, and its Temurin JDK.** This is the 2026-07-20
  layer, present only so `suuudokuuu`'s APK build could occupy mac-mini's macOS
  builder in the absence of an arm64-Linux NDK. `MULTI_NODE_PLAN.md` moves that
  build to geekom. Measured cost on mac-mini: about 4 GB.
- **Anything `builder`-only** — EAS CLI, CocoaPods, the Expo toolchain, build
  caches — **for this image specifically**, which is Maestro-only by scope,
  not by the host's admission envelope. mac-studio's `hostBudget` was
  originally 4 vCPU / 10240 MiB, which put `builder` (6 vCPU / 12288 MiB)
  permanently out of reach; it was later revised to **6 vCPU / 16384 MiB**
  once mac-studio's physical spec (14 cores / 36 GiB) was confirmed, and that
  budget does fit `builder`. Per ADR 0034's shared-label amendment mac-studio
  now declares the `builder` label alongside mac-mini, so a `builder` job can
  land here — this recipe still only builds a lean, Maestro-only image, and
  such a job would fail on missing tooling until a separate image update adds
  it. Building that image is future work, not covered by this document.
- **A Go toolchain.** It was never in the image; see the `-go` note above.
- **`ccache`.** This recipe's image compiles nothing.
- **Creating a second `iPhone 17 Pro` simulator, strictly speaking.** A
  `maestro` guest is 4 vCPU regardless of which node's budget admits it, and
  each guest boots exactly one device. Measured on `26.4.1`: the Cirrus image already ships one
  available `iPhone 17 Pro` device. `ios-maestro.yml` selects a simulator by
  exact name, so a second `simctl create` with the same device type produces a
  same-named duplicate — an ambiguity, not a spare. The recipe below prewarms
  whatever the image already shipped instead of creating anything.

## Build the mac-studio image

Run these on the Mac Studio. Nothing here contacts mac-mini.

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
# Measured on 26.4.1: the image already ships node@24 (v24.15.0) — running
# `brew install node@22` here installs a second, conflicting Node instead of
# satisfying the requirement. Symlink the shipped binary instead; v24.15.0 was
# verified to resolve as `node` under the daemon exact runner PATH (see
# "The daemon replaces PATH and the Android environment outright" above).
node_prefix=$(brew --prefix node@24)
sudo ln -sfn "$node_prefix/bin/node" /usr/local/bin/node
sudo ln -sfn "$node_prefix/bin/npx"  /usr/local/bin/npx
node --version

# --- A JVM for the Maestro CLI ------------------------------------------------
# Settled by measurement, not inferred: ~/.maestro/lib holds 195 jars and no
# bundled JRE, and the maestro launcher is a Gradle start script that fails
# outright (JAVA_HOME is not set and no java command could be found) without
# one. A JDK is required. openjdk@17 is already in the image and already
# registers at /Library/Java/JavaVirtualMachines/openjdk-17.jdk, so the steps
# below are idempotent rather than load-bearing on a stock image.
brew list --versions openjdk@17 >/dev/null 2>&1 || brew install openjdk@17
sudo mkdir -p /Library/Java/JavaVirtualMachines
sudo ln -sfn /opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk \
  /Library/Java/JavaVirtualMachines/openjdk-17.jdk
java -version

# --- Maestro, pinned to what the workflow asserts ----------------------------
curl -fsSL https://get.maestro.mobile.dev | env MAESTRO_VERSION="$MAESTRO_VERSION" bash
test "$(MAESTRO_CLI_NO_ANALYTICS=true "$HOME/.maestro/bin/maestro" --version \
  2>&1 | tail -n 1 | tr -d "\r")" = "$MAESTRO_VERSION"
# ~/.maestro/bin is not on the daemon runner PATH above, so a bare `maestro`
# invocation on this image alone would fail. ios-maestro.yml already accounts
# for this itself, adding the directory via GITHUB_PATH before it calls
# maestro — no symlink is required here for the job to find it. If one is
# ever added anyway, /usr/local/bin/maestro -> ~/.maestro/bin/maestro is
# safe: the launcher resolves symlinks before locating its jars relative to
# its own path.
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
[`INSTALL.md`](../INSTALL.md) for downloading and verifying that release. The
released asset is named `tart-runner-fleet-bootstrap-darwin-arm64` (the
architecture suffix is part of the filename, not just the install path):

```sh
base64 < "$RELEASE_DIR/tart-runner-fleet-bootstrap-darwin-arm64" |
  tart exec -i "$BASE" bash -lc '
set -euo pipefail
base64 -d > /tmp/bootstrap
# /usr/local/libexec does not exist in the guest by default; the Cirrus image
# never creates it, so `install` fails without this.
sudo mkdir -p /usr/local/libexec
sudo install -m 0755 -o root -g wheel /tmp/bootstrap \
  /usr/local/libexec/tart-runner-fleet-bootstrap
rm /tmp/bootstrap
'
```

### 4. Prewarm the simulators

This is the 2026-07-19 layer, and on mac-mini it was worth more than everything
else combined: a Maestro VM went from 91 minutes cold to 51 minutes prewarmed.
Without it, each clone creates a fresh device on first use and pays an iOS 26
first-boot asset, extension, and accessibility-indexing storm that also breaks
the XCUITest driver.

A Tart clone preserves these on-disk device caches. It is **not** a RAM
snapshot: every clone still performs a normal macOS and Simulator boot.

Measured on `26.4.1`: the Cirrus image already ships an available
`iPhone 17 Pro` device. `ios-maestro.yml` selects a simulator **by exact
name**, so calling `simctl create` here would add a second device with the
identical name — a same-named ambiguity, not a spare. Prewarm whatever the
image already shipped instead of creating anything:

```sh
tart exec "$BASE" env SIMULATOR_DEVICE_TYPE='iPhone 17 Pro' SETTLE=180 bash -lc '
set -euo pipefail
export DEVELOPER_DIR=/Applications/Xcode_26.4.1.app/Contents/Developer

xcrun simctl shutdown all >/dev/null 2>&1 || true

udids=$(xcrun simctl list devices -j | python3 -c "
import sys, json, os
name = os.environ[\"SIMULATOR_DEVICE_TYPE\"]
devices = json.load(sys.stdin)[\"devices\"]
for entries in devices.values():
    for d in entries:
        if d[\"name\"] == name and d.get(\"isAvailable\", True):
            print(d[\"udid\"])")
test -n "$udids"

for udid in $udids; do
  xcrun simctl boot "$udid"
  xcrun simctl bootstatus "$udid" -b
  sleep "$SETTLE"          # let indexing and first-launch work drain
  xcrun simctl shutdown "$udid"
done
'
```

This recipe does not maintain its own UDID manifest. An earlier draft wrote
one to `~/.ci-base-manifest/macos-simulator-udids`, but no workflow reads that
path — `ios-maestro.yml` queries `xcrun simctl list -j` directly at job time
(its own comment references an unrelated `~/.budgie-ci/simulators.json`,
which also is not this). Query `simctl` live, as above and in
[Seal and verify](#seal-and-verify), rather than trusting a file that nothing
downstream honors.

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

### Write the capability manifest first

The last thing to go into an image is its own account of what is in it. The
bootstrap helper reads this file in every ephemeral clone and compares it against
what the daemon expected of the scale sets that routed the job here; a mismatch
fails the instance at the `bootstrap` stage with a named reason instead of
letting the job die inside a consumer's workflow (ADR 0034, amendment
2026-08-04c §3; the reasons are in [`OPERATIONS.md`](OPERATIONS.md)).

**List only what this image actually carries.** A manifest is worth exactly the
audit behind it, and one that lists a capability the image lacks is worse than no
manifest at all: it converts a caught failure into a flaky one. The list below is
the mac-studio image this document builds, and every entry corresponds to a step
above.

```sh
tart exec "$BASE" bash -lc '
set -euo pipefail
sudo install -d -o root -g wheel -m 0755 /usr/local/share/tart-runner-fleet
sudo tee /usr/local/share/tart-runner-fleet/image-capabilities.json >/dev/null <<JSON
{"schemaVersion": 1, "image": "macos-tartelet-base", "sealedAt": "'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",
 "capabilities": ["android-build-sdk", "ios-simulator-prewarmed", "jvm", "maestro-cli", "node-runtime"]}
JSON
sudo chmod 0644 /usr/local/share/tart-runner-fleet/image-capabilities.json
python3 -m json.tool /usr/local/share/tart-runner-fleet/image-capabilities.json >/dev/null
'
```

### The macOS capability vocabulary

A capability is a lowercase identifier matching
`^[a-z0-9][a-z0-9-]*$` that does not end in a hyphen. It names something a base image provides that
neither the profile's resource vector nor the canonical label can state. The
vocabulary is not open: it is the result of an audit of the live fleet, and a
name that is not in this table — or in the Linux one in
[`LINUX_BASE_IMAGE.md`](LINUX_BASE_IMAGE.md) — is a name no image has been
audited for.

| macOS capability | What it means |
| --- | --- |
| `xcode` | Xcode installed, first-launch complete, and a GPU the guest can see |
| `ios-simulator-prewarmed` | the simulator runtimes of [step 4](#4-prewarm-the-simulators) already booted once |
| `android-build-sdk` | an Android SDK and NDK a job can build an APK against |
| `jvm` | a JDK registered where the Maestro launcher finds it |
| `maestro-cli` | the pinned `maestro` CLI of [step 3](#3-provision), with its jars |
| `node-runtime` | `node` resolvable on the exact daemon runner PATH |
| `cocoapods` | a working `pod` for a workflow that installs pods in the job |

The two vocabularies are separate because a node's two base images answer
separate questions: `xcode` is a fact about the macOS image and says nothing
about the Linux one.

The identifiers must match the node's `macosBurst.baseImageCapabilities`
exactly. They are compared for string equality across two machines'
configuration files, so a spelling that differs is a capability that does not
exist. `fleet config validate` given more than one node's configuration compares
them; see [`CLI.md`](CLI.md) and
[`config/nodes/README.md`](../config/nodes/README.md).

### Then stop the guest

**This step has exactly one way to fail silently, and it produces a corrupt
base with no error anywhere.** `tart exec` runs commands with PATH
`/bin:/usr/bin:/usr/sbin:/usr/local/bin:/opt/homebrew/bin` — there is no
`/sbin` in it. `sudo shutdown -h now` therefore fails with
`sudo: shutdown: command not found`, and because both commands below are
chained with `|| true`, that failure is swallowed and the VM simply keeps
running. A follower who does not check has sealed what is still a live,
unstopped disk — every future clone inherits an unbooted-consistent,
effectively corrupt base, and nothing says so until a job fails against it.
Always call `shutdown` by its absolute path, and always verify the guest
actually stopped before trusting the seal.

Stop the base from inside, so the guest filesystem is consistent:

```sh
tart exec "$BASE" sync || true
tart exec "$BASE" sudo /sbin/shutdown -h now || true

# Verify the VM actually stopped — do not trust the exit code above alone.
until [ "$(tart list --format json | python3 -c '
import json, sys
name = sys.argv[1]
rows = json.load(sys.stdin)
print(any(v["Name"] == name and v["Running"] for v in rows))
' "$BASE")" = "False" ]; do
  sleep 2
done
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
test -r /usr/local/share/tart-runner-fleet/image-capabilities.json
python3 -m json.tool /usr/local/share/tart-runner-fleet/image-capabilities.json >/dev/null
command -v node && node --version
command -v python3
java -version
"$HOME/.maestro/bin/maestro" --version
xcrun swift -e "import Metal; precondition(MTLCreateSystemDefaultDevice() != nil)"
udids=$(xcrun simctl list devices -j | python3 -c "
import sys, json
devices = json.load(sys.stdin)[\"devices\"]
for entries in devices.values():
    for d in entries:
        if d[\"name\"] == \"iPhone 17 Pro\" and d.get(\"isAvailable\", True):
            print(d[\"udid\"])")
test -n "$udids"
for udid in $udids; do xcrun simctl boot "$udid"; done
for udid in $udids; do xcrun simctl bootstatus "$udid" -b; done
xcrun simctl shutdown all
'

tart exec "$BASE" sync || true
tart exec "$BASE" sudo /sbin/shutdown -h now || true

# Verify the VM actually stopped — see the warning above; this is the same
# silent-failure risk on every seal, not just the first one.
until [ "$(tart list --format json | python3 -c '
import json, sys
name = sys.argv[1]
rows = json.load(sys.stdin)
print(any(v["Name"] == name and v["Running"] for v in rows))
' "$BASE")" = "False" ]; do
  sleep 2
done
```

From here the base stays **stopped forever**. Maintenance follows mac-mini's
discipline, which is also [ADR 0011](adr/0011-atomic-production-updates.md)'s
shape: clone a dated candidate, change the candidate, verify it, then promote by
rename and keep the outgoing image as `<base>-pre-<change>-<yyyymmdd>`. Never
edit a live base in place — mac-mini's recoverable history exists only because
every layer was renamed rather than overwritten.

## Wire it to the mac-studio configuration

`macosBurst.baseVm` must equal the Tart name exactly:

```json
{
  "hostBudget": { "cpu": 6, "memoryMb": 16384 },
  "macosBurst": {
    "enabled": true,
    "baseVm": "macos-maestro-base",
    "baseImageCapabilities": ["ios-simulator-prewarmed", "jvm", "maestro-cli", "node-runtime", "xcode"],
    "vmPrefix": "trf-macos",
    "maestro": { "id": "maestro", "label": "trf-macos-arm64-4x7",
                 "cpu": 4, "memoryMb": 7168, "maxActive": 2,
                 "aliases": ["macos-maestro"] }
  }
}
```

Use a distinct name. mac-mini's `macos-tartelet-base-go` carries the Android
toolchain and the Tartelet-era history; a mac-studio image that answered to the same
name would make two materially different images indistinguishable in an
incident.

**`macosBurst.baseVm` in the checked-in config is the source of truth, not the
name suggested above.** Observed on mac-studio: the config committed there points
`baseVm` at `macos-tartelet-base`, not `macos-maestro-base` — itself a
collision with mac-mini's pre-`-go` Tartelet-era base, the opposite of the
distinct-name advice this section gives. Before wiring anything in, read
mac-studio's actual config and reconcile the two: either name the built image
`macos-tartelet-base` to match it, or change the config's `baseVm` to a
distinct name and build under that instead. Whatever the config says is what
the daemon clones on the first job; a recipe followed under the wrong
assumption produces a base nothing points at.

`macosBurst.baseImageCapabilities` must equal what the manifest inside the image
says, for the same reason `baseVm` must equal the Tart name: one of them is
checked against the machine and the other against every other node that
advertises the same label. The list above omits `android-build-sdk` and
`cocoapods` deliberately — this recipe is Maestro-only and installs neither, and
a mac-studio that advertises `macos-maestro` beside mac-mini must therefore either
carry them or share only labels whose consumers do not need them.

At mac-studio's current `hostBudget` (6 vCPU / 16384 MiB) `builder` fits, so it
is no longer configured out of reach the way an earlier revision of this
document specified — see [What mac-studio does not need](#what-mac-studio-does-not-need)
for what that means and does not mean for this recipe. Confirm the budget
binds regardless: with one guest live of any profile — `builder` at 6 vCPU /
12288 MiB or `maestro` at 4 vCPU / 7168 MiB — `fleet status` must report no
remaining envelope for a second one.

Do **not** set `diskGb` on a macOS profile. ADR 0006 requires a positive floor
only for Linux profiles in authority mode; macOS guests keep the base's
immutable 140 GB virtual capacity, which is sparse on APFS and consumes host
storage only as written. Tart cannot shrink a disk, so 140 GB is inherited from
the Cirrus image whether or not it is wanted.

## Size expectations

Measured on mac-mini with `tart list` (`Size` is GB actually consumed; every image
in the chain has the same 140 GB virtual disk):

| Image | Used GB |
| --- | ---: |
| `macos-tartelet-base-go` (mac-mini today) | 91 |
| `…-pre-androidsdk-20260720` (before the Android layer) | 87 |
| `…-pre-prewarm-20260719` | 82 |
| `ghcr.io/cirruslabs/macos-sequoia-xcode:26.4.1` as pulled | 84 |

A Maestro-only mac-studio image was predicted to land near **85–89 GB** — the
Cirrus baseline, plus roughly 5 GB of settled simulators and hygiene, plus
about 1.5 GB of Node, JDK, Maestro, and the runner payload, minus the roughly
4 GB Android layer.

**Measured on mac-studio's `26.4.1` build, it did not land there: it added no
measurable disk at all.** `tart list` reported **84 GB both before and after**
the full recipe — identical to the pulled image, not the predicted 85–89 GB.
Two things the prediction did not account for absorbed what it expected to
add: `brew cleanup` in the host-hygiene step freed about 220 MB on its own,
and the image's dyld shared caches are already built by Apple, so
`xcrun simctl runtime dyld_shared_cache update --all` and the simulator
prewarm cost rounding-error space rather than gigabytes. Treat 85–89 GB as an
upper bound this recipe did not reach, not an expectation, and measure with
`tart list` rather than budgeting off this table.

**State the saving honestly: it is small, and it is not the point.** Xcode and
the bundled simulator runtimes dominate the image, and mac-studio needs both. The
reason to build rather than transfer is *where the bytes come from*:

| | Transfer mac-mini's image | Build on mac-studio |
| --- | --- | --- |
| Bytes | ~91 GB of raw disk | 64.6 GB compressed |
| Source | mac-mini's residential uplink | GitHub's CDN, at mac-studio's downlink |
| Realistic time | days | under an hour on a fast link |
| Effect on production | competes with live job traffic for the whole transfer | none — mac-mini is not involved |
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
- **What the Cirrus image already ships is not fully enumerated.** Node
  (`node@24`, v24.15.0) and `openjdk@17` are confirmed present by direct
  measurement on `26.4.1` — see [step 3](#3-provision) — but nothing else in
  the image has been enumerated the same way. The provisioning steps stay
  idempotent for that reason. Verify inside the guest before assuming
  anything else is missing, and adjust rather than reinstall.
- **The image's exact `xcrun simctl` runtime version is inferred**, not read.
  The prewarm step selects the newest available iOS runtime rather than pinning
  one, matching what the consumer workflow does.
- **The 84 → 82 GB dip** between the pulled image and the pre-prewarm snapshot
  is unexplained. It is most likely `brew cleanup` plus sparse-file accounting,
  but no measurement confirms it.
- **The optional simulator-runtime trim is unquantified**, as noted above.
- **mac-studio's config exists and disagrees with this document's suggested
  `baseVm`.** An earlier draft of this section assumed `fleet.json` did not
  exist yet; it does, and its `macosBurst.baseVm` is `macos-tartelet-base`,
  not `macos-maestro-base`. See
  [Wire it to the mac-studio configuration](#wire-it-to-the-mac-studio-configuration)
  — the config is the authoritative value, not the fragment above.
- **mac-studio now declares `builder` alongside mac-mini, and this document
  does not build a `builder`-capable image.** The recipe above is
  Maestro-only by design — see
  [What mac-studio does not need](#what-mac-studio-does-not-need) — and
  mac-studio's revised `hostBudget` (6 vCPU / 16384 MiB) makes `builder`
  admissible where the original 4 vCPU / 10240 MiB budget could not. Whether
  a `builder` job that lands on mac-studio today succeeds or fails on missing
  tooling has not been measured; ADR 0034's amendment 2026-08-04c's
  capability-parity check would refuse the declaration once the render step
  and rendered configuration exist, but until then this is an unaudited gap,
  not a closed one. Building a `builder`-capable mac-studio image is future
  work.
