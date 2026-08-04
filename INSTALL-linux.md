# Install on a Linux node

This guide installs one immutable release on a Linux machine and brings it up in
**observe mode**, and then — if the machine has a rootless container runtime —
gives it an execution backend so it can serve runners. It is the Linux twin of
[`INSTALL.md`](INSTALL.md), and the two differ in exactly four places: the
directory layout, the service manager, how a release is activated, and the
execution technology, which on a Linux node is rootless Podman rather than
Tart.

Read [ADR 0034](docs/adr/0034-a-node-serves-the-scale-sets-it-owns.md) and
[`docs/MULTI_NODE_PLAN.md`](docs/MULTI_NODE_PLAN.md) first. Under ADR 0034 each
node is an independent fleet: it owns the scale sets named in its own
configuration, it never talks to another node, and GitHub's one-session-per-scale
-set rule is the only coordination there is.

## What a Linux node can and cannot do yet

| | Status |
| --- | --- |
| Run the daemon, measure the host, serve `status`/`doctor`/`queues`/`health`/`metrics` | **Yes.** The host probe reads `/proc` and `df`. |
| Observe mode | **Yes.** |
| Shadow, canary, authority | **Only with an `executor` block.** Without one the daemon refuses to start: those modes exist to act on a machine that has told it no execution technology. |
| Provision runners | **Yes, in containers.** One rootless, unprivileged, ephemeral Podman container per job, never reused. Configure it in step 4a. |
| Run macOS guests | **No, ever.** Apple's Virtualization framework is macOS-only. |
| `fleet update apply-latest` | **No.** The release transaction drives `launchctl` and `plutil`. It refuses with a message naming the gap; use the manual bridge below. |

Bring a node up in observe mode **first**, with no `executor` block and no
scopes. That is Part A of `docs/MULTI_NODE_PLAN.md`'s node B bring-up, and it is
deliberately a whole step: a node with no execution backend that reaches the
observe steady state is what proves the daemon, the configuration, the host
probe, and the packaging are right before any runner is at stake. Step 4a adds
the backend afterwards.

## 1. Prerequisites

- A 64-bit Linux machine with `systemd` and a per-user service manager;
- an unprivileged service account that owns the daemon, its state, and its
  credentials — never install any of this with `sudo`;
- lingering enabled for that account, so its units survive logout:
  `sudo loginctl enable-linger fleet`;
- an authenticated GitHub CLI (`gh auth status`) for release downloads;
- `coreutils` (`df`) and a readable `/proc`, which is every normal Linux.

Do not add a container runtime yet. The daemon has no backend to drive it with,
and issue #139 specifies the runtime and its configuration together.

## 2. Download and verify a production release

Every release publishes one archive per node type. The Linux node's is
`tart-runner-fleet-<version>-linux-amd64.tar.gz`, and it is listed in the same
`SHA256SUMS` as the macOS archive, so verifying one verifies the release.

```sh
set -eu
umask 077
REPOSITORY=owner/tart-runner-fleet
VERSION=$(gh release view --repo "$REPOSITORY" --json tagName --jq .tagName)
ROOT="${XDG_DATA_HOME:-$HOME/.local/share}/tart-runner-fleet"
DOWNLOAD=$(mktemp -d)
RELEASE_DIR="$ROOT/releases/$VERSION"

gh release download "$VERSION" --repo "$REPOSITORY" --dir "$DOWNLOAD" --pattern '*'
(cd "$DOWNLOAD" && sha256sum --check --ignore-missing SHA256SUMS)
mkdir -p "$RELEASE_DIR"
tar -xzf "$DOWNLOAD/tart-runner-fleet-$VERSION-linux-amd64.tar.gz" -C "$RELEASE_DIR"
install -m 0600 "$DOWNLOAD/SHA256SUMS" "$RELEASE_DIR/SHA256SUMS"
install -m 0600 "$DOWNLOAD/tart-runner-fleet-$VERSION-linux-amd64.tar.gz" \
  "$RELEASE_DIR/tart-runner-fleet-$VERSION-linux-amd64.tar.gz"
chmod 0700 "$RELEASE_DIR/fleet" "$RELEASE_DIR/render-systemd.sh"
"$RELEASE_DIR/fleet" version
```

The controller inside the archive is named `fleet`, exactly as on a macOS node;
the `-linux-amd64` suffix exists only on the loose release assets, where both
platforms' binaries have to coexist.

Keep the previous immutable release. It is the local rollback generation when
GitHub or the network is unavailable.

## 3. The directory layout

A Linux node mirrors the macOS layout under the XDG base directories, and the
daemon, the operator interface, and the renderer all derive it the same way:

| | macOS | Linux |
| --- | --- | --- |
| Immutable root | `~/Library/Application Support/tart-runner-fleet` | `$XDG_DATA_HOME/tart-runner-fleet` (default `~/.local/share/...`) |
| State, database, socket | `$ROOT/state` | `$ROOT/state` |
| Service definitions | `~/Library/LaunchAgents` | `$XDG_CONFIG_HOME/systemd/user` (default `~/.config/systemd/user`) |
| Service domain | `gui/<uid>` | the `systemd --user` manager |

## 4. Configure state and GitHub credentials

```sh
set -eu
ROOT="${XDG_DATA_HOME:-$HOME/.local/share}/tart-runner-fleet"
STATE_DIR="$ROOT/state"
mkdir -p "$STATE_DIR"
chmod 0700 "$ROOT" "$STATE_DIR"
install -m 0600 config/fleet.example.json "$STATE_DIR/fleet.json"
"$RELEASE_DIR/fleet" config validate "$STATE_DIR/fleet.json"
```

Bring the node up with **no `github.scopes` entries**. A node with no scopes
takes no demand, which is what makes an observe-mode bring-up risk-free: nothing
in GitHub can change because of it.

**Restate `hostBudget` for this machine, or remove it.** The checked-in example
declares the live Mac mini's envelope — ten cores and 23552 MiB — and a budget
is a claim about one specific machine. When the machine cannot honour it the
host observation fails closed, every tick's plan is blocked, and the daemon
never becomes ready; it reports the reason in `fleet status`, naming both
figures. That is the correct answer to an operator claiming capacity this node
does not have, and it is the first thing to check if step 6 does not go green.

```sh
# state this node's own envelope ...
"$RELEASE_DIR/fleet" config validate "$STATE_DIR/fleet.json"   # after editing hostBudget
# ... or omit the key entirely, which imposes no bound.
```

### 4a. The container executor

A Linux node with no `executor` block observes and nothing else. Adding the
block is what makes it able to provision, and it is the only difference between
the two stages of the bring-up.

```json
"executor": {
  "backend": "podman",
  "image": "ghcr.io/vitalyiegorov/trf-runner-amd64:2026-08",
  "binary": "/usr/bin/podman",
  "kvmProfiles": ["xl"],
  "holdCommand": ["sleep", "infinity"]
}
```

| Key | Meaning |
| --- | --- |
| `backend` | `podman` is the only value this release implements. Omit the whole block for an observe-only node. |
| `image` | The OCI reference every runner container is created from. It takes the place of `linux.baseVm`, which stays in the file because the schema requires it and means nothing on this node. Pin it by digest in production. |
| `binary` | Optional. Empty resolves `podman` through `PATH`, which is what a distribution package installs. |
| `kvmProfiles` | The profile IDs whose containers get `--device /dev/kvm`. Per ADR 0034 this is the Android emulator profile and nothing else; a profile named here that the node does not declare is a refused configuration. |
| `holdCommand` | Optional. What keeps a created container alive and idle until the JIT bootstrap runs inside it; the default is `sleep infinity`. |

The runtime must be **rootless**. The daemon shells `podman info` before it
starts in any mutating mode and refuses a runtime that is absent, unusable, or
running as root — approved third-party code executes in these containers, and a
root-owned container daemon or a `docker` group is root-equivalent. That refusal
is a startup error you read once, not a queue of parked jobs you read an hour
later.

```sh
sudo apt install podman uidmap slirp4netns fuse-overlayfs
podman info --format '{{.Host.Security.Rootless}}'   # must print: true
podman pull ghcr.io/vitalyiegorov/trf-runner-amd64:2026-08
```

The runner image must contain the JIT bootstrap helper at
`/usr/local/libexec/tart-runner-fleet-bootstrap` and a `sleep` for the hold
command. The daemon starts the runner by executing that helper inside the
container with the JIT configuration on standard input; the configuration never
appears in argv, in the environment, or in an image layer.

Prove the whole adapter against this machine's real podman before serving a job:

```sh
TRF_PODMAN_SMOKE=required ./scripts/podman-smoke.sh
```

On a node configured to execute jobs in containers, `SKIPPED` is not an answer.

### 4b. GitHub credentials

There is no Keychain on Linux, so the GitHub App private key is always a file:
set `github.app.privateKeyFile` to a `0600`, non-symlink, service-account-owned
path, and leave `keychainService` and `keychainAccount` unset. The daemon refuses
any other mode. Never put the PEM, JIT configuration, or a runner token in JSON,
argv, logs, or shell history.

```sh
install -m 0600 /path/to/github-app.pem "$ROOT/credentials/github-app.pem"
```

Note that the legacy single-scope `github` configuration still requires Keychain
fields, so a Linux node must use the scoped `github.app` / `github.installations`
/ `github.scopes` form described in
[`docs/OPERATIONS.md`](docs/OPERATIONS.md).

## 5. Render and install the systemd units

Render from the release being installed; never edit a generated unit in place.
`render-systemd.sh` writes all three of the node's services at once — the
controller for the selected mode, the five-minute updater with its timer, and
the updater handoff — because the updater units carry release-specific paths.

```sh
set -eu
UNIT_STAGING="$ROOT/systemd/$VERSION"
UNITS_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
mkdir -p "$UNIT_STAGING" "$UNITS_DIR"
"$RELEASE_DIR/render-systemd.sh" observe "$RELEASE_DIR" "$STATE_DIR" "$UNIT_STAGING"
install -m 0600 "$UNIT_STAGING/tart-runner-fleet.service" "$UNITS_DIR/tart-runner-fleet.service"
systemctl --user daemon-reload
systemctl --user enable --now tart-runner-fleet.service
systemctl --user status tart-runner-fleet.service
```

**Install the controller unit only.** The rendered
`tart-runner-fleet-updater.service`, `tart-runner-fleet-updater.timer`, and
`tart-runner-fleet-updater-handoff.service` are the shape the automatic updater
will take, and they are rendered so that a node never needs a hand-written unit
later — but `fleet update apply-latest` still drives a launchd transaction and
refuses on this node. Do not enable the timer; use the manual bridge in step 7.

The controller unit restarts only on failure, stops within thirty seconds,
umasks to `0077`, runs at low CPU and I/O priority, and stops the controller
without killing processes it has already handed work to. It deliberately does
**not** set `ProtectProc` or `ProcSubset`: hiding `/proc` would turn every
admission decision on the node into an unavailable observation.

## 6. Prove the observe steady state

```sh
ENDPOINT="unix://$ROOT/state/fleetd.sock"
FLEET="$ROOT/current/fleet"
systemctl --user status tart-runner-fleet.service
"$RELEASE_DIR/fleet" status --endpoint "$ENDPOINT" --require-ready --output json
"$RELEASE_DIR/fleet" doctor --endpoint "$ENDPOINT" --output json
```

Success means the unit is `active (running)`, the daemon reports the exact
installed version in `observe` mode, the `scheduler` observation is `fresh`, and
`hostPressure` carries plausible memory, disk, swap, load, and idle CPU read
from `/proc`. A stale or unavailable observation on a machine that is plainly
running means the probe could not read the host; do not proceed.

`hostPressure.admissionAllowed` may be `false` — a node with less free disk than
`guards.minFreeDiskGiB`, for instance, correctly refuses to admit work. That is
a measurement, not a fault, and it does not stop the node reaching the steady
state. An unavailable *observation* does. The `scheduler` observation's `detail`
names which one and why.

The repository ships the same check as one command, which is what CI runs on
every commit:

```sh
./scripts/observe-smoke.sh "$RELEASE_DIR/fleet"
```

Reboot once and repeat. Lingering plus `WantedBy=default.target` is what makes
the unit come back without a login.

## 7. Installing a newer release, by hand

Until the release transaction speaks `systemctl`, a Linux node adopts a new
generation with the same steps a macOS node's manual bridge uses. Do it while
the node is quiescent — no queued jobs, no live instances, no retrying
operations — which on an observe-mode node with no scopes is always true.

```sh
set -eu
# 1. Download and verify the new release exactly as in step 2.
# 2. Re-render the units from the NEW release directory.
"$RELEASE_DIR/render-systemd.sh" observe "$RELEASE_DIR" "$STATE_DIR" "$UNIT_STAGING"
install -m 0600 "$UNIT_STAGING/tart-runner-fleet.service" "$UNITS_DIR/tart-runner-fleet.service"
# 3. Swap the convenience link and the recorded generation together.
ln -sfn "$RELEASE_DIR" "$ROOT/current.new" && mv -Tf "$ROOT/current.new" "$ROOT/current"
# 4. Reload and restart, then VERIFY.
systemctl --user daemon-reload
systemctl --user restart tart-runner-fleet.service
systemctl --user status tart-runner-fleet.service
"$RELEASE_DIR/fleet" status --endpoint "$ENDPOINT" --require-ready --output json
```

Verification is not optional. `systemctl --user restart` reports success as soon
as the process starts; only `status --require-ready` proves the exact new version
became ready. If it does not, re-render from the previous release directory and
restart again — the previous generation is still on disk, which is the whole
point of keeping it.

## Rollback

Stop the unit and the node stops existing as far as the rest of the fleet is
concerned: it owns no scale sets, holds no session, and has told GitHub nothing.

```sh
systemctl --user disable --now tart-runner-fleet.service
```

## Next

- [`USAGE.md`](USAGE.md) — the operator command surface, identical on every node.
- [`docs/AGENT_RUNBOOK.md`](docs/AGENT_RUNBOOK.md) — monitoring and incidents.
- [`docs/MULTI_NODE_PLAN.md`](docs/MULTI_NODE_PLAN.md) — Part B, the cutover
  that gives this node scale sets.
