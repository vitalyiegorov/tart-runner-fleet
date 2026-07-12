# Security model

- Authenticate with a narrowly scoped GitHub App, not personal tokens.
- Load the App key from macOS Keychain into locked process memory when possible.
- Never persist Scale Set JIT configuration; redact all credential-shaped fields.
- Invoke Tart and helpers with argument vectors and context deadlines, never a
  shell command assembled from repository, job, label, or secret data.
- Pin runner/Tart artifacts and verify SHA-256 or signed OCI provenance.
- Only mutate instances carrying this controller's deterministic ownership.
- Require fresh GitHub, Tart, and host observations before destructive actions.
- Run as an unprivileged launchd agent with state/config permissions set to 0700.

