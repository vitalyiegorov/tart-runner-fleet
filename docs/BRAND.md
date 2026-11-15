# Unyoke

## Name and positioning

**Winner: Unyoke** (un-YOHK). A short verb for releasing something from a yoke:
CI/CD work comes onto machines you own. It covers compute, caches and artifacts
without naming a particular executor, operating system or host count.

**Tagline:** CI/CD, on your own terms.

**GitHub repository description** (one sentence, under 350 characters):

> Unyoke runs GitHub Actions on your own macOS and Linux machines, with ephemeral runners, LAN caches and a local artifact store, keeping CI/CD resources under your control with fail-closed scheduling.

The product is **Unyoke**; the operator command remains **`fleet`**. Write
“Unyoke's `fleet` CLI” on first mention. `unyoke` is a plausible future command
spelling, not a new executable or alias supplied by this change.

This is local infrastructure for GitHub Actions, not an offline replacement for
GitHub. Avoid promises of unlimited resources, zero infrastructure costs or
complete independence from GitHub. Owned storage removes dependence on hosted
artifact quotas; local disks still have capacity and retention limits.

## Exploration

| Candidate | Rationale / tradeoff |
| --- | --- |
| **Unyoke** | Direct release metaphor; two syllables, executor-neutral and broad enough for storage. |
| Ownrun | Ownership and execution in six letters; can sound like a fitness product. |
| Runstead | A homestead for CI; grounded and memorable, but an existing developer product. |
| Unhitch | Clear separation from rented capacity; an existing terminal command. |
| Unchain | Closest to the owner's phrase; crowded by proxy and blockchain projects. |
| Runyard | A place for your runners; already crowded with developer tools. |
| Unrack | Release plus physical hardware; may suggest taking servers out of service. |
| Runlo | Compact “run local” contraction; meaning needs explanation. |
| Homebound | Brings work home; can imply restriction rather than freedom. |
| Outrun | Energy and escape; generic and heavily used. |
| Freehold | Ownership with staying power; broad commercial associations. |
| Unmoor | Freedom from a fixed provider; suggests drifting rather than safe operation. |
| Workyard | Work on your premises; broad and less distinctive. |
| Runroot | Execution rooted in your hardware; can imply privileged execution. |
| Groundrun | Compute brought to ground; slightly awkward spoken rhythm. |

## Top five and collision screening

Screened on 2026-11-15 using exact-name web searches, searches restricted to
GitHub, npm and pkg.go.dev, and direct GitHub-user and npm-registry lookups.
“No indexed match” is a search result, not proof that a namespace is available.
Go modules use full import paths rather than reserving a global short name.
This is a practical discovery screen, not trademark clearance or a reservation.

| Rank | Candidate | Developer tools and GitHub | npm and Go | Decision |
| --- | --- | --- | --- | --- |
| 1 | **Unyoke** | No prominent developer tool surfaced. The exact [GitHub handle](https://github.com/unyoke) exists as a user, not an organization. An unrelated [Bible app](https://apps.apple.com/us/app/unyoke/id6751823418) uses the name. | Exact [npm registry lookup](https://registry.npmjs.org/unyoke) returned 404; exact-name pkg.go.dev search found no indexed module match. | Best semantic fit with the least developer-tool ambiguity in this shortlist; do not assume the GitHub handle is obtainable. |
| 2 | **Ownrun** | No prominent developer tool surfaced; exact [GitHub handle lookup](https://api.github.com/users/ownrun) returned 404. [OwnRun](https://www.ownrun.app/) already names a running app. | Exact [npm lookup](https://registry.npmjs.org/ownrun) returned 404; no indexed exact-name Go module surfaced. | Strong ownership signal, but the fitness association competes with CI runners. |
| 3 | **Runstead** | Exact collision: [runstead/runstead](https://github.com/runstead/runstead) is an autonomous-work control plane; `runstead` is a GitHub organization. | Exact [npm name](https://registry.npmjs.org/runstead) exists; no indexed exact-name Go module surfaced. | Reject: overlaps both name and developer audience. |
| 4 | **Unhitch** | [Hitch](https://github.com/maxktz/hitch) exposes an `unhitch` terminal command; the exact [GitHub organization](https://github.com/unhitch) also exists. | Exact [npm lookup](https://registry.npmjs.org/unhitch) returned 404; no indexed exact-name Go module surfaced. | Reject: future CLI would conflict with an existing developer command. |
| 5 | **Unchain** | Existing [UNCHAIN developer organization](https://github.com/unchain-tech/UNCHAIN-projects); the exact [GitHub handle](https://github.com/unchain) is a user. | Exact [npm package](https://registry.npmjs.org/unchain) exists for function chaining; [Go proxy command](https://pkg.go.dev/github.com/unchainese/unchain) uses the same name. | Reject: too many nearby software meanings. |

## Visual identity

The mark is an open **U** with its upper-right bar detached. The U holds a
stable base while the separated bar expresses release. It avoids platform
mascots, cloud symbols and detailed chain links that disappear at favicon size.
The gap remains two pixels wide at 16px; the main stroke is three pixels wide.
A 64-unit square includes eight units of clear space around the silhouette.

The horizontal wordmark is an original geometric drawing of “Unyoke,” with
rounded strokes and an open lowercase e. All letters are SVG paths: no font
files, font dependencies, embedded raster data, scripts, filters or external
resources. The mark uses one filled path; each lockup uses that same mark and
one stroked wordmark path. These are editable vector originals.

| Token | Hex | Use |
| --- | --- | --- |
| Deep teal | `#00856A` | Standalone mark and accent on light backgrounds |
| Mint | `#52D6B3` | Accent on dark backgrounds |
| Ink | `#142A2A` | Light lockup lettering; recommended dark background |
| Chalk | `#F3F8F5` | Dark lockup lettering; recommended light background |

- [`logo-mark.svg`](../assets/brand/logo-mark.svg): square symbol, transparent background; use on light backgrounds or with a light backing tile.
- [`logo-lockup-light.svg`](../assets/brand/logo-lockup-light.svg): ink wordmark and deep-teal mark for light backgrounds.
- [`logo-lockup-dark.svg`](../assets/brand/logo-lockup-dark.svg): chalk wordmark and mint mark for dark backgrounds.

Keep the lockup's aspect ratio and built-in spacing. Use the symbol alone below
176px lockup width. Do not close the gap, add gradients or outline the symbol.
SVG titles provide accessible names; the README supplies an HTML image alt.
The README's `<picture>` selects a lockup by preferred color scheme and falls
back to the light version.

All three SVGs were rasterized locally with `rsvg-convert` and visually
inspected, including the mark at 16px and 512px and both background variants.
PNG renders are disposable verification files, not shipped assets.

## Scope and evidence

The README introduction reflects the owner's rebrand brief: multi-node Tart
and Podman execution, an npm/git/Actions LAN cache gate and a local Actions
artifact store. This checkout's ADR directory ends at 0060; ADR 0097, cited by
the owner for artifact storage, is not present here. The storage description
comes from that explicit brief, not a claimed inspection or production audit.
[ADR 0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md) and
[ADR 0058](adr/0058-a-node-declares-the-execution-technology-it-has.md) support
the multi-node, backend-specific platform description. Older plan documents
contain historical single-Mac assumptions; technical sections below the README
introduction are preserved for integration with the parent's broader work.

This is a presentation change. Runtime invariants, `fleet.v1` JSON, database
schema, migration behavior and observe/shadow/canary/authority limits remain
unchanged. It does not establish evidence of an operational cutover.

Answers for the integrating PR's three questions:

1. **Remove overengineering?** Three small, standalone SVGs replace any need for
   a font pipeline, image service or runtime theme logic.
2. **Reduce complexity?** Keep the brand separate from deployment identifiers;
   introduce no new command, setting, state or policy.
3. **Test this?** Validate SVG structure and README asset references, render
   both themes and icon sizes, and run the existing repository gates. No
   scheduler or simulation behavior changes, so no new runtime oracle is needed.

## Later, optional migration checklist

These are independent future decisions, not work authorized by this rebrand.
Keep every existing deployment identifier until a migration has its own ADR,
compatibility tests, rollout evidence and rollback proof.

- [ ] **Go module path:** decide whether an import-path migration has value;
  inventory downstream users and retain old versions and import compatibility.
- [ ] **`fleet` binary:** prefer keeping it; if an `unyoke` alias is ever added,
  preserve `fleet`, scripts, updater invocations and documented exit codes.
- [ ] **systemd/launchd unit names:** inventory services and updater references;
  prove old/new coexistence, single-authority exclusion and rollback before any
  explicitly authorized operational switch.
- [ ] **State paths:** preserve existing databases, sockets, ownership and
  permissions; plan an atomic, reversible migration without dual writers.
- [ ] **Release asset names:** preserve download URLs, signatures, checksums and
  updater compatibility; dual-publish before retiring any old asset spelling.
- [ ] **GitHub repo slug:** inventory workflow guards, release URLs, module
  imports, images and external links; decide whether to retain the slug forever
  or perform a separately approved rename with redirect and updater validation.
