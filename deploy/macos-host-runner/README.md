# Host-process node setup (macOS)

[ADR 0090](../../docs/adr/0090-ios-e2e-runs-as-host-processes-on-a-second-node-per-mac.md)
runs iOS Maestro jobs as host processes on a Mac, under a dedicated non-admin
`ci-runner` user, through a root-installed launcher. This directory holds the
one-shot root setup and an example node file. **Nothing here has run on a live
host.** Setup comes first, and the host gates G1 to G6 in the ADR must pass
before any job runs.

## Files

- `setup.sh`: the one-shot root setup. It creates `ci-runner` (standard, hidden,
  not an admin, in `_developer`) and installs the launcher at
  `/usr/local/libexec/tart-runner-fleet-host-launcher` (root:wheel 0755). It
  writes `/usr/local/etc/tart-runner-fleet/host-launcher.json` and
  `host-capabilities.json` (root-owned), copies the runner template to
  `/usr/local/share/tart-runner-fleet/actions-runner-template` (root-owned and
  read-only to jobs), and creates `~ci-runner/trf-instances` (0700). It installs
  `/etc/sudoers.d/tart-runner-fleet-host-launcher` only after `visudo -cf`
  accepts it. Finally it installs a pinned simlock **as `ci-runner`** with
  `drivers.ios.deviceSet default`, `ios.defaultMode slim`, and the capacity
  limits. It starts nothing.
- `fleet.host-sim.example.json`: the mac-studio target for the second daemon
  instance: `hostBudget` 6 vCPU / 14 GiB, profile `1x2` × 5, label alias
  `trf-macos-arm64-host-sim`. `internal/config` tests keep it valid.

## Order, on mac-studio first

1. Build the launcher from the release tag:
   `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o tart-runner-fleet-host-launcher-darwin-arm64 ./cmd/tart-runner-fleet-host-launcher`.
2. Copy it, this directory, and an extracted `actions/runner` to the host. The
   host login shell is zsh, so name the interpreter, then run:

   ```sh
   sudo bash setup.sh --daemon-user macstudio \
     --launcher-binary ./tart-runner-fleet-host-launcher-darwin-arm64 \
     --runner-source ./actions-runner \
     --capabilities ios-simulator-simlock,maestro-cli,node-runtime,xcode \
     --simlock-version <exact version> \
     --simlock-max-devices 5 --simlock-slim-bytes 2147483648
   ```

3. Prove G1 (CoreSimulator and simlock as `ci-runner` from the daemon's
   session), G2 (`simlock doctor` as `ci-runner`), G3
   (`sudo -n -u ci-runner -- /usr/local/libexec/tart-runner-fleet-host-launcher list`
   as the daemon user prints `[]`), and G4 (the host tool audit), and record
   each on issue #454.
4. Lower the Tart node's `hostBudget` to 6 vCPU / 12 GiB before the second
   instance takes its 6 / 14 GiB. Never raise it.
5. Start the second instance in observe mode only (G6), with its own state
   directory, socket, and health port.

Root commands the owner runs are the `sudo bash setup.sh …` above and, for G1's
fallback only if G1 fails, a new decision recorded on ADR 0090 first.
