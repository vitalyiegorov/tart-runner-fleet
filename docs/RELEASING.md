# Releasing

The release pipeline treats binaries, configuration, checksums, SBOMs, notes,
and the source revision as one immutable generation. Do not rebuild or replace
an already-published generation.

## Continuous production builds

Every trusted push to `main` runs the complete CI pipeline. The publisher
downloads that exact run's verified artifact and creates:

```text
v0.1.<CI-run-number>+main.<12-character-commit-prefix>
```

The CI run number provides monotonic ordering; the build metadata identifies
the source. This channel exists for the fleet's guarded natural updater and is
not a claim that every merge changes the public API.

The publisher verifies the version manifest, allowlisted contents, archive,
SHA-256 manifest, reproducible binaries, and CycloneDX SBOMs before publishing.
Release creation uploads assets while the release is a draft and publishes only
after all uploads succeed. Retries may repair a draft, but never mutate a
published release.

## Promoted semantic versions

Promoted versions use Go-compatible `vMAJOR.MINOR.PATCH` tags. Choose the next
version from all changes since the preceding promoted version:

- `fix` and `perf` require at least a patch increment;
- `feat` requires at least a minor increment;
- a `!` or `BREAKING CHANGE:` requires a major increment once the project is
  stable;
- prereleases use `-alpha.N`, `-beta.N`, or `-rc.N`.

For modules at v2 and later, Go also requires the major suffix in the module and
import paths, such as `/v2`. Do not create a v2 tag until that migration is
complete.

Create promoted tags only from a commit on `main`:

```sh
git switch main
git pull --ff-only
git tag -s v1.2.3 -m 'v1.2.3'
git push origin v1.2.3
```

The tag workflow independently repeats all release verification and publishes
the result. Never use `workflow_dispatch` as a substitute for pushing the
reviewed tag; dispatch builds an artifact but intentionally does not publish.

## Release notes

The PR title is the durable changelog entry. CI requires Conventional Commits,
and the metadata workflow adds a release category. GitHub then generates these
sections automatically:

- Breaking changes
- Features
- Fixes
- Performance
- Documentation
- Dependencies
- Maintenance

Use `release-skip` only for changes that truly have no value in release history.
Generated GitHub releases are the canonical changelog; a manually maintained
second changelog would drift from the artifacts actually deployed.

The repository accepts squash merges only, so the validated PR title becomes
the single commit entering `main`. GitHub's commit-metadata rules are not
available on the repository's current plan; validating the squash subject in
both the unprivileged CI workflow and the non-checkout metadata workflow gives
the protected branch the same Conventional Commit invariant. The same plan
limitation applies to tag-name metadata rules, so `build-release.sh` and the tag
workflow reject non-SemVer release identifiers while the live ruleset makes
every `v*` tag immutable.

## Verification

Download the release assets and verify the manifest before installation:

```sh
shasum -a 256 -c SHA256SUMS
```

Inspect `BUILDINFO.txt` with the version manifest and release tag. Each binary
also has a matching CycloneDX SBOM. GitHub artifact attestations are not enabled
because private repositories require GitHub Enterprise Cloud; enabling the
action here would make valid production releases fail. Revisit attestations if
the repository moves to a plan that supports them.
