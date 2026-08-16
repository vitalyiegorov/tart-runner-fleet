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
- [What the *label* requires of a Linux guest](#what-the-label-requires-of-a-linux-guest)
- [Build the image](#build-the-image)
- [Seal and verify](#seal-and-verify)
- [Deviations from the legacy script](#deviations-from-the-legacy-script)
- [Wire it to the node configuration](#wire-it-to-the-node-configuration)
- [Size and time expectations](#size-and-time-expectations)
- [Adapting this for geekom (amd64, containers)](#adapting-this-for-geekom-amd64-containers)
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

## What the *label* requires of a Linux guest

Everything above is what the daemon needs to start a runner at all. It is not
the whole contract, and the part it leaves out caused a production failure on
2026-08-04 that is worth recording in full, because the recipe as first written
is what caused it.

[ADR 0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md), as amended, lets
two nodes advertise **the same runner label** and leaves the placement to
GitHub. A consumer names `[self-hosted, linux-tiered, linux-xl]` and GitHub
sends the job to whichever node has room. The consumer cannot see which node it
landed on, cannot ask, and has no way to express a preference.

That makes a label a promise about the *guest*, not only about the host that
serves it. Two images behind one label must be capability-equivalent, or the
label lies for exactly the fraction of jobs that land on the leaner image.

### The failure

mac-studio's Linux base was built from this document. mac-mini's incumbent base — the
one with unrecovered provenance recorded under
[Known unknowns](#known-unknowns) — additionally carries a **prewarmed Redroid
container**, which `vitalyiegorov/suuudokuuu` needs to run Maestro against
Android. This document did not know that, because it reconstructed the recipe
from the legacy script and the daemon's code, and neither mentions Redroid. It
even states the absence as a confirmed finding: "the legacy script installs
neither Docker nor Podman".

Both nodes then advertised `linux-xl`. `.github/workflows/android-maestro.yml`
opens with its own guard:

```sh
manifest="$HOME/.sudoku-ci/android-emulator.json"
test -f "$manifest" || {
  echo '::error::The guest image lacks the Redroid prewarm; rebuild the base with prewarm-redroid-linux.sh.'
  exit 1
}
```

The result was a job that failed **deterministically on mac-studio and passed
deterministically on mac-mini**, which presents as flakiness: same commit, same
label, same workflow, different outcome, no visible cause. The consumer's guard
is the only reason the cause was legible at all; without it the failure would
have surfaced as a missing `docker` binary somewhere further down.

The mitigation was to remove mac-studio's `linux-xl` scale sets from its
configuration, so the label was again advertised by one node. **That is a
capacity loss taken to restore correctness**, and it is the right emergency
action, but the durable fix is the image.

### The rule

> A node must not advertise a label unless its base image provides every
> capability the consumers of that label depend on. A leaner image behind a
> shared label is not an optimization; it is a correctness bug that presents as
> flakiness.

The invariant is stated in ADR 0034's amendment *"a shared label is a promise
about the guest"*. When this document was first written the enforcement was the
document itself: **any node that advertises `linux-xl` runs
[step 6](#6-prewarm-redroid-on-any-node-that-advertises-linux-xl)**. That rule
still stands, and since issue #202 it is also mechanical — see
[Which labels carry which extra capability, today](#which-labels-carry-which-extra-capability-today)
for how it is written down and
[Seal and verify](#seal-and-verify) for the manifest the image carries so it can
be held to it.

### The capability vocabulary

A capability is a lowercase identifier matching
`^[a-z0-9][a-z0-9-]*$` that does not end in a hyphen. It names something a base image provides that
neither the profile's resource vector nor the canonical label can state. The
vocabulary is not open: it is the result of an audit of the live fleet, and a
name that is not in one of these two tables is a name no image has been audited
for. Adding one means auditing an image, not inventing a word.

| Linux capability | What it means |
| --- | --- |
| `container-runtime` | `docker` on the daemon runner PATH, with a working daemon |
| `redroid-android` | the Redroid image present offline, a prewarmed `/data` volume, and `adb` — [step 6](#6-prewarm-redroid-on-any-node-that-advertises-linux-xl) |
| `android-build-sdk` | an Android SDK and NDK a job can build an APK against |
| `jdk` | a JDK on PATH, with `JAVA_HOME` resolvable |
| `node-runtime` | `node`, `npm`, `npx`, `yarn`, and `corepack` on the daemon runner PATH |
| `playwright-system-deps` | the stable system packages Playwright browsers link against |

The macOS vocabulary is in [`BASE_IMAGE.md`](BASE_IMAGE.md); the two are
separate because a node's two base images answer separate questions.

### Which labels carry which extra capability, today

| Label | Extra capability | Declared as | Consumer |
| --- | --- | --- | --- |
| `linux-xl` (`trf-linux-arm64-6x12`) | Docker, the Redroid image, a prewarmed `/data`, and `adb` | `container-runtime`, `redroid-android` | `suuudokuuu` Android Maestro |
| every other Linux label | none beyond the daemon contract | — | — |

This table is a snapshot of a fact that lives in consumer repositories, not
here. Re-derive it before adding a node: search the scopes in the node's
`fleet.json` for workflows that assert something about the guest, which by
convention is a guard in the first step of the job.

The third column is what the node's configuration now says out loud. The image
declares what it provides and the scale set declares what its labels require:

```json
"baseVm": "linux-runner-base-go",
"baseImageCapabilities": ["container-runtime", "redroid-android"]
```

```json
{ "profile": "linux-6x12", "name": "trf-sudoku-xl-studio",
  "labels": ["self-hosted", "linux-tiered", "linux-xl"],
  "requiresCapabilities": ["container-runtime", "redroid-android"] }
```

`fleet config validate` refuses the second without the first. Given more than
one node's configuration it also compares them, which is the check that would
have caught 2026-08-04 before a job ever ran:

```sh
fleet config validate config/nodes/*.json
```

See [`CLI.md`](CLI.md) and [`config/nodes/README.md`](../config/nodes/README.md).
A declaration is still a claim an operator types; what makes the image
answerable for it is the manifest below.

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

### 4b. Bound a kernel panic, and give it somewhere to land

**A job granted `--privileged` can panic this guest's kernel at will, and the
fleet cannot take that away while granting privileged at all.** On 2026-08-16 a
redroid container did exactly that: it could not open `/dev/binder`, Android
`init` shut its "device" down, and 300 seconds later `init`'s reboot watchdog —
which can never succeed inside a container — escalated through
`/proc/sysrq-trigger` to `c`. The guest kernel panicked with
`Kernel panic - not syncing: sysrq triggered crash`.

`kernel.panic` is `0` on this image, so the panicked kernel hung **forever**.
`tart list` went on reporting the VM `running`, the `tart run` process idled at
0.0% CPU, and eight production runners each held 6 vCPU / 12 GiB until GitHub's
own grace timer failed their jobs sixteen to eighteen minutes later. Nothing was
captured, because no userspace ran again. (Issue #236,
[ADR 0040](adr/0040-a-guest-that-stopped-answering-is-not-running.md).)

Two settings, both measured on a probe VM rather than reasoned about:

```sh
"$TART" exec "$BUILD" bash -lc '
set -euxo pipefail
sudo tee /etc/sysctl.d/60-tart-runner-fleet-panic.conf >/dev/null <<CONF
# A panicked guest reboots instead of hanging. Measured: the identical failing
# arm panicked at +305s, printed "Rebooting in 10 seconds..", and the guest was
# unreachable for 14 seconds before coming back. One sysctl turns a VM the host
# reports as running forever into a bounded reboot the control plane can see.
kernel.panic = 10
# An oops that would otherwise limp on becomes a panic, and therefore a reboot.
kernel.panic_on_oops = 1
CONF
sudo chmod 0644 /etc/sysctl.d/60-tart-runner-fleet-panic.conf
sudo sysctl --system >/dev/null
test "$(sysctl -n kernel.panic)" = "10"
test "$(sysctl -n kernel.panic_on_oops)" = "1"
'
```

**Do not reach for `kernel.sysrq=0`.** This image already has it, from Ubuntu's
own `/etc/sysctl.d/10-magic-sysrq.conf`, and the panic happened anyway: a write
to `/proc/sysrq-trigger` bypasses that sysctl by design, which governs only the
keyboard path. Masking the file is also unavailable — Docker 29 / runc refuses
`-v /dev/null:/proc/sysrq-trigger` with `cannot be mounted because it is inside
/proc`.

Then the console. The guest's cmdline names `console=tty1 console=ttyAMA0` while
the VM exposes `hvc*`, so **`tart run --serial` captures nothing** and the panic
above had to be read over netconsole. Name the console the VM actually offers:

```sh
"$TART" exec "$BUILD" bash -lc '
set -euxo pipefail
# hvc0 is what Apple Virtualization exposes to an arm64 Linux guest; ttyAMA0 is
# inherited from the upstream image and does not exist here.
sudo sed -i "s/console=ttyAMA0/console=hvc0/" /etc/default/grub
grep -q "console=hvc0" /etc/default/grub
sudo update-grub
'
```

If the source image ever stops carrying `console=ttyAMA0`, append
`console=hvc0` to `GRUB_CMDLINE_LINUX_DEFAULT` instead; the assertion above
fails loudly rather than silently doing nothing.

The host half is `linuxSerialLogDirectory` in `fleet.json`, which makes the
adapter pass `tart run --serial-path <dir>/<instance>.log`. It is **off by
default and unverified against this fleet's tart build** — check
`tart run --help` on the node before enabling it, because a flag tart does not
know fails every instance start. Verify after the reboot:

```sh
"$TART" exec "$BUILD" bash -lc 'grep -o "console=[^ ]*" /proc/cmdline'
# want: console=tty1 console=hvc0   (and no ttyAMA0)
```

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

### 6. Prewarm Redroid, on any node that advertises `linux-xl`

Skip this only if no scale set in the node's `fleet.json` advertises
`linux-xl`. If one does, this step is **mandatory** — see
[What the *label* requires of a Linux guest](#what-the-label-requires-of-a-linux-guest).

The provisioning is not written here, and must not be copied here. It is
`tests/app-tests/scripts/prewarm-redroid-linux.sh` in
`vitalyiegorov/suuudokuuu`, and the consumer owns it: it pins the Android
version, and the job that consumes it is the only thing that can say which
version works. Fetch it, record the digest you fetched, and run it unmodified.

```sh
gh api repos/vitalyiegorov/suuudokuuu/contents/tests/app-tests/scripts/prewarm-redroid-linux.sh \
  --jq '.content' | base64 -d > "$HOME/linux-base-build/prewarm-redroid-linux.sh"
shasum -a 256 "$HOME/linux-base-build/prewarm-redroid-linux.sh"

# Stage it inside the guest and run it from a file. See the warning below.
"$TART" exec -i "$BUILD" bash -c 'cat > /tmp/prewarm-redroid-linux.sh' \
  < "$HOME/linux-base-build/prewarm-redroid-linux.sh"
"$TART" exec "$BUILD" bash -lc 'sha256sum /tmp/prewarm-redroid-linux.sh'
"$TART" exec "$BUILD" bash -lc 'bash /tmp/prewarm-redroid-linux.sh </dev/null' </dev/null
```

What it bakes, and therefore what the image gains: `docker.io` and `adb` from
apt; `/etc/modules-load.d/redroid.conf` so `binder_linux` loads on every boot;
the pulled Redroid image; a `$HOME/redroid-data` volume that has completed one
Android boot with animations disabled; and the manifest at
`$HOME/.sudoku-ci/android-emulator.json` that the consumer's guard tests for.

Android runs as a **privileged container, not a nested VM**. Nothing here needs
`/dev/kvm` or nested virtualization inside the guest; `binder_linux` is a kernel
module and the container mounts binderfs itself. Ubuntu 24.04's
`7.0.0-28-generic` ships the module — confirm before running, because a kernel
without it fails the script's own `test -d /sys/module/binder_linux`:

```sh
"$TART" exec "$BUILD" modinfo binder_linux
```

**Do not pipe the script into `bash -s` over `tart exec -i`.** This cost the
build an hour. `bash` reading a script from a pipe reads it incrementally, and
the script's first background-daemon child — `adb`, here — inherits that pipe
and swallows the rest of the source. The observed result is the worst kind: the
run stops silently at `docker run`, leaves the container up, writes no manifest,
and **exits 0**. Stage the file inside the guest and run it by path, with
`</dev/null`, as above.

Then record what was baked, so the image can answer for itself later. Nothing
reads this file; it is the provenance the next operator will want, and the seed
of the image-capability manifest ADR 0034's amendment proposes:

```sh
"$TART" exec "$BUILD" bash -lc '
set -euo pipefail
manifest="$HOME/.sudoku-ci/android-emulator.json"
{
  printf "redroid_image=%s\n" "$(python3 -c "import json;print(json.load(open(\"$manifest\"))[\"image\"])")"
  printf "redroid_image_digest=%s\n" "$(sudo docker image inspect \
    "$(python3 -c "import json;print(json.load(open(\"$manifest\"))[\"image\"])")" \
    --format "{{index .RepoDigests 0}}")"
  printf "redroid_prewarm_script_sha256=%s\n" "<the digest you fetched>"
  printf "redroid_prewarmed_at=%s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf "docker_version=%s\n" "$(sudo docker info --format "{{.ServerVersion}}")"
} >> "$HOME/.ci-base-manifest"
'
```

Measured on mac-studio, 2026-08-04: the whole step ran in under five minutes, of
which the 694 MB image pull was most; `docker.io` resolves to server 29.1.3 on
the `overlayfs` driver; the prewarmed volume is 32 MB; and the image grew from
4.7 GB to 7.1 GB in-guest, 5 GB to 8 GB as Tart reports it.

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
manifest at all: it converts a caught failure into the flaky one this whole
section exists to prevent. The list below is for an image that ran step 6; drop
`redroid-android` and `container-runtime` from an image that did not.

```sh
"$TART" exec "$BUILD" bash -lc '
set -euo pipefail
sudo install -d -o root -g root -m 0755 /usr/local/share/tart-runner-fleet
sudo tee /usr/local/share/tart-runner-fleet/image-capabilities.json >/dev/null <<JSON
{"schemaVersion": 1, "image": "linux-runner-base-go", "sealedAt": "'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",
 "capabilities": ["container-runtime", "jdk", "node-runtime", "playwright-system-deps", "redroid-android"]}
JSON
sudo chmod 0644 /usr/local/share/tart-runner-fleet/image-capabilities.json
python3 -m json.tool /usr/local/share/tart-runner-fleet/image-capabilities.json >/dev/null
'
```

The identifiers come from [the capability vocabulary](#the-capability-vocabulary)
and must match the node's `baseImageCapabilities` exactly. They are compared for
string equality across two machines' configuration files, so a spelling that
differs is a capability that does not exist.

### Then stop the guest

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
test -r /usr/local/share/tart-runner-fleet/image-capabilities.json
python3 -m json.tool /usr/local/share/tart-runner-fleet/image-capabilities.json >/dev/null
for path in /bin/sh /usr/bin/sudo /sbin/shutdown /usr/bin/systemd-run; do test -x "$path"; done

# A panicked guest must reboot rather than hang, and the panic must have a
# console the VM actually exposes. Both are step 4b, and both are exactly the
# kind of provisioning that only worked in the session that applied it.
test "$(sysctl -n kernel.panic)" = "10"
test "$(sysctl -n kernel.panic_on_oops)" = "1"
grep -q "console=hvc0" /proc/cmdline
! grep -q "console=ttyAMA0" /proc/cmdline
sudo -n /sbin/shutdown --help >/dev/null
systemd-run --scope --collect --quiet --unit=tart-runner-fleet-probe -- /bin/true

# Everything a job needs, on the daemon PATH and nothing else.
env -i HOME="$HOME" PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin \
  LANG=C.UTF-8 LC_ALL=C.UTF-8 sh -c '"'"'
for tool in node npm npx yarn corepack git jq gh java python3 curl tar unzip zip make gcc sudo shutdown; do
  command -v "$tool" >/dev/null || { echo "MISSING $tool"; exit 1; }
done
# On a node that advertises linux-xl, add: docker adb
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

Two traps in writing these checks. `systemd-run --scope` is the honest polkit
probe, **not** `test -f /etc/polkit-1/rules.d/49-tart-runner-fleet.rules`:
`/etc/polkit-1/rules.d` is `0700 root:root`, so the runner user's `test -f`
returns false on a rule that is present and working. Worse, written as
`test -f … && echo present`, `set -e` will not stop on it — POSIX exempts every
command in an `AND-OR` list but the last — so the check *prints nothing and the
script continues*. Use `sudo test -f`, or trust the probe.

### Verify the Redroid prewarm, on a node that advertises `linux-xl`

Verify what the consumer's guard tests, then verify the thing the guard is a
proxy for. Both, after the reboot — a container runtime that only worked in the
session that installed it is exactly the failure a reboot check exists to catch:

```sh
"$TART" exec "$BUILD" bash -lc '
set -euo pipefail
# 1. Exactly what .github/workflows/android-maestro.yml asserts.
manifest="$HOME/.sudoku-ci/android-emulator.json"
test -f "$manifest"
image=$(python3 -c "import json; print(json.load(open(\"$manifest\"))[\"image\"])")
data=$(python3 -c "import json; print(json.load(open(\"$manifest\"))[\"dataDir\"])")
echo "REDROID_PREWARM_OK image=$image dataDir=$data"

# 2. The image is present offline, and the volume really completed a boot.
sudo docker image inspect "$image" --format "{{index .RepoDigests 0}}"
test -d "$data/data"

# 3. Both survived the reboot, which is the whole point of checking here.
systemctl is-active docker.service
test -d /sys/module/binder_linux

# 4. The production step itself: boot Android from the prewarmed volume.
sudo docker run -d --name redroid-verify --privileged \
  --memory 6g --memory-swap 6g --cpus 4 -v "$data":/data -p 5555:5555 \
  "$image" androidboot.redroid_gpu_mode=guest >/dev/null
adb connect localhost:5555 >/dev/null
deadline=$(( $(date +%s) + 300 ))
until [ "$(adb -s localhost:5555 shell getprop sys.boot_completed 2>/dev/null | tr -d "\r")" = "1" ]; do
  test "$(date +%s)" -lt "$deadline"; sleep 5
  adb connect localhost:5555 >/dev/null 2>&1 || true
done
adb -s localhost:5555 shell getprop ro.build.version.release
adb -s localhost:5555 shell getprop ro.product.cpu.abi
adb -s localhost:5555 shell settings get global window_animation_scale
adb kill-server; sudo docker rm -f redroid-verify
'
```

Measured on mac-studio, 2026-08-04, on the rebooted image:

```
REDROID_PREWARM_OK image=redroid/redroid:15.0.0_64only-latest dataDir=/home/admin/redroid-data
redroid/redroid@sha256:b51bde9cef80f7bd7581148192f2b2f4d41f23c6344cfe88eceeb8ddd67490ee
docker.service    active            29.1.3, storage driver overlayfs
binder_linux      loaded at boot    via /etc/modules-load.d/redroid.conf
ANDROID BOOT COMPLETED in 10s from the prewarmed volume
release           15
abi               arm64-v8a
animation scale   0
```

**Ten seconds is the measurement that justifies the step.** The prewarm is not
only about the packages being present; a first Android boot on a cold `/data`
is minutes of the job's 45-minute budget, and it is paid once here instead of on
every job. `arm64-v8a` is also the check that matters for the consumer, whose
APK must ship that ABI — and it is the first thing that changes on x86_64, see
[Adapting this for geekom](#adapting-this-for-geekom-amd64-containers).

Booting Android here writes to the base's `$HOME/redroid-data`. That is
acceptable and intended — it is the same second boot every ephemeral clone
performs — but it means this check belongs before the final cleanup pass, and
the container must be removed before sealing.

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
# On an image with the Redroid layer: no container may be baked in, only the
# image and the volume. Leaving one makes the consumer'"'"'s `docker run` fail on a
# name clash it did not create.
sudo docker rm -f redroid-verify redroid-prewarm >/dev/null 2>&1 || true
test "$(sudo docker ps -aq | wc -l)" = "0"
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
"$TART" rename linux-runner-base-go linux-runner-base-go-pre-<change>-<yyyymmdd>
"$TART" rename "$BUILD" linux-runner-base-go
```

Run the two renames as one command, with no operator decision between them:
between the first and the second, the name the daemon's `baseVm` points at does
not exist, and a clone issued in that window fails. Check `tart list` for a
running guest first and do it when the node is idle.

From here the base stays **stopped forever**. Never provision a live base in
place.

### Maintenance: rebuild, or extend the incumbent

Adding the Redroid layer to mac-studio used the second form, and it is worth naming
because the recipe above reads as if there were only the first:

- **Rebuild from the pinned ancestor.** Everything above, in order. Use it when
  the change is to a step this document owns, or when the incumbent's provenance
  is unknown, or on a schedule.
- **Extend the incumbent.** `tart clone linux-runner-base-go <dated candidate>`,
  apply only the new step, verify **everything** — not just the new step — and
  promote by rename. About 20 minutes against 55, and the clone is copy-on-write
  so it costs almost no disk until it is written to.

The second form inherits every property of the incumbent, including anything it
got wrong and anything about it that was never recorded. That is the trade: it
is fast because it does not rebuild, and it is unreproducible for the same
reason. It is the right choice for an urgent capability gap on an image whose
provenance is already documented, and the wrong one as a habit. Either way
`$HOME/.ci-base-manifest` is what the next operator reads, so append to it.

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
| — | `kernel.panic=10`, `kernel.panic_on_oops=1` | A privileged container can panic this kernel, and `kernel.panic=0` makes that an unbounded hang the host reports as `running`. Issue #236, [ADR 0040](adr/0040-a-guest-that-stopped-answering-is-not-running.md). |
| — | `console=hvc0` on the cmdline | The inherited `console=ttyAMA0` names a device the VM does not expose, so `tart run --serial` captures nothing and a panic leaves no trace. |

One more, not a change but a confirmation: **the legacy script installs neither
Docker nor Podman, and neither is in the source image.**

That sentence used to end "a workflow that needs a container runtime inside the
guest needs an explicit layer that does not exist yet", and it was wrong within
the day — not about the script, but about the fleet. `suuudokuuu` already needed
one, mac-mini already had one, and this document did not know. Step 6 is that
layer, and the reason it is written as a mandatory step rather than an optional
extra is
[What the *label* requires of a Linux guest](#what-the-label-requires-of-a-linux-guest).
The general lesson survives the correction: **the legacy script is not a
statement about what the fleet's consumers need**, only about what one control
plane once installed. Derive consumer requirements from the consumers.

## Wire it to the node configuration

`baseVm` must equal the Tart name exactly, and the configuration is the source
of truth — not this document. The node this was built on has:

```json
{
  "baseVm": "linux-runner-base-go",
  "baseImageCapabilities": ["container-runtime", "jdk", "node-runtime", "playwright-system-deps", "redroid-android"],
  "vmPrefix": "gha-linux",
  "hostBudget": { "cpu": 6, "memoryMb": 16384 },
  "linuxProfiles": [
    { "id": "linux-2x4", "label": "trf-linux-arm64-2x4", "aliases": ["linux-medium"],
      "cpu": 2, "memoryMb": 4096, "diskGb": 50 }
  ]
}
```

`baseImageCapabilities` must equal what the manifest inside the image says, for
the same reason `baseVm` must equal the Tart name: one of them is checked against
the machine and the other is checked against every other node, and a
configuration that disagrees with its own image is the failure mode this whole
section is about.

Read the node's actual `fleet.json` before building and name the candidate to
match. `BASE_IMAGE.md` records the same lesson from the macOS side, where the
committed `baseVm` disagreed with the document's suggested name.

Read it for a second reason as well, which the first version of this document
did not: **the labels the node's scale sets advertise decide which optional
steps are mandatory.** The deployed mac-studio configuration has the full profile
matrix — `linux-1x2`, `linux-2x4`, `linux-4x8`, `linux-6x12` — and its `6x12`
scale sets advertise `[self-hosted, linux-tiered, linux-xl]`, the same labels
mac-mini's do. That is what made step 6 compulsory there.

An earlier version of this note recorded a divergence here: `MULTI_NODE_PLAN.md`
once assigned the Mac Studio macOS `maestro` work only, with "no Linux scale
sets", while the deployed configuration already carried a full Linux profile
matrix. That plan document is now corrected to match what is actually
deployed — mac-studio shares mac-mini's labels, Linux tiers included, for as
long as geekom is not yet delivered — so the divergence this paragraph used to
flag no longer exists. The general lesson still holds regardless: the image is
correct for whatever the node's configuration asks for, not for what a plan
document assumed it would ask for.

## Size and time expectations

Measured on the Mac Studio, 2026-08-04.

| | Value |
| --- | ---: |
| Source image, pulled (`tart list` `Size`) | 5.2 GB |
| Built base, consumed on the APFS host (`du -sh`) | **5.4 GB** |
| Virtual disk capacity after `--disk-size 50` | 50 GB (45 GB usable in-guest) |
| In-guest root filesystem used | 4.7 GB of 45 GB |
| Wall-clock, pull to promote | about 55 minutes, including two false starts |
| **With the Redroid layer of step 6**, `tart list` `Size` | 8 GB |
| **With the Redroid layer of step 6**, in-guest root used | 7.1 GB of 45 GB |
| Redroid layer, added to a cloned incumbent: wall-clock | about 20 minutes, three boots and all verification included |

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

## Adapting this for geekom (amd64, containers)

[`MULTI_NODE_PLAN.md`](MULTI_NODE_PLAN.md) gives geekom a **rootless Podman**
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

geekom additionally needs the Android SDK/NDK, the emulator, `platform-tools`,
and Maestro, per `MULTI_NODE_PLAN.md`. None of that is in this recipe, because
no arm64 Linux consumer uses it.

### The Redroid layer does *not* transfer to geekom unchanged

Step 6 is the part of this document most likely to be copied verbatim onto the
GEEKOM and the part least entitled to be. Everything below is stated as
something to **re-verify on the node**, not as a finding: none of it has been
measured on x86_64, and the arm64 result is not evidence about it.

- **The pinned image is an arm64 manifest.** The digest recorded in this
  document, `sha256:b51bde…90ee`, is what `redroid/redroid:15.0.0_64only-latest`
  resolved to on arm64. The same tag on amd64 resolves to a different manifest,
  and the tag itself is `-latest`: pin whatever amd64 actually resolves to,
  record it, and do not assume the Android version behaves the same. The arm64
  finding that Android 14 hard-locks the 6.17 guest kernel and 13 never boots is
  a *guest-kernel* interaction, so it must be re-tested against the GEEKOM's
  kernel rather than inherited.
- **The ABI question inverts, and it is the one that breaks the consumer.**
  The verification above measures `ro.product.cpu.abi = arm64-v8a`, and
  `suuudokuuu`'s prewarm script says in its own header that "apps must ship
  arm64-v8a". On x86_64 the container reports an x86 ABI, so an APK carrying
  only `arm64-v8a` native libraries either refuses to install or crashes on the
  first native call — and the app is React Native, so there are always native
  libraries. geekom therefore needs one of: an APK built with an x86_64 ABI
  split, a Redroid image variant that ships a native bridge (ARM translation),
  or the Google emulator path instead. **Which of those works is unmeasured.**
  It is also the one item here that changes a *consumer's build*, not just an
  image, so it is a conversation with the consumer before it is a provisioning
  step.
- **Two Android paths exist on geekom and must not be conflated.** Redroid is a
  privileged container over `binder_linux` and wants no `/dev/kvm` at all. The
  Google AVD path — which is why geekom exists, since
  `reactivecircus/android-emulator-runner` with `arch: x86_64` is failing on the
  Mac mini today — needs `/dev/kvm`, and the podman adapter grants it per
  profile through `executor.kvmProfiles`. A profile can need both, one, or
  neither, and the grant is not interchangeable with the module.
- **`binder_linux` is a property of the GEEKOM's kernel, not of x86.** Ubuntu
  ships it as a module under `kernel/drivers/android/`; a distribution that
  builds `CONFIG_ANDROID_BINDER_IPC` differently, or not at all, fails the
  prewarm script's own `test -d /sys/module/binder_linux`. Check `modinfo
  binder_linux` before anything else, because it is the cheapest possible
  disproof.
- **Privileged containers inside a rootless runner container are the real
  design problem.** On mac-mini the runner is a VM, so `docker run --privileged`
  inside it is ordinary. On geekom the runner *is* a rootless container, and
  `--privileged` nested inside it — with binderfs mounted and a port
  published — is not the same operation and may not be permitted at all. The
  plausible shapes are: run Redroid as a sibling on the node's own runtime and
  give the job an `adb` endpoint; or grant the runner container the specific
  capabilities and device access Redroid needs; or accept the Google AVD path
  instead. This is design work with a security argument attached — the
  `MULTI_NODE_PLAN.md` rationale for rootless is that approved third-party code
  runs on this node — and it should not be settled by a provisioning script.
- **The consumer's workflow hard-codes `sudo docker`.** geekom's runtime is
  podman, and rootless podman needs no `sudo`. Either the image provides a
  `docker` shim, or the workflow learns the runtime from the manifest that
  already exists at `$HOME/.sudoku-ci/android-emulator.json` — the manifest has
  a `type` field, and it is the natural place for the runtime to be named.

Until every one of those is answered on the hardware, **geekom must not
advertise `linux-xl`**, or it reproduces exactly the failure recorded in
[What the *label* requires of a Linux guest](#what-the-label-requires-of-a-linux-guest)
— on a third node, with a third set of symptoms. Advertise the canonical
`trf-linux-amd64-*` labels, which no Android consumer names, until the Android
path there is proved.

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
- ~~**No container runtime is present.** Whether any arm64 Linux consumer needs
  one was not audited.~~ **Corrected the same day, in production.** One did, the
  audit was the outage, and step 6 is the answer. The unaudited-consumer class
  of unknown is what the ADR 0034 amendment asks to be made mechanical; the
  entry is struck rather than deleted because the shape of the miss is the
  lesson.
- **Why `tart run` does not re-bind a leftover `control.sock` was not traced
  into Tart's source.** The behaviour is reproducible and the workaround is
  reliable, but the mechanism is inferred from the symptom.
- **The end-to-end helper test used an invalid JIT configuration**, so it
  proves the launch, supervision, and poweroff path but not a real job. The
  first real job on this base is still the first real job.
- **Build-time CPU and memory were not tuned.** 4 vCPU / 8192 MiB was chosen to
  stay well inside a shared host's headroom, not because it is optimal.
- **mac-mini's Redroid layer was read, not rebuilt, and the two images are
  equivalent only where they were compared.** The comparison was made against a
  *running clone* of mac-mini's base while it served a job — read-only, and the
  live base was never touched. What matched: the Redroid tag, the image digest
  `sha256:b51bde…90ee`, `/usr/bin/docker` and `/usr/bin/adb`,
  `/etc/modules-load.d/redroid.conf`, the runner user's `docker` group
  membership, and every field of `$HOME/.sudoku-ci/android-emulator.json`. What
  differs: mac-mini's base carries no `$HOME/.ci-base-manifest` at all, so its
  package set, Node version, and runner version are still unverified against
  mac-studio's. **The two images are proved equivalent for the `linux-xl` Android
  capability and for nothing else.** That is exactly the gap the enforcement
  proposal exists to close.
- **The prewarmed `/data` was 32 MB on mac-studio and 155 MB on the mac-mini clone
  that was read.** The mac-mini figure was taken from a guest that had already run
  a job, and job execution writes to `/data`, so the difference is expected and
  was not investigated further. It is recorded because a *base* whose `/data`
  is much larger than 32 MB would mean something was baked in that should not
  have been.
- **The `linux-xl` capability was proved in the base image, not in an ephemeral
  clone.** The daemon resizes and boots a clone with the profile's own vector,
  and the verification here ran in the base at 4 vCPU / 8192 MiB while the
  profile is 6 vCPU / 12 GiB. More resources, same layer — but the first real
  Android job on mac-studio is still the first real Android job.
