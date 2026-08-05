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

## The two files here today

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

The capability vocabulary these files draw from is documented at the seal step of
[`docs/LINUX_BASE_IMAGE.md`](../../docs/LINUX_BASE_IMAGE.md) and
[`docs/BASE_IMAGE.md`](../../docs/BASE_IMAGE.md). A capability that is not in one
of those tables is a capability no image has been audited for.
