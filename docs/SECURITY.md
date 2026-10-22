# Security model

- Authenticate with a narrowly scoped GitHub App, not personal tokens.
- Grant the App read-only Actions repository permission for canonical workflow
  job inventory. Queue observation does not require Administration permission;
  runner acquisition and cleanup remain behind the official scale-set protocol.
- Keep `github.canonicalJobInventory` disabled until that least-privilege
  permission is approved for every installation and a truthful-capacity
  candidate passes shadow and exact-scope canary verification.
- Load the App key from macOS Keychain when unattended access is available. If
  launchd would trigger an interactive Keychain prompt, use only a user-owned,
  non-symlink regular `privateKeyFile` with exact mode `0600`; file selection
  takes deterministic precedence over stale Keychain metadata.
- Never persist Scale Set JIT configuration; redact all credential-shaped fields.
- Invoke Tart and helpers with argument vectors and context deadlines, never a
  shell command assembled from repository, job, label, or secret data.
- Pin runner/Tart artifacts and verify SHA-256 or signed OCI provenance.
- Only mutate instances carrying this controller's deterministic ownership.
- Require fresh GitHub, Tart, and host observations before destructive actions.
- Run as an unprivileged launchd agent with state/config permissions set to 0700.

## The LAN Actions cache

- A node's LAN Actions cache (ADR 0076) verifies every runtime token against
  GitHub's signing keys and keeps GitHub's per-repository, per-ref scoping.
  Never set `SKIP_TOKEN_VALIDATION`.
- Only branch and tag runs save. `fleet cache-gate` refuses every save whose
  token grants write on anything else, which is what a pull request's token
  does, and refuses any token it cannot read. Pull requests restore what
  trusted runs seeded and write nothing.
- The cache server listens on loopback; guests reach it only through the gate,
  which does not serve the server's management API, metrics or API docs.
- `pull_request_target` runs in the base branch's scope and can save, as on
  GitHub's own cache. Treat it as trusted code.

## Public repositories served by a private fleet

- Treat a public target repository as a source of untrusted code. The fleet
  serves whatever GitHub dispatches; admission control lives in the target
  repository's workflows and Actions settings, not in the daemon.
- Every self-hosted job in a public target must refuse pull requests whose head
  lives in a fork. `.github/workflows/ci.yml` shows the required condition.
- Require approval for all outside contributors on every public target, and
  keep the default `GITHUB_TOKEN` permission read-only, so an approved run
  still starts from the smallest possible credential.
- Keep public-repository targets in a fleet configuration whose blast radius is
  acceptable to every other target on the same host. A public target shares the
  Mac mini, its network position, and its base images with client scopes such
  as `budgie-at/budgie`; separate hosts, or at minimum separate base images and
  a distinct `vmPrefix`, keep one compromised public build from observing
  another tenant's work.
