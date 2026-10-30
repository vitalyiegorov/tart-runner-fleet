#!/bin/bash
# One-shot root setup for a host-process node on a Mac (ADR 0090).
#
# Run it ONCE per host, as root, with an explicit interpreter (the hosts' login
# shell is zsh), after reading deploy/macos-host-runner/README.md:
#
#   sudo bash setup.sh --daemon-user macstudio \
#     --launcher-binary ./tart-runner-fleet-host-launcher-darwin-arm64 \
#     --runner-source /path/to/extracted/actions-runner \
#     --capabilities ios-simulator-simlock,maestro-cli,node-runtime,xcode \
#     --simlock-version <pinned version>
#
# It creates the runner account, installs the root-owned launcher and its
# root-owned configuration, installs the runner template, writes the sudoers
# drop-in only after `visudo -cf` accepts it, and installs and configures
# simlock AS THE RUNNER USER. It does not start anything: it installs no launchd
# unit, starts no simlock daemon, and enables no fleet mode. The host gates in
# the README come after it.
set -euo pipefail

runner_user="ci-runner"
daemon_user=""
launcher_binary=""
runner_source=""
capabilities=""
simlock_version=""
simlock_max_devices=5
simlock_slim_bytes=2147483648

launcher_path="/usr/local/libexec/tart-runner-fleet-host-launcher"
etc_dir="/usr/local/etc/tart-runner-fleet"
template_dir="/usr/local/share/tart-runner-fleet/actions-runner-template"
sudoers_path="/etc/sudoers.d/tart-runner-fleet-host-launcher"

die() {
  printf 'setup: %s\n' "$*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --runner-user) runner_user="${2:?}"; shift 2 ;;
    --daemon-user) daemon_user="${2:?}"; shift 2 ;;
    --launcher-binary) launcher_binary="${2:?}"; shift 2 ;;
    --runner-source) runner_source="${2:?}"; shift 2 ;;
    --capabilities) capabilities="${2:?}"; shift 2 ;;
    --simlock-version) simlock_version="${2:?}"; shift 2 ;;
    --simlock-max-devices) simlock_max_devices="${2:?}"; shift 2 ;;
    --simlock-slim-bytes) simlock_slim_bytes="${2:?}"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(uname -s)" = "Darwin" ] || die "this script configures a Mac"
[ "$(id -u)" -eq 0 ] || die "run as root: sudo bash setup.sh ..."
if ! [[ "$runner_user" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || [ "$runner_user" = root ]; then
  die "invalid --runner-user"
fi
if [ -z "$daemon_user" ] || ! id -u "$daemon_user" >/dev/null 2>&1; then
  die "--daemon-user must name the fleet daemon's existing account"
fi
[ "$daemon_user" != "$runner_user" ] || die "the daemon must not run as the runner user"
[ -f "$launcher_binary" ] || die "--launcher-binary must be the built tart-runner-fleet-host-launcher (darwin/arm64)"
[ -x "$runner_source/run.sh" ] || die "--runner-source must be an extracted actions/runner directory containing run.sh"
[[ "$capabilities" =~ ^[a-z0-9-]+(,[a-z0-9-]+)*$ ]] || die "--capabilities must be a comma-separated lowercase list"
[[ "$simlock_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--simlock-version must pin an exact simlock release"
[[ "$simlock_max_devices" =~ ^[1-9][0-9]?$ ]] || die "--simlock-max-devices must be 1..99"
[[ "$simlock_slim_bytes" =~ ^[1-9][0-9]{8,11}$ ]] || die "--simlock-slim-bytes must be a byte count"
command -v npm >/dev/null 2>&1 || [ -x /opt/homebrew/bin/npm ] || die "npm is required to install simlock for the runner user"

step() { printf '==> %s\n' "$*"; }

# 1. The runner account: a standard (non-admin), hidden local user. No
#    password is set here; whether it needs a GUI login session at all is host
#    gate G1 in the README, decided on the real host.
if ! id -u "$runner_user" >/dev/null 2>&1; then
  step "creating standard user $runner_user"
  uid=600
  while dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$uid"; do uid=$((uid + 1)); done
  dscl . -create "/Users/$runner_user"
  dscl . -create "/Users/$runner_user" UserShell /bin/zsh
  dscl . -create "/Users/$runner_user" RealName "tart-runner-fleet host jobs"
  dscl . -create "/Users/$runner_user" UniqueID "$uid"
  dscl . -create "/Users/$runner_user" PrimaryGroupID 20
  dscl . -create "/Users/$runner_user" NFSHomeDirectory "/Users/$runner_user"
  dscl . -create "/Users/$runner_user" IsHidden 1
  createhomedir -c -u "$runner_user" >/dev/null
fi
if dseditgroup -o checkmember -m "$runner_user" admin >/dev/null 2>&1; then
  die "$runner_user is an administrator; a job must never hold admin rights"
fi
runner_uid="$(id -u "$runner_user")"
runner_home="$(dscl . -read "/Users/$runner_user" NFSHomeDirectory | awk '{print $2}')"
[ -d "$runner_home" ] || die "runner home $runner_home does not exist"
# Simulator tests drive xcodebuild, which needs developer-tools authorization.
step "granting $runner_user developer-tools access"
DevToolsSecurity -enable >/dev/null
dseditgroup -o edit -a "$runner_user" -t user _developer

# 2. The launcher, root-owned, outside every directory the daemon can write.
step "installing $launcher_path"
install -d -o root -g wheel -m 0755 /usr/local/libexec "$etc_dir" "$(dirname "$template_dir")"
install -o root -g wheel -m 0755 "$launcher_binary" "$launcher_path"

# 3. The runner template, root-owned and read-only to jobs, so no job can
#    poison the runner every later job is cloned from.
step "installing the runner template at $template_dir"
rm -rf "$template_dir"
ditto "$runner_source" "$template_dir"
chown -R root:wheel "$template_dir"
chmod -R go-w,a+rX "$template_dir"

# 4. The instance root, private to the runner user.
install -d -o "$runner_user" -g staff -m 0700 "$runner_home/trf-instances"

# 5. The capability manifest: the operator's audited declaration of what this
#    host provides (ADR 0079 analogue; see README "tool audit").
step "writing $etc_dir/host-capabilities.json"
caps_json="$(printf '%s' "$capabilities" | awk -F, '{for (i = 1; i <= NF; i++) printf "%s\"%s\"", (i > 1 ? "," : ""), $i}')"
printf '{"schemaVersion":1,"image":"host:%s","sealedAt":"%s","capabilities":[%s]}\n' \
  "$(scutil --get LocalHostName)" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$caps_json" > "$etc_dir/host-capabilities.json"
chown root:wheel "$etc_dir/host-capabilities.json"
chmod 0644 "$etc_dir/host-capabilities.json"

# 6. The launcher's root-owned configuration.
step "writing $etc_dir/host-launcher.json"
cat > "$etc_dir/host-launcher.json" <<JSON
{
  "runnerUser": "$runner_user",
  "runnerUid": $runner_uid,
  "runnerHome": "$runner_home",
  "instanceRoot": "$runner_home/trf-instances",
  "runnerTemplate": "$template_dir",
  "cloneCommand": ["/bin/cp", "-c", "-R"],
  "capabilityManifest": "$etc_dir/host-capabilities.json"
}
JSON
chown root:wheel "$etc_dir/host-launcher.json"
chmod 0644 "$etc_dir/host-launcher.json"

# 7. The sudoers drop-in: the daemon user may run exactly the launcher, as
#    exactly the runner user, without a password. The launcher's stdin carries
#    the JIT configuration, so sudo must never record it.
step "validating and installing $sudoers_path"
candidate="$(mktemp)"
trap 'rm -f "$candidate"' EXIT
cat > "$candidate" <<SUDOERS
# tart-runner-fleet host-process node (ADR 0090). Managed by deploy/macos-host-runner/setup.sh.
Defaults!$launcher_path !log_input, !log_output, env_reset
$daemon_user ALL=($runner_user) NOPASSWD: $launcher_path
SUDOERS
visudo -cf "$candidate" >/dev/null || die "visudo rejected the generated drop-in; nothing was installed"
install -o root -g wheel -m 0440 "$candidate" "$sudoers_path"
visudo -c >/dev/null || { rm -f "$sudoers_path"; die "the full sudoers policy failed validation; the drop-in was removed"; }

# 8. simlock, installed and configured AS THE RUNNER USER. The job reaches it
#    with `simlock --local`, and the CLI only talks to the daemon of its own
#    macOS user, so a worker under any other account is invisible to jobs.
step "installing simlock $simlock_version for $runner_user"
as_runner() { sudo -u "$runner_user" -H env PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin" "$@"; }
simlock_prefix="$runner_home/.simlock-cli"
as_runner npm install --global --prefix "$simlock_prefix" "simlock@$simlock_version" >/dev/null
simlock="$simlock_prefix/bin/simlock"
as_runner "$simlock" config set drivers.ios.deviceSet default
as_runner "$simlock" config set ios.defaultMode slim
as_runner "$simlock" config set capacity "{\"strategy\":\"resource\",\"config\":{\"limits\":{\"ios\":{\"maxDevices\":$simlock_max_devices,\"maxRunning\":$simlock_max_devices},\"maxRunning\":$simlock_max_devices},\"ramBudget\":{\"iosSlimBytesPerDevice\":$simlock_slim_bytes}}}"

step "done; nothing was started"
cat <<NEXT
Remaining steps are host gates, not setup. See deploy/macos-host-runner/README.md:
  G1  prove CoreSimulator works for $runner_user under 'sudo -u' from the daemon's session
      (simlock daemon install needs a GUI login session for $runner_user, or the
      launchctl asuser fallback);
  G2  run '$simlock_prefix/bin/simlock doctor' as $runner_user and confirm the config above;
  G3  run 'sudo -n -u $runner_user -- $launcher_path list' as $daemon_user: it must print [];
  G4  audit the host toolchain against the consumers' pins before exposing the scale set.
NEXT
