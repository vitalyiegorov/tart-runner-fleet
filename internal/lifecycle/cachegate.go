package lifecycle

import (
	"context"
	"errors"
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
const resultsFallbackScript = `env=$HOME/actions-runner/.env
grep -q '^ACTIONS_RESULTS_URL=' "$env" 2>/dev/null || exit 0
sed -i 's|^ACTIONS_RESULTS_URL=.*|ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/|' "$env"
grep -qx 'ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/' "$env"`

// NPMRoute is the gate's npm cache as this node's guests reach it (ADR 0092). A nil
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
	npmMarker = "# tart-runner-fleet npm cache (ADR 0092)"
	// npmScriptName is the script's $0.
	npmScriptName = "trf-npm-registry"
	// npmWriteTimeout bounds the setting within the launch's own deadline. It covers
	// a login shell and the guest's own three-second probe.
	npmWriteTimeout = 5 * time.Second
)

// npmRegistryScript is the guest's half of the choice. With no arguments it removes
// a setting this fleet wrote, which a launch retried on the same guest may have left.
// With a registry and its host, it first asks the route from inside the guest, because
// the host's proof says nothing about the guest's network and no client falls back
// once its registry is set. Then it writes ~/.npmrc atomically, so the file is whole or
// absent, and absent means npmjs. The user level is deliberate: a project's .npmrc,
// a job's NPM_CONFIG_REGISTRY and setup-node's registry-url all still win. Both proxy
// spellings are needed, because npm reads noproxy and pnpm 11 reads only no-proxy.
const npmRegistryScript = `set -eu
rc=$HOME/.npmrc
if [ -e "$rc" ]; then
  grep -qxF '` + npmMarker + `' "$rc" || exit 0
  rm -f "$rc"
fi
[ "$#" -eq 2 ] || exit 0
curl --silent --fail --noproxy '*' --proto =http --max-redirs 0 --max-time 3 --output /dev/null "$1-/ping" || exit 0
tmp=$(mktemp "$rc.XXXXXX")
printf '%s\nregistry=%s\nnoproxy=%s\nno-proxy=%s\n' '` + npmMarker + `' "$1" "$2" "$2" >"$tmp"
mv -f "$tmp" "$rc"`

// selectGate is one launch's choice, on one fresh proof. An unproven gate moves the
// guest's results to GitHub and takes any registry away. A proven gate whose npm route
// answers gives the guest its registry, and a failure writing that costs only a cold
// job, because no setting means npmjs. A guest that may still name an unproven gate
// does not start.
func (b StdinBootstrapper) selectGate(ctx context.Context, name string) error {
	if b.Gate == nil {
		return nil
	}
	verified := b.Gate.Verified(ctx)
	if !verified {
		if err := b.guestScript(ctx, name, "results fallback failed", resultsFallbackScript); err != nil {
			return err
		}
	}
	if b.NPM == nil {
		return nil
	}
	if verified && b.NPM.Ready.Verified(ctx) {
		writeCtx, cancel := context.WithTimeout(ctx, npmWriteTimeout)
		defer cancel()
		_ = b.guestScript(writeCtx, name, "", npmRegistryScript, npmScriptName, b.NPM.Registry, b.NPM.Host)
		return nil
	}
	return b.guestScript(ctx, name, "npm registry clear failed", npmRegistryScript, npmScriptName)
}

func (b StdinBootstrapper) guestScript(ctx context.Context, name, failure string, script ...string) error {
	args := append([]string{"exec", name, "bash", "-lc"}, script...)
	if err := b.Runner.Run(ctx, strings.NewReader(""), args...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New(failure)
	}
	return nil
}
