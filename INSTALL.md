# Install on an Apple Silicon Mac

This guide installs one immutable release, configures GitHub's runner control
plane, and makes the selected controller generation survive login and reboot.
It never installs from a mutable source checkout.

Installing on a Linux node instead? Under ADR 0034 each machine is its own
independent fleet, and the Linux node has its own layout, service manager, and
bring-up sequence: [`INSTALL-linux.md`](INSTALL-linux.md).

## 1. Prerequisites

- Apple Silicon macOS with [Tart](https://tart.run/) available at
  `/opt/homebrew/bin/tart`;
- an authenticated GitHub CLI (`gh auth status`) for release downloads;
- a GitHub App installed on every repository or organization the fleet serves;
- one GitHub Actions runner scale set per profile variant each scope exposes;
- immutable Linux and macOS Tart bases containing `$HOME/actions-runner/run.sh`
  and the matching released `tart-runner-fleet-bootstrap` helper — to build one
  from a pinned public image, follow
  [`docs/BASE_IMAGE.md`](docs/BASE_IMAGE.md) for macOS and
  [`docs/LINUX_BASE_IMAGE.md`](docs/LINUX_BASE_IMAGE.md) for Linux;
- enough host capacity for the configured resource envelope and disk guard.

Run the daemon as an unprivileged logged-in user. Do not use `sudo` for the
LaunchAgent, state directory, or release directory.

## 2. Download and verify a production release

Normal production releases are generated only from successful trusted `main`
CI. Each release contains reproducible binaries, CycloneDX SBOMs, a
deterministic archive, and `SHA256SUMS`.

```sh
set -eu
umask 077
REPOSITORY=owner/tart-runner-fleet
VERSION=$(gh release view --repo "$REPOSITORY" --json tagName --jq .tagName)
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
DOWNLOAD=$(mktemp -d)
RELEASE_DIR="$ROOT/releases/$VERSION"

gh release download "$VERSION" --repo "$REPOSITORY" --dir "$DOWNLOAD" --pattern '*'
(cd "$DOWNLOAD" && shasum -a 256 -c SHA256SUMS)
mkdir -p "$RELEASE_DIR"
tar -xzf "$DOWNLOAD/tart-runner-fleet-$VERSION-darwin-arm64.tar.gz" -C "$RELEASE_DIR"
install -m 0600 "$DOWNLOAD/SHA256SUMS" "$RELEASE_DIR/SHA256SUMS"
install -m 0600 "$DOWNLOAD/tart-runner-fleet-$VERSION-darwin-arm64.tar.gz" \
  "$RELEASE_DIR/tart-runner-fleet-$VERSION-darwin-arm64.tar.gz"
chmod 0700 "$RELEASE_DIR/fleet" "$RELEASE_DIR/render-launchd.sh"
"$RELEASE_DIR/fleet" version
```

The remaining sections assume these variables stay exported in the same shell;
set `ROOT`, `VERSION`, `RELEASE_DIR`, `STATE_DIR`, and `REPOSITORY` again after
opening a new terminal.

Keep the previous immutable release. It is the local rollback generation when
GitHub or the network is unavailable.

## 3. Configure state and GitHub credentials

```sh
set -eu
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
STATE_DIR="$ROOT/state"
mkdir -p "$STATE_DIR"
chmod 0700 "$ROOT" "$STATE_DIR"
install -m 0600 config/fleet.example.json "$STATE_DIR/fleet.json"
"$RELEASE_DIR/fleet" config validate "$STATE_DIR/fleet.json"
```

The checked-in example is an observe-mode starting point. Before shadow,
canary, or authority, add the scoped `github` configuration described in
[`docs/OPERATIONS.md`](docs/OPERATIONS.md), replace all repository targets and
base names, and validate again.

Store the GitHub App PEM either in a Keychain generic-password item or in the
strict `0600` non-symlink file configured by `github.app.privateKeyFile`. Never
put the PEM, JIT configuration, or runner token in JSON, argv, logs, or shell
history.

Create or reconcile runner scale sets with a plan-first command:

```sh
"$RELEASE_DIR/fleet" scale-sets provision --config "$STATE_DIR/fleet.json"
"$RELEASE_DIR/fleet" scale-sets provision \
  --config "$STATE_DIR/fleet.json" --apply --write \
  --confirm provision-scale-sets --reason "initial fleet installation"
```

## 4. Render and validate launchd

Render from the release being installed; never edit a generated plist in place.

```sh
set -eu
PLIST_DIR="$ROOT/launchd/$VERSION"
mkdir -p "$PLIST_DIR"
"$RELEASE_DIR/render-launchd.sh" authority "$RELEASE_DIR" "$STATE_DIR" \
  "$PLIST_DIR/authority.plist"
plutil -lint "$PLIST_DIR/authority.plist"
```

The authority template has `RunAtLoad=true`, restart-on-failure `KeepAlive`, a
bounded exit timeout, restrictive umask, absolute binaries, and the deterministic
PATH needed by Tart and GitHub tooling.

Follow the observe → shadow → dedicated real canary sequence in
[`docs/OPERATIONS.md`](docs/OPERATIONS.md) before the first authority handoff.
Once that evidence is green, atomically install the rendered authority plist as
the canonical boot plist:

```sh
install -m 0600 "$PLIST_DIR/authority.plist" \
  "$HOME/Library/LaunchAgents/com.vitalyiegorov.tart-runner-fleet.plist"
launchctl bootstrap gui/"$(id -u)" \
  "$HOME/Library/LaunchAgents/com.vitalyiegorov.tart-runner-fleet.plist"
launchctl kickstart -k gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.authority
```

## 5. Adopt automatic production updates

Adopt only the exact installed, running, ready generation:

```sh
"$RELEASE_DIR/fleet" update adopt \
  --repo "$REPOSITORY" \
  --release-dir "$RELEASE_DIR" \
  --mode authority \
  --config "$STATE_DIR/fleet.json" \
  --endpoint "unix://$STATE_DIR/fleetd.sock" \
  --confirm adopt-current-generation
```

Adoption installs a separate updater LaunchAgent. It runs at login and every
five minutes, follows normal forward-only production releases, defers while any
queue/VM/retry/dead operation exists, verifies all artifacts and config, and
atomically rolls back if the new exact version and mode do not become ready.
It also publishes `$ROOT/current` as an atomic convenience link to the committed
immutable release. Launchd still executes the exact versioned path.

## 6. Prove reboot recovery

After installation, log out/in or reboot once and run:

```sh
ENDPOINT="unix://$HOME/Library/Application Support/tart-runner-fleet/state/fleetd.sock"
FLEET="$HOME/Library/Application Support/tart-runner-fleet/current/fleet"
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.authority
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater
"$FLEET" status --endpoint "$ENDPOINT" --require-ready --output json
"$FLEET" doctor --endpoint "$ENDPOINT" --output json
```

Success means both LaunchAgents are loaded, the daemon reports the exact
installed version in `authority` mode, observations are fresh, and the updater's
last exit status is zero. If any proof fails, restore the previous immutable
generation locally; do not delete uncertain VMs or start a second authority.
For repeated monitoring and incident handoff, continue with
[`docs/AGENT_RUNBOOK.md`](docs/AGENT_RUNBOOK.md).
