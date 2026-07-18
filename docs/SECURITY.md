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
