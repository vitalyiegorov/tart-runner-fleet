package lifecycle

import "context"

// GateVerifier says whether the LAN cache gate a node's guests are baked to use
// proved it is ours just now (ADR 0078). A nil verifier means the node configures
// no gate, and its guests keep what their image says.
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
