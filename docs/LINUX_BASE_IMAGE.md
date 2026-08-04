# Linux base image: provenance and reproduction

[`BASE_IMAGE.md`](BASE_IMAGE.md) does this for the macOS base. This is its
Linux twin, and it exists for the same reason: [ADR
0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md) makes each node
responsible for its own guests, and nothing in the repository said where a
Linux base image comes from. A node that needs one had only two options —
copy another node's disk, or reconstruct the recipe from a script in a
different repository that was written for a control plane this daemon
replaced.

The fleet never builds images. It clones a stopped base, sizes the clone
([ADR 0006](adr/0006-per-profile-disk-floors.md)), starts one runner in it, and
deletes it. Image construction is an operator activity, performed once per node
and repeated only for maintenance.

**This document was written by executing it**, on a remote Mac Studio, against
a live authority-mode daemon serving macOS jobs on the same host. Every number,
path, and failure below was measured there. Two of the steps exist only because
the build failed without them.

## Contents

- [Provenance: the legacy script and what it gets wrong](#provenance-the-legacy-script-and-what-it-gets-wrong)
- [What the daemon requires of a Linux guest](#what-the-daemon-requires-of-a-linux-guest)
- [Build the image](#build-the-image)
- [Seal and verify](#seal-and-verify)
- [Deviations from the legacy script](#deviations-from-the-legacy-script)
- [Wire it to the node configuration](#wire-it-to-the-node-configuration)
- [Size and time expectations](#size-and-time-expectations)
- [Adapting this for node B (amd64, containers)](#adapting-this-for-node-b-amd64-containers)
- [Known unknowns](#known-unknowns)

## Provenance: the legacy script and what it gets wrong

Unlike the macOS image, the Linux base *does* have a checked-in ancestor:
`runner-infra/scripts/create-linux-runner-base.sh` in the pre-fleet
`ci-migration` tree, 288 lines, driven by `config/linux-burst.json`. It is a
complete candidate/verify/promote builder and it is the honest record of what
a Linux runner guest is expected to contain. Its package list, its NodeSource
pinning, its Yarn set, and its Playwright system-dependency step are carried
into the recipe below unchanged.

It was written for `linux-burst-manager.sh`, the shell control plane the Go
daemon replaced, and it makes three classes of assumption that are wrong on any
node running this fleet:

**1. It installs the runner where the daemon cannot start it.** The script
unpacks the Actions runner into `$HOME/actions-runner-cache/<version>/` and
asserts `config.sh` is executable there. The shell manager registered a runner
by calling `config.sh` from that cache. The Go daemon never calls `config.sh`:
`cmd/tart-runner-fleet-bootstrap/main.go` computes exactly
`$HOME/actions-runner/run.sh` and `internal/guestbootstrap` refuses to start
anything else. An image built by the script verbatim produces a guest that
boots, registers nothing, and fails every job at the `bootstrap` stage. This is
the same trap `BASE_IMAGE.md` records for the Tartelet-era macOS image, and it
is the single most important reason not to follow the legacy script literally.

**2. It never installs the bootstrap helper.** `internal/lifecycle/executor.go`
runs `/usr/local/libexec/tart-runner-fleet-bootstrap` inside the guest. That
path is a fleet contract that post-dates the script entirely, so the script
does not create it, and `/usr/local/libexec` does not exist in the source
image.

**3. Its safety interlocks belong to a machine that is not this one.** Before
building, the script takes a lock directory inside its own checkout, refuses to
proceed while any VM matching the shell manager's `vmPrefix` is running, and
walks every configured repository's GitHub Actions runs asserting no
self-hosted job is queued or in progress anywhere. On a node whose fleet daemon
is serving live jobs from a different control plane, the lock path does not
exist, the prefix does not match, and the GitHub assertion would refuse to
build for reasons that have nothing to do with this host. **Drop all three.**
Building a base is a clone-and-boot of an unrelated VM; it does not touch the
daemon, its state, or its guests. Keep only the resource preconditions —
free disk and available memory — because those are real and shared.

## What the daemon requires of a Linux guest

Every item is a code contract, not a preference. The file is the citation.

| Requirement | Source |
| --- | --- |
| `$HOME/actions-runner/run.sh`, an executable **regular file**, not a symlink | `cmd/tart-runner-fleet-bootstrap/main.go`, `internal/guestbootstrap/bootstrap.go` |
| `$HOME/actions-runner` a real directory (the helper's `WorkDir`, and it creates `.tart-runner-fleet/` at `0700` inside it) | `internal/guestbootstrap/bootstrap.go` |
| No `.runner`, `.credentials`, or `_work` in that directory | registration state must stay unique to each ephemeral clone |
| `/usr/local/libexec/tart-runner-fleet-bootstrap`, executable, from the release the node runs | `internal/lifecycle/executor.go:25` |
| `/bin/sh`, `/usr/bin/sudo`, `/sbin/shutdown`, `/usr/bin/systemd-run` — all executable | `internal/guestbootstrap/process.go` `paths()` |
| Passwordless `sudo -n /sbin/shutdown -h now` for the runner user | [ADR 0010](adr/0010-ephemeral-guest-shutdown.md); the supervisor powers the guest off when the runner exits |
| The runner user may create a **transient systemd scope** | `systemd-run --scope` in `process.go`; see the polkit step below |
| Everything a job needs resolves on `/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin` | `runnerToolchainPath("linux")` |
| Locale is `C.UTF-8` | `runnerLocale("linux")` |
| Root filesystem at least the profile's `diskGb` floor (50 GB by default) | [ADR 0006](adr/0006-per-profile-disk-floors.md) |

### The daemon replaces PATH outright

As on macOS, the runner process does not inherit the guest's login `PATH`.
`childEnvironmentForOS` strips `PATH`, `LANG`, `LC_ALL`, and the two Android
variables from the parent environment and substitutes fixed values. On Linux
the substituted `PATH` is exactly:

```
/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
```

No `/opt`, no `~/.local/bin`, no shell profile, no `nvm`. This is why the
recipe installs Node from NodeSource — which lands in `/usr/bin` — rather than
from a version manager. The macOS recipe needs explicit `/usr/local/bin`
symlinks for the same reason; on Linux, apt already puts everything in the
right place, and no symlink is required.

Unlike macOS, Linux guests get **no** `ANDROID_HOME` or `ANDROID_SDK_ROOT`:
`childEnvironmentForOS` adds those only when `goos == "darwin"`. A Linux job
that needs an Android SDK must set the variable itself.

### `systemd-run --scope` is the requirement nobody would guess

[ADR 0010](adr/0010-ephemeral-guest-shutdown.md) says the helper "starts
`run.sh` beneath a fixed detached supervisor". On Linux specifically,
`internal/guestbootstrap/process.go` first wraps that supervisor in

```
/usr/bin/systemd-run --scope --collect --quiet --unit=tart-runner-fleet-runner -- /bin/sh -c …
```

so the runner lives in a transient scope outside the Tart guest-agent's exec
cgroup and survives the short-lived helper. `systemd-run --scope` without
`--user` addresses the **system** manager, and polkit denies an unprivileged
caller with no login session. On a stock Cirrus Ubuntu guest the exact
observed failure is:

```
Failed to start transient scope unit: Interactive authentication required.
```

The helper does not report this. `exec.Command(...).Start()` succeeds — the
process was spawned — and the failure only surfaces as the supervisor never
writing its ready file, so the helper kills it and returns `ErrStart`. The
operator sees a `bootstrap` stage failure with no cause. **Every Linux job on
an image without the polkit rule below fails this way**, and the image looks
perfect from the outside.

## Build the image

Run these on the node that will use the image. Nothing here contacts another
node, and nothing here touches the running fleet daemon.

### 0. Preconditions

- Tart installed. Over a non-interactive SSH session `/opt/homebrew/bin` is
  not on `PATH`; use absolute paths for `tart`, and note that `jq` is
  `/usr/bin/jq` on macOS 15 while `gh` may not exist at all.
- At least **60 GB free** — the fleet's own `minFreeDiskGb` guard — plus room
  for the build. The image itself is small (see
  [Size and time expectations](#size-and-time-expectations)).
- Enough spare CPU and memory for a modest build guest. This build ran 4 vCPU /
  8192 MiB on a 14-core Mac Studio whose fleet budget is 6 vCPU / 16 GiB, while
  the daemon served macOS jobs; host load stayed under 4.5 throughout.
- **Do not** stop, restart, or reconfigure the fleet daemon, and do not assert
  anything about GitHub's queue. Neither is a precondition for cloning an
  unrelated VM.

### 1. Pull the pinned ancestor

```sh
TART=/opt/homebrew/bin/tart
IMAGE=ghcr.io/cirruslabs/ubuntu@sha256:ea337c9f1c3935929c04dc1de370425f65174a14f50087a192354a9dc3cfb521
BUILD="linux-runner-base-go-build-$(date -u +%Y%m%d%H%M%S)"

"$TART" pull "$IMAGE"
"$TART" clone "$IMAGE" "$BUILD"
```

That digest was `:latest` and `:24.04` — the same manifest — on 2026-08-04. It
resolves to Ubuntu 24.04.4 LTS, arm64, kernel 7.0.0-28-generic, default user
`admin` with passwordless sudo, `/dev/vda1` on a 20 GB disk. The legacy script
uses the floating `:latest`; pin the digest instead, for the reason
`BASE_IMAGE.md` gives — a base rebuilt months later from a moved tag is a
different image with the same name, and nothing says so.

Build under a **dated candidate name** and promote by rename at the end
([ADR 0011](adr/0011-atomic-production-updates.md)). Never provision a VM the
configuration already points at: a half-built base is worse than no base,
because the daemon will happily clone it.

### 2. Size it, including the disk, and boot it

```sh
"$TART" set "$BUILD" --cpu 4 --memory 8192 --disk-size 50
"$TART" run "$BUILD" --nested --no-graphics --no-audio --no-clipboard \
  >"$HOME/linux-base-build/$BUILD.log" 2>&1 &
until "$TART" exec "$BUILD" true 2>/dev/null; do sleep 2; done
```

CPU and memory here affect only how fast the build runs; the daemon issues its
own `tart set` against every clone before boot. **The disk is different.** ADR
0006 requires a positive `diskGb` floor on every Linux profile, the adapter
grows a clone that is under it, and Tart cannot shrink. Setting the floor on
the base once means no clone ever pays a resize. Measured: the guest's
`cloud-init`/`growpart` expanded `/dev/vda1` to 45 GB usable on first boot with
no intervention, which is what ADR 0006 assumes when it says "most cloud-ready
Linux images grow their root filesystem on boot" — this confirms it for this
image.

`--nested` matches what the adapter passes for Linux guests
(`internal/adapters/tart/adapter.go`), so the build guest is configured the way
production clones will be. It requires M3 or newer silicon.

### 3. Provision

This is the legacy script's payload, unchanged except where noted.

```sh
RUNNER_VERSION=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest |
  jq -r '.tag_name | ltrimstr("v")')

"$TART" exec "$BUILD" env RUNNER_VERSION="$RUNNER_VERSION" SOURCE_IMAGE="$IMAGE" \
  PLAYWRIGHT_VERSION=1.60.0 bash -lc '
set -euxo pipefail

sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  bash build-essential ca-certificates curl gh git gnupg jq libicu-dev \
  openjdk-17-jre-headless tar unzip xz-utils wget zip

# Node 22 from NodeSource. It installs into /usr/bin, which is on the daemon
# runner PATH; a version manager would not be.
sudo install -d -m 0755 /etc/apt/keyrings
sudo rm -f /etc/apt/keyrings/nodesource.gpg /etc/apt/sources.list.d/nodesource.list
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key |
  sudo gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
printf "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main\n" |
  sudo tee /etc/apt/sources.list.d/nodesource.list >/dev/null
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nodejs
sudo corepack enable --install-directory /usr/bin
for version in 4.12.0 4.16.0 4.17.0; do
  sudo corepack prepare "yarn@$version" --activate
done

# Browser archives stay workflow-versioned in actions/cache; their stable
# system packages belong in the base instead of being reinstalled by every
# short-lived Playwright shard.
npx --yes "playwright@$PLAYWRIGHT_VERSION" install-deps chromium webkit

# The runner payload, at the one path internal/guestbootstrap will start.
rm -rf "$HOME/actions-runner"
mkdir -p "$HOME/actions-runner"
curl --retry 3 --retry-all-errors -fsSL -o /tmp/actions-runner.tar.gz \
  "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-arm64-${RUNNER_VERSION}.tar.gz"
tar xzf /tmp/actions-runner.tar.gz -C "$HOME/actions-runner"
rm /tmp/actions-runner.tar.gz
test -x "$HOME/actions-runner/run.sh"
test ! -e "$HOME/actions-runner/.runner"
test ! -e "$HOME/actions-runner/.credentials"

{
  printf "built_at=%s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf "source_image=%s\n" "$SOURCE_IMAGE"
  printf "runner_version=%s\n" "$RUNNER_VERSION"
  printf "yarn_versions=4.12.0,4.16.0,4.17.0\n"
  printf "node_version=%s\n" "$(node --version)"
  printf "npm_version=%s\n" "$(npm --version)"
  printf "playwright_os_dependencies_version=%s\n" "$PLAYWRIGHT_VERSION"
} >"$HOME/.ci-base-manifest"

sudo apt-get clean
sudo rm -rf /var/lib/apt/lists/*
'
```

Notes from the real run:

- The legacy script's `gh` entry works: Ubuntu noble carries `gh 2.45.0-1ubuntu0.3`
  in `universe`. A distribution that does not needs the upstream
  `cli.github.com` repository added first.
- `openjdk-17-jre-headless` resolves to `17.0.19`, and `java` lands in
  `/usr/bin` — on the daemon runner PATH.
- Node resolves to `v22.23.2` with `npm 10.9.8`. `yarn --version` outside a
  project reports `1.22.22`, because `corepack`'s shim falls back to the
  classic line without a `packageManager` field. That is expected; the three
  prepared Berry versions activate per project.
- `install-deps chromium webkit` is the long step. It pulls in roughly 230
  packages, including the GTK, GStreamer, and WebKit stacks. No display
  manager is installed and the default systemd target does not change.

### 4. Let the runner user create a transient systemd scope

**Without this the image is silently broken.** See
[the section above](#systemd-run---scope-is-the-requirement-nobody-would-guess)
for why the failure is invisible.

```sh
"$TART" exec "$BUILD" bash -lc '
set -euxo pipefail
runner_user="$(id -un)"
sudo tee /etc/polkit-1/rules.d/49-tart-runner-fleet.rules >/dev/null <<RULE
polkit.addRule(function (action, subject) {
  if (action.id == "org.freedesktop.systemd1.manage-units" &&
      subject.user == "$runner_user") {
    return polkit.Result.YES;
  }
});
RULE
sudo chmod 0644 /etc/polkit-1/rules.d/49-tart-runner-fleet.rules
sudo systemctl restart polkit
systemd-run --scope --collect --quiet --unit=tart-runner-fleet-probe -- /bin/true
'
```

The rule is scoped to one action and one user, not a blanket grant. Ubuntu
24.04 ships `polkitd 124`, whose JavaScript rules engine reads
`/etc/polkit-1/rules.d`; a distribution still on the deprecated `.pkla`
local-authority mechanism needs the equivalent expressed there instead.

Do not substitute `sudo systemd-run` — the supervisor's whole purpose is to own
the runner as the runner's user, and the helper's argument vector is fixed.

### 5. Install the bootstrap helper

From the same release the node runs. The released asset name carries the
architecture: `tart-runner-fleet-bootstrap-linux-arm64`. See
[`INSTALL.md`](../INSTALL.md) for downloading and verifying that release.

```sh
base64 < "$RELEASE_DIR/tart-runner-fleet-bootstrap-linux-arm64" |
  "$TART" exec -i "$BUILD" bash -lc '
set -euo pipefail
base64 -d > /tmp/bootstrap
# /usr/local/libexec does not exist in the source image.
sudo mkdir -p /usr/local/libexec
sudo install -m 0755 -o root -g root /tmp/bootstrap \
  /usr/local/libexec/tart-runner-fleet-bootstrap
rm /tmp/bootstrap
sha256sum /usr/local/libexec/tart-runner-fleet-bootstrap
'
```

Compare that digest with `shasum -a 256` of the host-side asset. The macOS
recipe uses `-g wheel`; on Linux the root group is `root`.

## Seal and verify

`tart exec` runs with a `PATH` that has no `/sbin`, so `shutdown` must be
called by absolute path, and the exit status of a command that kills its own
transport is not evidence. Both hazards are the same ones
[`BASE_IMAGE.md`](BASE_IMAGE.md) documents for macOS, and both apply here.

```sh
seal() {
  "$TART" exec "$1" sync || true
  "$TART" exec "$1" sudo /sbin/shutdown -h now || true
  until [ "$("$TART" list --format json |
    jq -r --arg n "$1" '[.[] | select(.Name == $n and .Running)] | length')" = "0" ]; do
    sleep 2
  done
}
seal "$BUILD"
```

### Remove the stale control socket before every reboot

This is a Linux-guest hazard the macOS document does not have, and it cost an
hour of this build. After `tart run` exits, `~/.tart/vms/<name>/control.sock`
is **left behind**. The next `tart run` does not re-bind it, so the guest boots
normally — it gets a DHCP lease, systemd reaches `running`,
`tart-guest-agent.service` is `active` and logging `running RPC server on
AF_VSOCK port 8080` — while every `tart exec` on the host fails with:

```
Failed to connect to the VM using its control socket:
The operation couldn't be completed. (GRPCConnectionPoolError error 1.),
is the Tart Guest Agent running?
```

The message points at the guest, and the guest is fine. Delete the socket
before each boot after the first:

```sh
rm -f "$HOME/.tart/vms/$BUILD/control.sock"
"$TART" run "$BUILD" --nested --no-graphics --no-audio --no-clipboard \
  >"$HOME/linux-base-build/$BUILD-verify.log" 2>&1 &
until "$TART" exec "$BUILD" true 2>/dev/null; do sleep 2; done
```

Production never hits this — the fleet deletes each ephemeral clone rather than
rebooting it — so it is purely a maintenance-time trap. Delete the socket from
the promoted base's directory too, so the first clone starts from a clean one.

### Verify from inside, after the reboot

A reboot check is what catches provisioning that only worked in the session
that applied it. Assert the contracts, not the installer's opinion:

```sh
"$TART" exec "$BUILD" bash -lc '
set -euo pipefail
test -f "$HOME/actions-runner/run.sh" && test -x "$HOME/actions-runner/run.sh"
test ! -L "$HOME/actions-runner/run.sh"
test ! -e "$HOME/actions-runner/.runner"
test ! -e "$HOME/actions-runner/.credentials"
test ! -e "$HOME/actions-runner/_work"
test -x /usr/local/libexec/tart-runner-fleet-bootstrap
for path in /bin/sh /usr/bin/sudo /sbin/shutdown /usr/bin/systemd-run; do test -x "$path"; done
sudo -n /sbin/shutdown --help >/dev/null
systemd-run --scope --collect --quiet --unit=tart-runner-fleet-probe -- /bin/true

# Everything a job needs, on the daemon PATH and nothing else.
env -i HOME="$HOME" PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin \
  LANG=C.UTF-8 LC_ALL=C.UTF-8 sh -c '"'"'
for tool in node npm npx yarn corepack git jq gh java python3 curl tar unzip zip make gcc sudo shutdown; do
  command -v "$tool" >/dev/null || { echo "MISSING $tool"; exit 1; }
done
node --version; npm --version'"'"'

# The runner ships native binaries; prove they are linked.
for binary in "$HOME"/actions-runner/bin/Runner.Listener "$HOME"/actions-runner/bin/Runner.Worker; do
  test "$(ldd "$binary" | grep -c "not found")" = "0"
done
cat "$HOME/.ci-base-manifest"
'
```

Do **not** use `bin/installdependencies.sh` as the check. It is an installer:
it exits non-zero with `Need to run with sudo privilege` whatever the guest
actually contains, so it reports a failure on a perfectly good image and would
report success on a bad one if run with `sudo`. `ldd` is the honest probe.

Measured on the built image, all of the above passed, with:

```
node      /usr/bin/node        v22.23.2
npm       /usr/bin/npm         10.9.8
java      /usr/bin/java        openjdk 17.0.19
gh        /usr/bin/gh
/bin/sh        -> /usr/bin/dash
/sbin/shutdown -> /usr/bin/systemctl
Runner.Listener  0 unresolved shared libraries
Runner.Worker    0 unresolved shared libraries
```

### Then prove the whole path, end to end

The checks above are static. This one runs the production code:

```sh
printf 'bm90LWEtcmVhbC1qaXQtY29uZmln\n' |
  "$TART" exec -i "$BUILD" /usr/local/libexec/tart-runner-fleet-bootstrap
```

A deliberately invalid JIT configuration is enough. What must happen, and what
was observed: the helper exits `0` (it started and released the supervisor);
`systemd-run` places the supervisor in its transient scope; `run.sh` starts and
`Runner.Listener` rejects the configuration, logging to
`$HOME/actions-runner/.tart-runner-fleet/runner.log`:

```
Unexpected character encountered while parsing value: n. Path '', line 0, position 0.
Runner listener exit with terminated error, stop the service, no retry needed.
```

and the supervisor then runs `sudo -n /sbin/shutdown -h now`. **The guest
powered itself off 4 seconds later.** That single test covers the runner path,
the polkit rule, the supervisor, the sudo rule, and ADR 0010's poweroff
contract at once — everything this document added to the legacy script.

It is also destructive to the payload's pristineness. Boot once more (deleting
the control socket first) and clean up before sealing for the last time:

```sh
"$TART" exec "$BUILD" bash -lc '
set -euo pipefail
rm -rf "$HOME/actions-runner/.tart-runner-fleet" "$HOME/actions-runner/_diag" \
       "$HOME/actions-runner/_work" "$HOME/actions-runner/run-helper.sh"
rm -f "$HOME"/actions-runner/.tart-runner-fleet-supervisor-ready-*
rm -f "$HOME/.bash_history"
sudo journalctl --rotate >/dev/null 2>&1 || true
sudo journalctl --vacuum-time=1s >/dev/null 2>&1 || true
test ! -e "$HOME/actions-runner/.runner"
'
seal "$BUILD"
```

`run-helper.sh` is generated from `run-helper.sh.template` by `run.sh` on every
start and is byte-identical to it; removing it just keeps the payload exactly
as extracted.

### Promote by rename

```sh
rm -f "$HOME/.tart/vms/$BUILD/control.sock"
"$TART" rename "$BUILD" linux-runner-base-go
```

From here the base stays **stopped forever**. Maintenance repeats this whole
document against a new dated candidate and promotes by rename, keeping the
outgoing image as `linux-runner-base-go-pre-<change>-<yyyymmdd>`. Never
provision a live base in place.

## Deviations from the legacy script

Everything this recipe changes, and why. This is the list to re-check whenever
`create-linux-runner-base.sh` is consulted again.

| Legacy script | Here | Why |
| --- | --- | --- |
| Runner at `$HOME/actions-runner-cache/<version>/`, asserts `config.sh` | `$HOME/actions-runner/run.sh` | `cmd/tart-runner-fleet-bootstrap/main.go`. The daemon never calls `config.sh`. |
| No bootstrap helper | `/usr/local/libexec/tart-runner-fleet-bootstrap`, from the node's release | `internal/lifecycle/executor.go:25` |
| No polkit rule | `49-tart-runner-fleet.rules` for `manage-units` | `systemd-run --scope` in `internal/guestbootstrap/process.go`; fails invisibly without it |
| `mkdir "$ROOT_DIR/.maintenance.lock"` | dropped | The path belongs to a checkout that is not on this node. |
| Refuses to build while any `$VM_PREFIX-*` or Tartelet VM runs | dropped | Those prefixes are the shell manager's. Building clones an unrelated VM. |
| `assert_no_active_runner_work` across every configured repository | dropped | Would refuse to build because *another node* is busy. Not a property of this host. |
| `SOURCE_IMAGE=ghcr.io/cirruslabs/ubuntu:latest` | pinned by digest | A rebuild months later must be the same image. |
| Promotes over the live name, keeping `<base>-previous` | dated candidate, promote into an **absent** name | The name the config points at must never be a work in progress. ADR 0011. |
| `tart set --cpu --memory` only | adds `--disk-size 50` | ADR 0006's floor, paid once on the base instead of on every clone. |
| Manifest at `$HOME/actions-runner-cache/manifest` | `$HOME/.ci-base-manifest` | The cache directory no longer exists. |
| `gh api` for the runner version | `curl` + `jq` against the public API | `gh` may not exist, or be unauthenticated, on the building host. |
| `tart run --nested --no-graphics` | adds `--no-audio --no-clipboard` | Matches what `internal/adapters/tart/adapter.go` passes in production. |
| Verifies with `test -x .../config.sh` and `node --version` | verifies the fleet contracts, the daemon `PATH`, `ldd`, and one end-to-end helper run | The old checks pass on an image the daemon cannot use. |
| — | delete `control.sock` before every reboot | Otherwise `tart exec` fails against a perfectly healthy guest. |

One more, not a change but a confirmation: **the legacy script installs neither
Docker nor Podman, and neither is in the source image.** Nothing was added
here. A workflow that needs a container runtime inside the guest needs an
explicit layer that does not exist yet.

## Wire it to the node configuration

`baseVm` must equal the Tart name exactly, and the configuration is the source
of truth — not this document. The node this was built on has:

```json
{
  "baseVm": "linux-runner-base-go",
  "vmPrefix": "gha-linux",
  "hostBudget": { "cpu": 6, "memoryMb": 16384 },
  "linuxProfiles": [
    { "id": "linux-2x4", "label": "trf-linux-arm64-2x4", "aliases": ["linux-medium"],
      "cpu": 2, "memoryMb": 4096, "diskGb": 50 }
  ]
}
```

Read the node's actual `fleet.json` before building and name the candidate to
match. `BASE_IMAGE.md` records the same lesson from the macOS side, where the
committed `baseVm` disagreed with the document's suggested name.

Note that [`MULTI_NODE_PLAN.md`](MULTI_NODE_PLAN.md) assigns the Mac Studio
macOS `maestro` work only, with "no Linux scale sets". The deployed
configuration there has a full Linux profile matrix and this `baseVm`, so the
plan and the node have diverged. That is an open question for the plan, not
for this recipe: the image is correct for whatever the node's configuration
asks for.

## Size and time expectations

Measured on the Mac Studio, 2026-08-04.

| | Value |
| --- | ---: |
| Source image, pulled (`tart list` `Size`) | 5.2 GB |
| Built base, consumed on the APFS host (`du -sh`) | **5.4 GB** |
| Virtual disk capacity after `--disk-size 50` | 50 GB (45 GB usable in-guest) |
| In-guest root filesystem used | 4.7 GB of 45 GB |
| Wall-clock, pull to promote | about 55 minutes, including two false starts |

The image is **small** — under a tenth of the macOS base. That makes the
transfer-versus-build comparison a very different argument from the macOS one:

| | Transfer another node's image | Build locally |
| --- | --- | --- |
| Bytes | 8.6 GB of raw disk | 1.4 GB compressed from GitHub's CDN |
| Source | the other node's residential uplink | the building node's downlink |
| Realistic time | four attempts, each failed or silently truncated | about 55 minutes, most of it `apt` |
| Reproducible later | no | yes, the digest is pinned |

The four failed transfers that motivated this document are the point: an 8.6 GB
`rsync` over a home uplink through a tunnel died with `client_loop: send
disconnect: Broken pipe` and left a truncated `disk.img` that Tart still listed
as a VM. A registry push has the same upload bound. Even at this size, pulling
from a CDN and running `apt` beats pushing bytes uphill.

## Adapting this for node B (amd64, containers)

[`MULTI_NODE_PLAN.md`](MULTI_NODE_PLAN.md) gives node B a **rootless Podman**
executor, not Tart. The provisioning *content* below transfers; the delivery
does not.

Transfers unchanged:

- the apt package set, the NodeSource repository and Node 22, corepack and the
  three Yarn versions, and `playwright install-deps chromium webkit`;
- `$HOME/actions-runner/run.sh` as the only runner location;
- `/usr/local/libexec/tart-runner-fleet-bootstrap` — but the
  **`tart-runner-fleet-bootstrap-linux-amd64` asset**, not the arm64 one;
- the daemon `PATH` contract, which is architecture-independent.

Changes for amd64:

- the runner tarball is `actions-runner-linux-x64-<version>.tar.gz`;
- `dpkg --print-architecture` reports `amd64`, so any repository line
  templated on it follows automatically.

Changes for containers, which are the substantive ones:

- **`systemd-run` and the polkit rule do not apply.** An ordinary container has
  no systemd. `MULTI_NODE_PLAN.md` already flags this as the one item of
  Phase 2 that is image work rather than adapter work: the helper must launch
  the runner and detach **without** `systemd-run`. Until that lands,
  `internal/guestbootstrap/process.go`'s Linux default of
  `/usr/bin/systemd-run` has to be made absent — `SystemdRunPath` is already a
  `*string` for exactly this reason, and the launcher skips the wrapper when it
  is empty.
- **`sudo -n /sbin/shutdown -h now` has no meaning in a container.** ADR 0010's
  poweroff is a VM contract; the container equivalent is the supervisor exiting
  and the runtime reaping the container.
- ADR 0006's `diskGb` floor is a Tart concept. A container's writable layer is
  bounded differently.
- The image is built with a `Containerfile` and pinned by digest in a registry,
  not by `tart clone` and rename.

Node B additionally needs the Android SDK/NDK, the emulator, `platform-tools`,
and Maestro, per `MULTI_NODE_PLAN.md`. None of that is in this recipe, because
no arm64 Linux consumer uses it.

## Known unknowns

Stated so a future operator does not mistake inference for measurement.

- **The legacy script has never been executed by this fleet's operator on this
  node, and this document does not claim it ever produced the incumbent
  `linux-runner-base-go` on the other node.** That image's real provenance is
  unrecovered; only the script survives. Everything above is the script's
  content plus what the Go daemon's code requires, verified by execution — not
  a reconstruction of another node's image.
- **`gh 2.45.0` is what Ubuntu noble ships**, considerably behind upstream. No
  consumer workflow was audited for a `gh` feature newer than that. If one
  breaks, add the `cli.github.com` repository rather than pinning backwards.
- **The Playwright system dependencies are for `chromium` and `webkit` only.**
  `firefox` was not requested by the legacy script and is not installed.
- **No container runtime is present**, as noted above. Whether any arm64 Linux
  consumer needs one was not audited.
- **Why `tart run` does not re-bind a leftover `control.sock` was not traced
  into Tart's source.** The behaviour is reproducible and the workaround is
  reliable, but the mechanism is inferred from the symptom.
- **The end-to-end helper test used an invalid JIT configuration**, so it
  proves the launch, supervision, and poweroff path but not a real job. The
  first real job on this base is still the first real job.
- **Build-time CPU and memory were not tuned.** 4 vCPU / 8192 MiB was chosen to
  stay well inside a shared host's headroom, not because it is optimal.
