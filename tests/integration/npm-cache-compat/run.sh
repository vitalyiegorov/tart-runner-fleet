#!/usr/bin/env bash
# A real compatibility run of the npm read-through cache (ADR 0093): the shipped
# Verdaccio config, the real gate binary, and the clients consumers actually pin.
# It needs the network, Podman, Node and Python, so it is not part of `make ci`.
#
#   FLEET=/path/to/fleet nice -n 15 tests/integration/npm-cache-compat/run.sh WORKDIR
#
# WORKDIR must be empty or absent. It runs on an unprivileged account, uses its own
# ports, and removes its container on exit. It never touches trf-* or
# fleet-actions-cache. The caller removes WORKDIR and, if it wants, the image.
#
# A line starting PASS or FAIL is a claim. A line starting NOTE is an observed
# behaviour that the docs state and that neither passes nor fails. The exit status is
# the number of FAIL lines.
# No pipefail: `producer | grep -q x` can kill its producer with SIGPIPE on a match, and
# pipefail would then read that match as no match, passing a check that must fail.
set -u

WORK=${1:?usage: run.sh WORKDIR}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
FLEET=${FLEET:?set FLEET to a fleet binary built from this tree}
IMAGE=$(sed -n 's/^Image=//p' "$REPO/deploy/npm-cache/tart-runner-fleet-npm-cache.container")
USERNS=$(sed -n 's/^UserNS=//p' "$REPO/deploy/npm-cache/tart-runner-fleet-npm-cache.container")
PNPM_VERSIONS=${PNPM_VERSIONS:-11.22.0 11.28.3 12.14.0}
OLD_NPM=${OLD_NPM:-10.9.2}
YARN_VERSION=${YARN_VERSION:-4.17.1}
BUN_VERSION=${BUN_VERSION:-1.3.1}
BASE_PORT=${BASE_PORT:-43100}
GATE=$((BASE_PORT + 1)) BACKEND=$((BASE_PORT + 2)) METRICS=$((BASE_PORT + 3))
RECORDER=$((BASE_PORT + 4)) FIXTURE=$((BASE_PORT + 5)) PRIVATE=$((BASE_PORT + 6))
BACKEND_FIXTURE=$((BASE_PORT + 7)) GATE_FIXTURE=$((BASE_PORT + 8))
CONTAINER=npmcompat-$$
CONTAINER_FIXTURE=npmcompat-fixture-$$
REGISTRY=http://127.0.0.1:$RECORDER/npm/
SECRET=compat-S3CRET-token-$$
FAILS=0 PIDS=()

mkdir -p "$WORK" && WORK=$(cd "$WORK" && pwd)
LOGS=$WORK/logs
mkdir -p "$LOGS" "$WORK/conf" "$WORK/storage" "$WORK/tools"
NODE_DIR=$(dirname "$(node -p 'process.execPath')")
CLEAN_PATH=$NODE_DIR:/usr/bin:/bin

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILS=$((FAILS + 1)); }
note() { echo "NOTE  $*"; }
check() { # check DESCRIPTION CONDITION...
    local description=$1
    shift
    if "$@"; then pass "$description"; else fail "$description"; fi
}

cleanup() {
    for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null; done
    podman rm -f "$CONTAINER" "$CONTAINER_FIXTURE" >/dev/null 2>&1
}
trap cleanup EXIT

background() { # background LOG COMMAND...; sets LAST_PID
    local log=$1
    shift
    "$@" >"$log" 2>&1 &
    LAST_PID=$!
    PIDS+=("$LAST_PID")
}

wait_http() { # wait_http URL
    local _
    for _ in $(seq 1 60); do
        curl -fsS -m 2 --noproxy '*' -o /dev/null "$1" 2>/dev/null && return 0
        sleep 0.5
    done
    return 1
}

start_verdaccio() { # start_verdaccio NAME PORT CONF STORAGE [HOST_LOOPBACK_PORT]
    local network=pasta _
    # Maps a port of the host's loopback into the container, for the fixture uplink.
    [ -n "${5:-}" ] && network=pasta:-T,$5
    podman rm -f "$1" >/dev/null 2>&1
    # The old pasta can hold the port for a moment after its container is gone.
    for _ in $(seq 1 40); do
        curl -s -m 1 --noproxy '*' -o /dev/null "http://127.0.0.1:$2/" || [ $? = 28 ] || break
        sleep 0.5
    done
    podman run -d --name "$1" --userns="$USERNS" --cap-drop=all --security-opt=no-new-privileges --memory=1024m \
        --network="$network" -p "127.0.0.1:$2:4873" -v "$3:/verdaccio/conf:ro,Z" -v "$4:/verdaccio/storage:Z" "$IMAGE" >/dev/null
    wait_http "http://127.0.0.1:$2/-/ping"
}

start_gate() { # start_gate LISTEN BACKEND LOG; sets GATE_PID
    background "$3" "$FLEET" cache-gate --listen "127.0.0.1:$1" --upstream http://127.0.0.1:1 \
        --metrics-listen "127.0.0.1:$METRICS" --npm-upstream "http://127.0.0.1:$2"
    GATE_PID=$LAST_PID
    wait_http "http://127.0.0.1:$1/npm/-/ping"
}

metric_to_guest() {
    curl -fsS --noproxy '*' "http://127.0.0.1:$METRICS/metrics" |
        sed -n 's/^fleet_cache_gate_bytes_total{upstream="npm",direction="to_guest"} //p'
}

# A clean environment per client call: nothing of this account's reaches the client.
client() { # client HOME COMMAND...
    local home=$1
    shift
    mkdir -p "$home"
    # No XDG_*: a guest has none, and Bun reads a different file when it is set.
    env -i PATH="$CLEAN_PATH" HOME="$home" CI=1 "${EXTRA_ENV[@]}" "$@"
}
EXTRA_ENV=()

guest_home() { # guest_home NAME: a HOME holding the file the launch script writes
    local home=$WORK/homes/$1
    rm -rf "$home"
    mkdir -p "$home"
    printf '# tart-runner-fleet npm cache (ADR 0093)\nregistry=%s\nnoproxy=127.0.0.1\nno-proxy=127.0.0.1\n' "$REGISTRY" >"$home/.npmrc"
    echo "$home"
}

direct_home() { # direct_home NAME: a HOME with no setting at all
    local home=$WORK/homes/$1
    rm -rf "$home"
    mkdir -p "$home"
    echo "$home"
}

recorded() { wc -l <"$LOGS/recorder.log"; }
recorded_since() { tail -n "+$(($1 + 1))" "$LOGS/recorder.log"; }

project() { # project DIR: the fixture project every client installs
    mkdir -p "$1"
    cat >"$1/package.json" <<'JSON'
{"name":"compat","version":"1.0.0","private":true,
 "dependencies":{"is-odd":"3.0.1","@sindresorhus/is":"6.3.1","left-pad":"1.3.0"}}
JSON
}

############################################################################
echo "== setup"
cp "$REPO/deploy/npm-cache/verdaccio.yaml" "$WORK/conf/config.yaml"
touch "$LOGS/recorder.log" "$LOGS/fixture.log" "$LOGS/private.log"
check "the pinned Verdaccio starts from the shipped config, rootless, with no capabilities" \
    start_verdaccio "$CONTAINER" "$BACKEND" "$WORK/conf" "$WORK/storage"
check "the gate serves the npm route" start_gate "$GATE" "$BACKEND" "$LOGS/gate.log"
MAIN_GATE_PID=$GATE_PID
background "$LOGS/recorder.out" python3 "$HERE/fixture.py" record "$RECORDER" "127.0.0.1:$GATE" "$LOGS/recorder.log"
background "$LOGS/private.out" python3 "$HERE/fixture.py" registry "$PRIVATE" "$LOGS/private.log"
background "$LOGS/fixture.out" python3 "$HERE/fixture.py" registry "$FIXTURE" "$LOGS/fixture.log"
wait_http "http://127.0.0.1:$RECORDER/npm/-/ping" || fail "the recording proxy did not start"
# A package each registry serves: they answer 404 to anything else, and wait_http needs a 2xx.
wait_http "http://127.0.0.1:$FIXTURE/trust-fixture" || fail "the fixture registry did not start"
wait_http "http://127.0.0.1:$PRIVATE/trust-fixture" || fail "the private registry did not start"

for version in $PNPM_VERSIONS; do
    npm i --silent --no-audit --no-fund --prefix "$WORK/tools/pnpm-$version" "pnpm@$version" >/dev/null 2>&1
done
npm i --silent --no-audit --no-fund --prefix "$WORK/tools/npm-$OLD_NPM" "npm@$OLD_NPM" >/dev/null 2>&1
npm i --silent --no-audit --no-fund --prefix "$WORK/tools/yarn" "@yarnpkg/cli-dist@$YARN_VERSION" >/dev/null 2>&1
npm i --silent --no-audit --no-fund --prefix "$WORK/tools/bun" "bun@$BUN_VERSION" >/dev/null 2>&1
note "node $(node --version), default npm $(npm --version), old npm $OLD_NPM, verdaccio image ${IMAGE##*@}"

############################################################################
echo "== the backend's own locks (the shipped config, with the gate out of the way)"
backend=http://127.0.0.1:$BACKEND
publish=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -X PUT -H 'Content-Type: application/json' \
    --data '{"name":"evil-compat","versions":{},"_attachments":{}}' "$backend/evil-compat")
[ "$publish" -ge 400 ] && pass "Verdaccio refuses an anonymous publish ($publish)" || fail "Verdaccio accepted a publish ($publish)"
adduser=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -X PUT -H 'Content-Type: application/json' \
    --data '{"name":"eve","password":"evepassword"}' "$backend/-/user/org.couchdb.user:eve")
[ "$adduser" -ge 400 ] && pass "Verdaccio refuses registration ($adduser)" || fail "Verdaccio registered a user ($adduser)"
web=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' "$backend/-/verdaccio/data/packages")
note "Verdaccio's own web API answers $web directly; the gate never forwards it"

echo "== the gate's refusals, with the real backend behind it"
gate=http://127.0.0.1:$GATE
for spec in "PUT /npm/evil-compat" "DELETE /npm/lodash/-rev/1" "PUT /npm/-/user/org.couchdb.user:eve" \
    "POST /npm/-/npm/v1/security/advisories/bulk" "POST /npm/-/npm/v1/tokens" "DELETE /npm/-/user/token/$SECRET"; do
    read -r method path <<<"$spec"
    code=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -X "$method" -H "Authorization: Bearer $SECRET" -d '{}' "$gate$path")
    [ "$code" = 403 ] && pass "gate refuses $method ${path/$SECRET/<token>} (403)" || fail "gate answered $code for $method ${path/$SECRET/<token>}"
done
code=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' "$gate/npm/-/whoami")
[ "$code" = 404 ] && pass "gate does not forward whoami (404)" || fail "whoami answered $code"
code=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -H 'Host: gate.example.dev' "$gate/npm/is-odd")
[ "$code" = 403 ] && pass "gate refuses a tunnel hostname (403)" || fail "tunnel hostname answered $code"
code=$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -H 'Cf-Connecting-Ip: 1.2.3.4' "$gate/npm/is-odd")
[ "$code" = 403 ] && pass "gate refuses a request carrying Cloudflare's headers (403)" || fail "Cf request answered $code"
if grep -q "$SECRET" "$LOGS/gate.log"; then fail "the gate logged a token"; else pass "no token in the gate's log"; fi

for host in 192.168.1.64:3017 host.containers.internal:3017; do
    tarball=$(curl -fsS --noproxy '*' -H "Host: $host" "$gate/npm/is-odd" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["versions"]["3.0.1"]["dist"]["tarball"])')
    [ "$tarball" = "http://$host/npm/is-odd/-/is-odd-3.0.1.tgz" ] &&
        pass "a tarball is advertised behind the gate at the address the guest used: $tarball" ||
        fail "advertised tarball for $host: $tarball"
done

############################################################################
for version in $PNPM_VERSIONS; do
    echo "== pnpm $version"
    PNPM=$WORK/tools/pnpm-$version/node_modules/.bin/pnpm
    dir=$WORK/pnpm-$version
    project "$dir"
    direct=$(direct_home "pnpm-$version-direct")
    (cd "$dir" && client "$direct" "$PNPM" install --lockfile-only --store-dir "$WORK/stores/$version-lock" >/dev/null 2>&1)
    [ -s "$dir/pnpm-lock.yaml" ] || fail "pnpm $version made no lockfile"
    cp "$dir/pnpm-lock.yaml" "$dir/lock.orig"

    home=$(guest_home "pnpm-$version")
    got=$(cd "$dir" && client "$home" "$PNPM" config get registry 2>&1 | tail -n 1)
    [ "$got" = "$REGISTRY" ] && pass "pnpm $version reads ~/.npmrc's registry" || fail "pnpm $version read registry = $got"

    before=$(recorded)
    started=$SECONDS
    rm -rf "$dir/node_modules"
    if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/$version-cold" >"$LOGS/pnpm-$version-cold.log" 2>&1); then
        pass "pnpm $version cold --frozen-lockfile through the gate ($((SECONDS - started))s)"
    else
        fail "pnpm $version cold --frozen-lockfile: $(tail -n 3 "$LOGS/pnpm-$version-cold.log")"
    fi
    tgz=$(recorded_since "$before" | grep -c '\.tgz"')
    [ "$tgz" -ge 3 ] && pass "pnpm $version fetched its tarballs through the gate ($tgz requests)" || fail "pnpm $version sent $tgz tarball requests to the gate"
    cmp -s "$dir/pnpm-lock.yaml" "$dir/lock.orig" && pass "pnpm $version left the lockfile alone" || fail "pnpm $version changed the lockfile"
    if grep -qE "127\.0\.0\.1|$RECORDER" "$dir/pnpm-lock.yaml"; then fail "the gate address reached pnpm $version's lockfile"; else pass "no gate address in pnpm $version's lockfile"; fi

    started=$SECONDS
    rm -rf "$dir/node_modules"
    if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/$version-warm" >"$LOGS/pnpm-$version-warm.log" 2>&1); then
        pass "pnpm $version warm --frozen-lockfile, empty store, cache warm ($((SECONDS - started))s)"
    else
        fail "pnpm $version warm --frozen-lockfile: $(tail -n 3 "$LOGS/pnpm-$version-warm.log")"
    fi

    # The integrity the lockfile pins is checked against what the cache serves.
    rm -rf "$dir/node_modules"
    python3 - "$dir/pnpm-lock.yaml" <<'PY'
import re, sys
path = sys.argv[1]
text = open(path, encoding="utf-8").read()
text = re.sub(r"(is-odd@3\.0\.1:\s+resolution: \{integrity: sha512-)[A-Za-z0-9+/]{10}", r"\1AAAAAAAAAA", text, count=1)
open(path, "w", encoding="utf-8").write(text)
PY
    printf 'fetchRetries: 0\n' >"$dir/pnpm-workspace.yaml"
    if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/$version-tamper" >"$LOGS/pnpm-$version-tamper.log" 2>&1); then
        fail "pnpm $version accepted a lockfile integrity the served tarball does not match"
    elif grep -q "INTEGRITY" "$LOGS/pnpm-$version-tamper.log"; then
        pass "pnpm $version still fails a tarball that does not match the lockfile's integrity"
    else
        fail "pnpm $version failed for another reason: $(tail -n 3 "$LOGS/pnpm-$version-tamper.log")"
    fi
    rm -f "$dir/pnpm-workspace.yaml"
    cp "$dir/lock.orig" "$dir/pnpm-lock.yaml"

    # A node_modules restored from a job that used another registry.
    rm -rf "$dir/node_modules"
    (cd "$dir" && client "$direct" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/$version-mix" >/dev/null 2>&1)
    if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/$version-mix" >"$LOGS/pnpm-$version-mix.log" 2>&1); then
        note "pnpm $version accepts a node_modules built against npmjs when the registry is now the gate"
    else
        note "pnpm $version REFUSES a node_modules built against npmjs when the registry is now the gate: $(grep -m1 -E 'ERR_PNPM|ERROR' "$LOGS/pnpm-$version-mix.log")"
    fi
    rm -rf "$dir/node_modules"

    # pnpm's audit is a POST, which the gate refuses.
    if (cd "$dir" && client "$home" "$PNPM" audit >"$LOGS/pnpm-$version-audit.log" 2>&1); then
        fail "pnpm $version audit succeeded through a gate that refuses audits"
    else
        pass "pnpm $version audit fails visibly through the gate: $(grep -m1 -E 'ERR_|403|Error' "$LOGS/pnpm-$version-audit.log" | cut -c1-90)"
    fi

    # A dead proxy in the environment: .npmrc's noproxy must keep the gate direct.
    EXTRA_ENV=(HTTP_PROXY=http://127.0.0.1:1 HTTPS_PROXY=http://127.0.0.1:1 http_proxy=http://127.0.0.1:1 https_proxy=http://127.0.0.1:1)
    printf 'fetchRetries: 0\n' >"$dir/pnpm-workspace.yaml"
    if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/$version-proxy" >"$LOGS/pnpm-$version-proxy.log" 2>&1); then
        pass "pnpm $version bypasses a configured proxy for the gate"
    else
        fail "pnpm $version went to the proxy: $(tail -n 2 "$LOGS/pnpm-$version-proxy.log")"
    fi
    EXTRA_ENV=()
    rm -f "$dir/pnpm-workspace.yaml"
    rm -rf "$dir/node_modules"
done

############################################################################
echo "== the supply-chain checks, through Verdaccio, from a fixture uplink"
mkdir -p "$WORK/conf-fixture" "$WORK/storage-fixture"
sed "s|url: https://registry.npmjs.org/|url: http://127.0.0.1:$FIXTURE/|; s|maxage: 10m|maxage: 1s|" \
    "$REPO/deploy/npm-cache/verdaccio.yaml" >"$WORK/conf-fixture/config.yaml"
# Only the uplink differs. The shipped config's uplink is the run's other half.
start_verdaccio "$CONTAINER_FIXTURE" "$BACKEND_FIXTURE" "$WORK/conf-fixture" "$WORK/storage-fixture" "$FIXTURE" || fail "fixture Verdaccio did not start"
METRICS_SAVED=$METRICS
METRICS=$((BASE_PORT + 9))
start_gate "$GATE_FIXTURE" "$BACKEND_FIXTURE" "$LOGS/gate-fixture.log" || fail "fixture gate did not start"
METRICS=$METRICS_SAVED

supply_chain() { # supply_chain PNPM VERSION PACKAGE PIN SETTING EXPECTED_CODE
    local pnpm=$1 version=$2 package=$3 pin=$4 setting=$5 expected=$6 route registry home dir log
    for route in direct gate; do
        registry=http://127.0.0.1:$FIXTURE/
        [ "$route" = gate ] && registry=http://127.0.0.1:$GATE_FIXTURE/npm/
        dir=$WORK/supply/$version-$package-$route
        rm -rf "$dir"
        mkdir -p "$dir"
        home=$(direct_home "supply-$version-$package-$route")
        printf 'registry=%s\n' "$registry" >"$home/.npmrc"
        # An exact pin: a range lets a newer pnpm resolve around the policy instead of failing it.
        printf '{"name":"s","version":"1.0.0","private":true,"dependencies":{"%s":"%s"}}\n' "$package" "$pin" >"$dir/package.json"
        printf 'fetchRetries: 0\n%s\n' "$setting" >"$dir/pnpm-workspace.yaml"
        log=$LOGS/supply-$version-$package-$route.log
        if (cd "$dir" && client "$home" "$pnpm" install --no-frozen-lockfile --store-dir "$WORK/stores/supply-$version-$package-$route" >"$log" 2>&1); then
            fail "pnpm $version $route: $package installed, and $expected should have stopped it"
        elif grep -qiE "$expected" "$log"; then
            pass "pnpm $version $route: $package fails with $expected"
        else
            fail "pnpm $version $route: $package failed without $expected: $(grep -m2 -E 'ERR|ERROR' "$log")"
        fi
    done
}

# The same checks on the path consumers actually run: a lockfile made before the policy
# existed, then installed frozen. Whatever pnpm does direct, it must do through the cache.
frozen_supply_chain() { # frozen_supply_chain PNPM VERSION PACKAGE PIN SETTING
    local pnpm=$1 version=$2 package=$3 pin=$4 setting=$5 dir home log route registry direct_result="" result
    for route in direct gate; do
        registry=http://127.0.0.1:$FIXTURE/
        [ "$route" = gate ] && registry=http://127.0.0.1:$GATE_FIXTURE/npm/
        dir=$WORK/frozen/$version-$package-$route
        rm -rf "$dir"
        mkdir -p "$dir"
        home=$(direct_home "frozen-$version-$package-$route")
        printf 'registry=%s\n' "$registry" >"$home/.npmrc"
        printf '{"name":"s","version":"1.0.0","private":true,"dependencies":{"%s":"%s"}}\n' "$package" "$pin" >"$dir/package.json"
        printf 'fetchRetries: 0\n' >"$dir/pnpm-workspace.yaml"
        (cd "$dir" && client "$home" "$pnpm" install --lockfile-only --store-dir "$WORK/stores/frozen-$version-$package-$route" >/dev/null 2>&1)
        printf 'fetchRetries: 0\n%s\n' "$setting" >"$dir/pnpm-workspace.yaml"
        log=$LOGS/frozen-$version-$package-$route.log
        if (cd "$dir" && client "$home" "$pnpm" install --frozen-lockfile --store-dir "$WORK/stores/frozen-$version-$package-$route" >"$log" 2>&1); then
            result=installs
        else
            result="fails: $(grep -m1 -oE 'ERR_PNPM_[A-Z_]+' "$log")"
        fi
        if [ "$route" = direct ]; then
            direct_result=$result
        elif [ "$result" = "$direct_result" ]; then
            pass "pnpm $version --frozen-lockfile with $package under '$setting' behaves the same through the cache: $result"
        else
            fail "pnpm $version --frozen-lockfile with $package under '$setting': direct $direct_result, through the cache $result"
        fi
    done
}

for version in $PNPM_VERSIONS; do
    PNPM=$WORK/tools/pnpm-$version/node_modules/.bin/pnpm
    supply_chain "$PNPM" "$version" trust-fixture 1.0.1 'trustPolicy: no-downgrade' 'TRUST_DOWNGRADE|trust downgrade'
    supply_chain "$PNPM" "$version" young-fixture 1.0.0 'minimumReleaseAge: 1440' NO_MATURE
    supply_chain "$PNPM" "$version" tamper-fixture 1.0.0 '' INTEGRITY
    frozen_supply_chain "$PNPM" "$version" trust-fixture 1.0.1 'trustPolicy: no-downgrade'
    frozen_supply_chain "$PNPM" "$version" young-fixture 1.0.0 'minimumReleaseAge: 1440'
done

############################################################################
echo "== npm, npx and global installs"
for flavour in "default:npm" "old:$WORK/tools/npm-$OLD_NPM/node_modules/.bin/npm"; do
    label=${flavour%%:*}
    NPM=${flavour#*:}
    dir=$WORK/npm-$label
    project "$dir"
    direct=$(direct_home "npm-$label-direct")
    (cd "$dir" && client "$direct" "$NPM" install --package-lock-only --no-audit --no-fund >/dev/null 2>&1)
    cp "$dir/package-lock.json" "$dir/lock.orig"
    resolved=$(grep -c '"resolved": "https://registry.npmjs.org/' "$dir/package-lock.json")
    home=$(guest_home "npm-$label")
    version=$(client "$home" "$NPM" --version 2>/dev/null)
    before=$(recorded)
    if (cd "$dir" && client "$home" "$NPM" ci --no-audit --no-fund --cache "$WORK/stores/npm-$label" >"$LOGS/npm-$label-ci.log" 2>&1); then
        pass "npm $version ci installs from a lockfile pinning registry.npmjs.org ($resolved entries)"
    else
        fail "npm $version ci: $(tail -n 3 "$LOGS/npm-$label-ci.log")"
    fi
    tgz=$(recorded_since "$before" | grep -c '\.tgz"')
    if [ "$tgz" -ge 1 ]; then
        pass "npm $version ci rewrote the lockfile's host and fetched through the gate ($tgz requests)"
    else
        note "npm $version ci went straight to the lockfile's registry.npmjs.org URLs (0 through the gate): direct, not cached"
    fi
    cmp -s "$dir/package-lock.json" "$dir/lock.orig" && pass "npm $version ci left the lockfile alone" || fail "npm $version ci changed the lockfile"

    # With a lockfile, `npm install`'s post-install audit meets the gate's refusal.
    rm -rf "$dir/node_modules"
    if (cd "$dir" && client "$home" "$NPM" install --cache "$WORK/stores/npm-$label-audit" >"$LOGS/npm-$label-install.log" 2>&1); then
        pass "npm $version install survives the audit call the gate refuses"
    else
        fail "npm $version install: $(tail -n 3 "$LOGS/npm-$label-install.log")"
    fi
    if (cd "$dir" && client "$home" "$NPM" audit >"$LOGS/npm-$label-audit.log" 2>&1); then
        fail "npm $version audit succeeded through a gate that refuses audits"
    else
        pass "npm $version audit fails visibly through the gate: $(grep -m1 -E 'ENOAUDIT|audit endpoint|403' "$LOGS/npm-$label-audit.log" | cut -c1-90)"
    fi
    if grep -q "127\.0\.0\.1" "$dir/package-lock.json"; then
        note "npm $version install wrote the gate's address into package-lock.json"
    else
        pass "no gate address in npm $version's package-lock.json after install"
    fi

    # No lockfile: what would a job that commits a generated lockfile record?
    fresh=$WORK/npm-$label-fresh
    project "$fresh"
    (cd "$fresh" && client "$home" "$NPM" install --no-audit --no-fund --cache "$WORK/stores/npm-$label-fresh" >/dev/null 2>&1)
    if grep -q "127\.0\.0\.1" "$fresh/package-lock.json"; then
        note "npm $version GENERATES a package-lock.json naming the gate: a job that commits one must opt out"
    else
        pass "a generated package-lock.json does not name the gate (npm $version)"
    fi

    before=$(recorded)
    output=$(cd "$WORK" && client "$home" "$NPM" exec --yes --cache "$WORK/stores/npx-$label" -- cowsay@1.6.0 compat 2>&1)
    if echo "$output" | grep -q compat && recorded_since "$before" | grep -q 'cowsay'; then
        pass "npm $version npx fetches through the gate"
    else
        fail "npm $version npx: $(echo "$output" | tail -n 2)"
    fi

    before=$(recorded)
    if client "$home" "$NPM" install --global --prefix "$WORK/global-$label" --no-audit --no-fund --cache "$WORK/stores/global-$label" typescript@5.6.3 >"$LOGS/global-$label.log" 2>&1 &&
        recorded_since "$before" | grep -q 'typescript'; then
        pass "npm $version install --global fetches through the gate"
    else
        fail "npm $version install --global: $(tail -n 3 "$LOGS/global-$label.log")"
    fi

    publishable=$WORK/publishable-$label
    mkdir -p "$publishable"
    printf '{"name":"compat-not-a-real-package","version":"1.0.0"}\n' >"$publishable/package.json"
    if (cd "$publishable" && client "$home" "$NPM" publish >"$LOGS/publish-$label.log" 2>&1); then
        fail "npm $version published through a read-only gate"
    elif grep -qE 'E403|403 Forbidden|ENEEDAUTH|need auth' "$LOGS/publish-$label.log"; then
        pass "npm $version publish fails visibly: $(grep -m1 -E 'E403|403 Forbidden|ENEEDAUTH|need auth' "$LOGS/publish-$label.log" | cut -c1-100)"
    else
        fail "npm $version publish failed differently: $(tail -n 2 "$LOGS/publish-$label.log" | cut -c1-140)"
    fi
done

############################################################################
echo "== what stays direct"
private_project() { # private_project DIR NPMRC
    mkdir -p "$1"
    printf '{"name":"p","version":"1.0.0","private":true,"dependencies":{"@private/pkg":"1.0.0","is-odd":"3.0.1"}}\n' >"$1/package.json"
    printf '%s\n' "$2" >"$1/.npmrc"
}
scoped="@private:registry=http://127.0.0.1:$PRIVATE/
//127.0.0.1:$PRIVATE/:_authToken=$SECRET"
for version in $PNPM_VERSIONS; do
    PNPM=$WORK/tools/pnpm-$version/node_modules/.bin/pnpm
    dir=$WORK/private-$version
    private_project "$dir" "$scoped"
    home=$(guest_home "private-$version")
    before=$(recorded)
    privateBefore=$(wc -l <"$LOGS/private.log")
    if (cd "$dir" && client "$home" "$PNPM" install --no-frozen-lockfile --store-dir "$WORK/stores/private-$version" >"$LOGS/private-$version.log" 2>&1); then
        pass "pnpm $version resolves a private scope beside the gate"
    else
        fail "pnpm $version private scope: $(tail -n 3 "$LOGS/private-$version.log")"
    fi
    if recorded_since "$before" | grep -q "$SECRET"; then fail "pnpm $version sent the private token to the gate"; else pass "pnpm $version sent the gate no token"; fi
    if recorded_since "$before" | grep -q "@private"; then fail "pnpm $version asked the gate for the private package"; else pass "pnpm $version never named the private package to the gate"; fi
    if tail -n "+$((privateBefore + 1))" "$LOGS/private.log" | grep -q '"authorized": true'; then
        pass "the private registry received the token, so it was really in play"
    else
        fail "the private registry saw no token: the assertion above proves nothing"
    fi

    # A token bound to npmjs is not bound to the gate.
    dir=$WORK/npmjs-token-$version
    private_project "$dir" "//registry.npmjs.org/:_authToken=$SECRET
@private:registry=http://127.0.0.1:$PRIVATE/"
    home=$(guest_home "npmjs-token-$version")
    before=$(recorded)
    (cd "$dir" && client "$home" "$PNPM" install --no-frozen-lockfile --store-dir "$WORK/stores/npmjs-token-$version" >/dev/null 2>&1)
    if recorded_since "$before" | grep -q "$SECRET"; then fail "pnpm $version sent an npmjs-bound token to the gate"; else pass "pnpm $version kept an npmjs-bound token off the gate"; fi

    # A token bound to nothing: the one spelling a client may send to its registry.
    dir=$WORK/unscoped-token-$version
    private_project "$dir" "_authToken=$SECRET
@private:registry=http://127.0.0.1:$PRIVATE/"
    home=$(guest_home "unscoped-token-$version")
    before=$(recorded)
    (cd "$dir" && client "$home" "$PNPM" install --no-frozen-lockfile --store-dir "$WORK/stores/unscoped-token-$version" >/dev/null 2>&1)
    if recorded_since "$before" | grep -q "$SECRET"; then
        note "pnpm $version SENDS an unscoped _authToken to its default registry, so to the gate, which drops it: such a repo must opt out"
    else
        pass "pnpm $version kept an unscoped _authToken off the gate"
    fi

    # setup-node's registry-url points NPM_CONFIG_USERCONFIG at its own file.
    dir=$WORK/userconfig-$version
    project "$dir"
    home=$(guest_home "userconfig-$version")
    printf 'registry=https://registry.npmjs.org/\n//registry.npmjs.org/:_authToken=%s\n' "$SECRET" >"$WORK/userconfig.npmrc"
    EXTRA_ENV=(NPM_CONFIG_USERCONFIG="$WORK/userconfig.npmrc")
    before=$(recorded)
    (cd "$dir" && client "$home" "$PNPM" install --no-frozen-lockfile --store-dir "$WORK/stores/userconfig-$version" >"$LOGS/userconfig-$version.log" 2>&1)
    if [ "$(recorded)" = "$before" ]; then
        pass "pnpm $version honours setup-node's registry-url and goes direct"
    else
        note "pnpm $version sent $(($(recorded) - before)) requests to the gate despite NPM_CONFIG_USERCONFIG"
    fi
    if recorded_since "$before" | grep -q "$SECRET"; then fail "pnpm $version sent setup-node's token to the gate"; else pass "pnpm $version kept setup-node's token off the gate"; fi
    EXTRA_ENV=()

    # A project's own registry line outranks the guest's.
    dir=$WORK/project-registry-$version
    project "$dir"
    printf 'registry=https://registry.npmjs.org/\n' >"$dir/.npmrc"
    home=$(guest_home "project-registry-$version")
    before=$(recorded)
    (cd "$dir" && client "$home" "$PNPM" install --no-frozen-lockfile --store-dir "$WORK/stores/project-registry-$version" >/dev/null 2>&1)
    [ "$(recorded)" = "$before" ] && pass "pnpm $version: a project's own registry line opts out" || fail "pnpm $version sent $(($(recorded) - before)) requests despite the project's registry line"
done

# A job that must not use the cache, such as a publish or a registry check, says so.
opt_out() { # opt_out NAME TOOL VARIABLE
    local home got
    home=$(guest_home "optout-$1-$3")
    EXTRA_ENV=("$3=https://registry.npmjs.org/")
    got=$(cd "$WORK" && client "$home" "$2" config get registry 2>/dev/null | tail -n 1)
    EXTRA_ENV=()
    if [ "$got" = "https://registry.npmjs.org/" ]; then
        pass "$1: a job's $3 opts out of the cache"
    else
        note "$1 ignores $3 and still reads $got"
    fi
}
opt_out npm npm NPM_CONFIG_REGISTRY
for version in $PNPM_VERSIONS; do
    opt_out "pnpm-$version" "$WORK/tools/pnpm-$version/node_modules/.bin/pnpm" NPM_CONFIG_REGISTRY
    opt_out "pnpm-$version" "$WORK/tools/pnpm-$version/node_modules/.bin/pnpm" pnpm_config_registry
done

dir=$WORK/npm-private
private_project "$dir" "$scoped"
home=$(guest_home npm-private)
before=$(recorded)
if (cd "$dir" && client "$home" npm install --no-audit --no-fund --cache "$WORK/stores/npm-private" >"$LOGS/npm-private.log" 2>&1); then
    pass "npm resolves a private scope beside the gate"
else
    fail "npm private scope: $(tail -n 3 "$LOGS/npm-private.log")"
fi
if recorded_since "$before" | grep -qE "$SECRET|@private"; then fail "npm sent the gate the private token or name"; else pass "npm never named the private package or its token to the gate"; fi

echo "== modern Yarn and Bun"
dir=$WORK/yarn
project "$dir"
printf 'nodeLinker: node-modules\nenableTelemetry: false\n' >"$dir/.yarnrc.yml"
touch "$dir/yarn.lock"
home=$(guest_home yarn)
before=$(recorded)
YARN=$WORK/tools/yarn/node_modules/@yarnpkg/cli-dist/bin/yarn.js
EXTRA_ENV=(YARN_ENABLE_IMMUTABLE_INSTALLS=false)
if (cd "$dir" && client "$home" node "$YARN" install >"$LOGS/yarn.log" 2>&1); then
    [ "$(recorded)" = "$before" ] && pass "Yarn $YARN_VERSION ignores ~/.npmrc and goes direct" || fail "Yarn $YARN_VERSION sent $(($(recorded) - before)) requests to the gate"
else
    fail "Yarn $YARN_VERSION install: $(tail -n 3 "$LOGS/yarn.log")"
fi
EXTRA_ENV=()

dir=$WORK/bun
project "$dir"
home=$(guest_home bun)
before=$(recorded)
BUN=$WORK/tools/bun/node_modules/.bin/bun
if (cd "$dir" && client "$home" "$BUN" install >"$LOGS/bun.log" 2>&1); then
    if [ "$(recorded)" = "$before" ]; then
        note "Bun $BUN_VERSION ignored ~/.npmrc and went direct"
    else
        note "Bun $BUN_VERSION read ~/.npmrc and sent $(($(recorded) - before)) requests through the gate"
    fi
    (cd "$dir" && rm -rf node_modules && client "$home" "$BUN" install --frozen-lockfile >"$LOGS/bun-frozen.log" 2>&1) &&
        pass "Bun $BUN_VERSION --frozen-lockfile works with the guest setting" ||
        fail "Bun $BUN_VERSION --frozen-lockfile: $(tail -n 3 "$LOGS/bun-frozen.log")"
else
    fail "Bun $BUN_VERSION install: $(tail -n 3 "$LOGS/bun.log")"
fi

############################################################################
echo "== the cache answers without npmjs, and what pruning does"
version=${PNPM_VERSIONS%% *}
PNPM=$WORK/tools/pnpm-$version/node_modules/.bin/pnpm
dir=$WORK/pnpm-$version
home=$(guest_home warm-offline)
sed -i 's|url: https://registry.npmjs.org/|url: http://127.0.0.1:1/|' "$WORK/conf/config.yaml"
start_verdaccio "$CONTAINER" "$BACKEND" "$WORK/conf" "$WORK/storage" || fail "Verdaccio did not restart with no uplink"
code=$(curl -s -m 60 -o /dev/null -w '%{http_code}' --noproxy '*' "http://127.0.0.1:$BACKEND/never-cached-compat-package")
[ "$code" != 200 ] && pass "with no uplink, a package never seen cannot be read ($code), so the uplink is really gone" ||
    fail "a package never seen was served with no uplink"
rm -rf "$dir/node_modules"
if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/offline" >"$LOGS/offline.log" 2>&1); then
    pass "a warm cache serves pnpm's frozen install with no uplink at all, so those bytes never crossed the WAN"
else
    fail "warm cache with no uplink: $(tail -n 3 "$LOGS/offline.log")"
fi
cp "$REPO/deploy/npm-cache/verdaccio.yaml" "$WORK/conf/config.yaml"
start_verdaccio "$CONTAINER" "$BACKEND" "$WORK/conf" "$WORK/storage" || fail "Verdaccio did not restart with its uplink"
note "cache after this run: $(du -sh "$WORK/storage" | cut -f1), $(find "$WORK/storage" | wc -l) inodes, npm to_guest $(metric_to_guest) bytes"
find "$WORK/storage" -name 'is-odd-*.tgz' -delete
rm -rf "$dir/node_modules"
if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/pruned" >"$LOGS/pruned.log" 2>&1); then
    pass "deleting cached tarballs is safe: the next read fetches them again"
else
    fail "after pruning a tarball: $(tail -n 3 "$LOGS/pruned.log")"
fi
find "$WORK/storage" -mindepth 2 -maxdepth 2 -name 'left-pad' -exec rm -rf {} + 2>/dev/null
find "$WORK/storage/data" -maxdepth 1 -name 'left-pad' -exec rm -rf {} + 2>/dev/null
rm -rf "$dir/node_modules"
if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/pruned2" >"$LOGS/pruned2.log" 2>&1); then
    pass "deleting a whole package directory is safe too"
else
    fail "after pruning a package: $(tail -n 3 "$LOGS/pruned2.log")"
fi

############################################################################
echo "== losing the gate"
home=$(guest_home gate-loss)
rm -rf "$dir/node_modules"
kill "$MAIN_GATE_PID" 2>/dev/null
wait "$MAIN_GATE_PID" 2>/dev/null
started=$SECONDS
if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/lost" >"$LOGS/lost.log" 2>&1); then
    fail "an install succeeded with the gate gone, which contradicts what the docs say"
else
    pass "a lost gate fails the install after $((SECONDS - started))s, with no fallback to npmjs"
fi
start_gate "$GATE" "$BACKEND" "$LOGS/gate2.log"
rm -rf "$dir/node_modules"
if (cd "$dir" && client "$home" "$PNPM" install --frozen-lockfile --store-dir "$WORK/stores/lost" >"$LOGS/rerun.log" 2>&1); then
    pass "the rerun succeeds once the gate is back"
else
    fail "the rerun failed: $(tail -n 3 "$LOGS/rerun.log")"
fi

############################################################################
# Optional, and the most convincing: a consumer's real lockfile, whose names, scopes and
# tarballs nobody picked for this test. REAL_PROJECTS holds "NAME:PNPM_VERSION:DIR"
# entries, each DIR holding at least that project's lockfile and workspace file.
for entry in ${REAL_PROJECTS:-}; do
    IFS=: read -r name version source <<<"$entry"
    echo "== a real lockfile: $name, pnpm $version"
    PNPM=$WORK/tools/pnpm-$version/node_modules/.bin/pnpm
    [ -x "$PNPM" ] || npm i --silent --no-audit --no-fund --prefix "$WORK/tools/pnpm-$version" "pnpm@$version" >/dev/null 2>&1
    dir=$WORK/real-$name
    rm -rf "$dir"
    cp -r "$source" "$dir"
    home=$(guest_home "real-$name")
    for pass in cold warm; do
        before=$(metric_to_guest)
        requests=$(recorded)
        started=$SECONDS
        if (cd "$dir" && client "$home" "$PNPM" fetch --store-dir "$WORK/stores/real-$name-$pass" >"$LOGS/real-$name-$pass.log" 2>&1); then
            pass "$name $pass fetch of the real lockfile through the gate, $((SECONDS - started))s, $(($(metric_to_guest) - before)) bytes, $(($(recorded) - requests)) requests"
        else
            fail "$name $pass fetch: $(grep -m3 -E 'ERR_|ERROR' "$LOGS/real-$name-$pass.log" | cut -c1-200)"
        fi
    done
    note "$name: cache now $(du -sh "$WORK/storage" | cut -f1), $(find "$WORK/storage" | wc -l) inodes; refused or missing: $(curl -fsS --noproxy '*' "http://127.0.0.1:$METRICS/metrics" >/dev/null && grep -c 'error' "$LOGS/real-$name-cold.log") error lines"
done

echo
echo "fails: $FAILS"
exit "$FAILS"
