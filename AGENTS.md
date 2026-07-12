# Contributor contract

This controller can terminate VMs and affect every CI queue on the host. Treat
all changes as safety critical.

1. Write a failing test before production code. Production incidents begin as
   replay fixtures.
2. Keep policy pure and deterministic. Time, I/O, randomness, and process
   execution enter through interfaces.
3. Never represent an unavailable observation as an empty collection.
4. Never perform a destructive action without fresh ownership, runner, job,
   Tart, and host confirmation as applicable.
5. Preserve at-least-once delivery and idempotent effects. Commit work before
   acknowledging Scale Set messages.
6. Never log or persist JIT configuration, tokens, private keys, or generated
   runner credentials.
7. Never assemble shell commands from external values. Use argument vectors and
   context deadlines.
8. Run `make verify`; coverage must remain at least 99% and race tests must pass.
9. Do not enable authority mode in a code change. Promotion is an explicit
   operational action after observe/shadow/canary evidence and rollback proof.

