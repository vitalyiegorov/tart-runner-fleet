# `config/nodes/`

One file per node. A node is a fleet
([ADR 0034](../../docs/adr/0034-a-node-serves-the-scale-sets-it-owns.md)): each
machine runs one daemon against one plain configuration file, reads no other
node's state, and is aware of no other node. Nothing in this directory is read at
runtime by anything.

What the directory is for is the one moment the fleet exists as a single
artifact. `tests/contract/node_configuration_test.go` decodes every `*.json`
here, validates each the way `fleet config validate` does, builds the bindings
the daemon builds at startup, and then applies the two rules that are knowable
only with every node in hand:

- **Guest-capability parity.** For any label advertised by more than one node,
  every capability a scale set requires behind that label must be declared by
  every node that advertises it, on the base image for that label's platform.
- **One owner per scale set.** A `(scope, scale-set name)` pair appears in
  exactly one node's file.
- **Every image answers for its runner version.** Each image a node boots
  declares a `baseImageRunnerVersion`
  ([ADR 0041](../../docs/adr/0041-a-base-image-declares-its-runner-version.md)).
  The test asserts the declaration exists and is orderable; whether it clears the
  floor is a live condition `fleet doctor` judges on the node that is running,
  because GitHub's 30-day rolling rule makes today's compliant declaration
  tomorrow's stale one without anyone touching this repository.

The same rules run on demand:

```sh
fleet config validate config/nodes/*.json
```

## What lands here

ADR 0034 §5 describes a render step — shared definitions under `nodes/shared/`,
a per-node overlay, and `scripts/render-node-config.sh` writing the composed
result. That step is **not built**. When it lands, its rendered files belong in
this directory, and the contract test above starts guarding them on the same
commit with no edit to the test.

## The three files here today

`mac-mini.json` and `mac-studio.json` are hand-written examples that model the
real topology: two nodes that both advertise `linux-xl` and `macos-maestro`,
which *Amendment 2026-08-04b* of ADR 0034 permits and which the parity rule then
qualifies. Their `baseImageCapabilities` are equal on both nodes, per platform,
which is the whole point — make one of them leaner and the contract test fails.

They carry **no credentials**, in the style of
[`config/fleet.example.json`](../fleet.example.json): `github.app` names blank
Keychain and client fields, no installation ID is real, and the scale-set IDs are
placeholders. They therefore validate in observe mode, which is what the contract
test asserts, and they are not runnable as-is. A node's real file supplies its own
credential source, its own installation ID, and the scale-set IDs
`fleet scale-sets provision` writes back.

Their `baseImageRunnerVersion` values are **measured, not aspirational**: on
2026-08-17 node A's two images carried `actions/runner` 2.335.1 and node C's
carried 2.336.0, which is why the two files disagree. `runnerVersionFloor` is
`2.336.0` on both because 2.336.0 was published 2026-07-20 and GitHub's rolling
rule gives 30 days to install a new release. Node A is therefore declared below
its own floor on purpose: `fleet doctor` fails the `runner version` check there
until the image is refreshed. Raising the floor is what an operator does when a
new `actions/runner` release ships; it is stated per node so it can be raised
without cutting a fleet release.

`geekom.json` is node B — a GEEKOM A9 (Ryzen AI 9 HX 470, 12c/24t, ~27 GiB
usable, x86_64) running Omarchy 4 — and it is the one file here that describes a
machine that exists but is not yet serving anything. It is [`Phase 1 Part A` of
`docs/MULTI_NODE_PLAN.md`](../../docs/MULTI_NODE_PLAN.md#part-a--the-day-the-geekom-arrives)
written down: `macosBurst.enabled: false`, `hostBudget` stated explicitly, the
Linux profile matrix, `executor.backend: podman` with `/dev/kvm` granted to the
Android profile alone, and **no `github.scopes` entries at all**. The empty scope
list is the point rather than an omission: ADR 0034 §2 gives a
`(scope, scale-set name)` pair exactly one owner, mac-mini still owns every Linux
scale set, and the migration that moves them (issue #200) is gated on the Android
question of
[`docs/MULTI_NODE_PLAN.md`](../../docs/MULTI_NODE_PLAN.md#android-is-the-load-bearing-migration).
A node that owns nothing advertises nothing, so it can be committed today without
racing mac-mini for a label.

One of its values is worth reading as the placeholder it is: its
`baseImageRunnerVersion` is the release the runner image *will* be built from —
that image does not exist yet, so unlike the two Macs' it is aspirational rather
than measured, and it must be re-measured the day the image is sealed.

Its `guestArch` is `amd64`, which is what makes its profile labels the canonical
`trf-linux-amd64-2x4` and `trf-linux-amd64-4x8` that ADR 0034 §4 promises. The
key is new (issue #269, ADR 0032's amendment of 2026-08-25); before it, the
architecture component was the package constant `arm64` and a configured
`trf-linux-amd64-2x4` was refused as a label describing a vector it does not
have, so this file shipped with plain `linux-amd64-*` aliases instead. Omitting
the key means `arm64`, which is why `mac-mini.json` and `mac-studio.json` do not
carry it.

The capability vocabulary these files draw from is documented at the seal step of
[`docs/LINUX_BASE_IMAGE.md`](../../docs/LINUX_BASE_IMAGE.md) and
[`docs/BASE_IMAGE.md`](../../docs/BASE_IMAGE.md). A capability that is not in one
of those tables is a capability no image has been audited for.
