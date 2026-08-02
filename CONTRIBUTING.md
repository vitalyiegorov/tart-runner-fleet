# Contributing

## Pull requests and commits

The repository uses squash merges. The pull request title becomes the commit on
`main`, the durable changelog entry, and the text shown in generated release
notes. Write it in [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/)
form:

```text
<type>[optional scope][!]: <description>
```

Supported types and their release-note sections are:

| Type | Use | Release section |
| --- | --- | --- |
| `feat` | User- or operator-visible capability | Features |
| `fix` | Correctness, reliability, or security fix | Fixes |
| `perf` | Measurable performance improvement | Performance |
| `docs` | Documentation only | Documentation |
| `chore(deps)` | Dependency-only update | Dependencies |
| `build`, `ci`, `chore`, `refactor`, `revert`, `style`, `test` | Internal maintenance | Maintenance |

Use a short, imperative, lower-case description without a trailing period.
Scopes are optional and should name a stable subsystem such as `scheduler`,
`github`, `sqlite`, `updater`, or `release`.

Examples:

```text
feat(github): reconcile the complete active job inventory
fix(scheduler): preserve queue age across request rotation
perf(sqlite): batch observation writes
docs: explain production rollback
chore(deps): update modernc sqlite
feat(api)!: replace the queue observation schema
```

Mark a breaking change with `!` before the colon and explain its migration in a
`BREAKING CHANGE:` footer in the pull request body. During development, commit
messages should follow the same format. Fixups may be used locally because they
are removed by the squash merge.

Pull request bodies must explain the change, its safety proof, and the checks
used to validate it. Add `release-skip` only when a pull request should be
omitted from generated notes.

## Continuous integration for external contributions

Every verification job in this repository runs on a self-hosted fleet of
ephemeral Tart virtual machines hosted on a maintainer's personal hardware.
Running a proposed change there is equivalent to granting the author shell
access to that machine, so **pull requests opened from a fork are never
scheduled onto the fleet**, with or without a maintainer's approval of the run.

What this means in practice:

- A fork pull request skips all verification jobs, and the `Required CI` check
  fails with an explanation instead of reporting a misleading pass.
- A maintainer validates an external contribution by pushing the proposed
  branch into this repository and opening a pull request from that branch,
  after reading the diff. Review precedes execution, never the reverse.
- Everything needed to reproduce the full pipeline locally is in the `Makefile`:
  `make verify` runs the same preflight, quality, unit, race, and build gates
  that CI runs. Please paste its result into the pull request body.
- Workflow runs in a fork of this repository are skipped by design, because the
  runner labels they request only resolve inside this repository.

## Versions and releases

Release tags use Go-compatible semantic versions beginning with `v`. The
continuous production channel publishes immutable verified builds as
`v0.1.<CI-run-number>+main.<commit-prefix>`. Manually promoted public versions
use ordinary `vMAJOR.MINOR.PATCH` tags:

- increment `MAJOR` for an incompatible stable API change;
- increment `MINOR` for a backwards-compatible capability;
- increment `PATCH` for backwards-compatible fixes;
- use a prerelease suffix such as `-rc.1` before promotion.

Promoted versions must be created as annotated signed tags. The release
workflow accepts them only when GitHub verifies the signature and the tag
targets a commit on `main`.

GitHub generates release notes from merged pull requests. The PR metadata
workflow derives exactly one release category from the Conventional Commit
title, and `.github/release.yml` renders Breaking changes, Features, Fixes,
Performance, Documentation, Dependencies, and Maintenance sections.

## Contributor License Agreement

First-time contributors must sign the [CLA](CLA.md) by including this line in
their first pull request description:

```
I have read the CLA document at CLA.md and I hereby sign the CLA.
```

The CLA grants the project owner the right to dual-license contributions
(AGPL-3.0 and commercial terms). Signing once covers all future contributions.
