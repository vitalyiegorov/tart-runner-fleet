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

## Versions and releases

Release tags use Go-compatible semantic versions beginning with `v`. The
continuous production channel publishes immutable verified builds as
`v0.1.<CI-run-number>+main.<commit-prefix>`. Manually promoted public versions
use ordinary `vMAJOR.MINOR.PATCH` tags:

- increment `MAJOR` for an incompatible stable API change;
- increment `MINOR` for a backwards-compatible capability;
- increment `PATCH` for backwards-compatible fixes;
- use a prerelease suffix such as `-rc.1` before promotion.

GitHub generates release notes from merged pull requests. The PR metadata
workflow derives exactly one release category from the Conventional Commit
title, and `.github/release.yml` renders Breaking changes, Features, Fixes,
Performance, Documentation, Dependencies, and Maintenance sections.
