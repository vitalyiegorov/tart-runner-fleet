package lifecycle

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// GateVerifier says whether the LAN cache gate a node's guests use proved it is ours
// just now (ADR 0078). A nil verifier means the node configures no gate, and its
// guests keep what their image says.
type GateVerifier interface {
	Verified(ctx context.Context) bool
}

// resultsFallbackScript points a guest's runner at GitHub's results service. It
// rewrites the override rather than deleting it: the patched Runner.Worker.dll
// ignores the job message's value, so with no override the variable would be
// unset and artifacts would break. An image with no override has an unpatched
// runner, which needs nothing.
//
// On a node with a roaming gate (ADR 0097 amendment, issue #446), $1 is that gate's
// origin and $2 its address, and the guest goes there instead when every check
// passes. The gate must route the guest's traffic, so it is the guest's own host and
// not something on whatever network the host roams to. An unsigned blob read crosses
// the gate's SSH forward, and only the owner answers it with 403: a forward that is
// down answers 503 and a gate with no artifact route 404 from GitHub. Then the image's
// host.containers.internal line, the name the owner signs blob URLs for, is moved to
// that address, and only then the results URL.
const resultsFallbackScript = `set -eu
env=$HOME/actions-runner/.env
grep -q '^ACTIONS_RESULTS_URL=' "$env" 2>/dev/null || exit 0
url=https://results-receiver.actions.githubusercontent.com/
if [ "$#" = 2 ] && ip route show default | grep -Fq " via $2 " &&
  [ "$(curl --silent --noproxy '*' --proto =http --max-redirs 0 --max-time 3 --output /dev/null --write-out '%{http_code}' "$1/.trf/artifacts/blobs/0")" = 403 ] &&
  sudo -n sed -i "s/^[^#]*[[:space:]]host\.containers\.internal\$/$2 host.containers.internal/" /etc/hosts &&
  sudo -n grep -Fqx "$2 host.containers.internal" /etc/hosts; then
  url=$1/
fi
sed -i "s|^ACTIONS_RESULTS_URL=.*|ACTIONS_RESULTS_URL=$url|" "$env"
grep -Fqx "ACTIONS_RESULTS_URL=$url" "$env"`

// homeRestoreScript returns a guest an earlier attempt sent to the roaming gate ($1 at
// $2) to the LAN gate this launch proved ($3 at $4), so a retried guest does not keep
// a route this launch did not choose. A guest with neither setting is left alone.
const homeRestoreScript = `set -eu
env=$HOME/actions-runner/.env
grep -Fqx "ACTIONS_RESULTS_URL=$1/" "$env" 2>/dev/null || sudo -n grep -Fqx "$2 host.containers.internal" /etc/hosts || exit 0
sudo -n sed -i "s/^[^#]*[[:space:]]host\.containers\.internal\$/$4 host.containers.internal/" /etc/hosts
sudo -n grep -Fqx "$4 host.containers.internal" /etc/hosts
sed -i "s|^ACTIONS_RESULTS_URL=.*|ACTIONS_RESULTS_URL=$3/|" "$env"
grep -Fqx "ACTIONS_RESULTS_URL=$3/" "$env"`

// roamingScriptName is the roaming scripts' $0.
const roamingScriptName = "trf-roaming-gate"

// NPMRoute is the gate's npm cache as this node's guests reach it (ADR 0093). A nil
// route, the default, means no guest is ever given a registry. Ready says the route
// answers now; it is asked only of a gate that has just proved itself.
type NPMRoute struct {
	Registry string
	Host     string
	Ready    GateVerifier
}

const (
	// npmMarker is the first line of the file this fleet owns, and the only thing
	// that lets a later launch remove it. A file without it is the image's own.
	npmMarker = "# tart-runner-fleet npm cache (ADR 0093)"
	// npmScriptName is the script's $0.
	npmScriptName = "trf-npm-registry"
	// npmWriteTimeout bounds the optional write within the launch's own deadline. It
	// covers a login shell and the guest's own three-second probe.
	npmWriteTimeout = 5 * time.Second
)

// npmClearScript removes a setting this fleet wrote, which a launch retried on the same
// guest may have left, and which may name a gate this launch did not prove. It leaves an
// image's own file alone, and that guest stays direct, because nothing merges a file this
// fleet did not write. It fails when it cannot show the old setting is gone: a file it
// cannot read is not a file without the marker, so grep's status 1 and its errors differ.
const npmClearScript = `set -eu
rc=$HOME/.npmrc
[ -e "$rc" ] || exit 0
status=0
grep -qxF '` + npmMarker + `' "$rc" || status=$?
case "$status" in
  0) rm -f "$rc" ;;
  1) exit 0 ;;
  *) exit 1 ;;
esac`

// npmWriteScript gives the guest its registry, after npmClearScript. It first asks the
// route from inside the guest, because the host's proof says nothing about the guest's
// network and no client falls back once its registry is set. Only a 200 counts, as for
// the host's own probe: with no --location, curl calls a redirect success, and a guest
// behind a redirecting intermediary must not be sent to a route it cannot use. The file
// is written atomically, so it is whole or absent, and absent means npmjs. The user level
// is deliberate: a project's .npmrc, a job's registry variables and setup-node's
// registry-url all still win. Both proxy spellings are needed, because npm reads noproxy
// and pnpm 11 reads only no-proxy.
const npmWriteScript = `set -eu
rc=$HOME/.npmrc
[ ! -e "$rc" ] || exit 0
status=$(curl --silent --noproxy '*' --proto =http --max-redirs 0 --max-time 3 --output /dev/null --write-out '%{http_code}' "$1-/ping") || exit 0
[ "$status" = 200 ] || exit 0
tmp=$(mktemp "$rc.XXXXXX")
printf '%s\nregistry=%s\nnoproxy=%s\nno-proxy=%s\n' '` + npmMarker + `' "$1" "$2" "$2" >"$tmp"
mv -f "$tmp" "$rc"`

// selectGate is one launch's choice, on one fresh proof. An unproven gate moves the
// guest's results to the node's roaming gate when it has one and the guest reaches it,
// and to GitHub otherwise; a proven one returns a guest an earlier attempt moved to the
// roaming gate. A failure there stops the runner. Then any registry an earlier attempt
// left is removed, and a failure there stops the runner too: that guest may still name
// a gate this launch did not prove. Only after that, a proven gate whose npm route
// answers gives the guest its registry. A failure writing it costs only a cold job,
// because no setting means npmjs.
func (b StdinBootstrapper) selectGate(ctx context.Context, name string) error {
	if b.Gate == nil {
		return nil
	}
	verified := b.Gate.Verified(ctx)
	script := []string{resultsFallbackScript}
	if b.Roaming != "" {
		roaming, roamingErr := url.Parse(b.Roaming)
		home, homeErr := url.Parse(b.Home)
		if roamingErr != nil || homeErr != nil {
			return errors.New("roaming gate unusable")
		}
		origin := strings.TrimSuffix(b.Roaming, "/")
		script = []string{resultsFallbackScript, roamingScriptName, origin, roaming.Hostname()}
		if verified {
			script = []string{homeRestoreScript, roamingScriptName, origin, roaming.Hostname(), strings.TrimSuffix(b.Home, "/"), home.Hostname()}
		}
	}
	if !verified || b.Roaming != "" {
		if err := b.guestScript(ctx, name, "results fallback failed", script...); err != nil {
			return err
		}
	}
	if b.NPM == nil {
		return nil
	}
	if err := b.guestScript(ctx, name, "npm registry clear failed", npmClearScript, npmScriptName); err != nil {
		return err
	}
	if verified && b.NPM.Ready.Verified(ctx) {
		writeCtx, cancel := context.WithTimeout(ctx, npmWriteTimeout)
		defer cancel()
		_ = b.guestScript(writeCtx, name, "", npmWriteScript, npmScriptName, b.NPM.Registry, b.NPM.Host)
	}
	return nil
}

func (b StdinBootstrapper) guestScript(ctx context.Context, name, failure string, script ...string) error {
	// -c and not -lc. Stock Ubuntu's ~/.bash_logout runs clear_console, which fails with no
	// terminal, and a non-interactive login shell reads that file on the exit builtin, so under
	// -l these scripts' `exit 0` exits 1 and every launch fails. They need only base tools.
	args := append([]string{"exec", name, "bash", "-c"}, script...)
	if err := b.Runner.Run(ctx, strings.NewReader(""), args...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New(failure)
	}
	return nil
}
