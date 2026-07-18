## Change

<!-- Describe policy, state-machine, adapter, or operational changes. -->
<!-- Title: <type>[optional scope][!]: <lower-case imperative description> -->

## Release note

<!-- Describe the operator/user impact. Use "None" for internal-only changes. -->

Release impact: <!-- breaking | feature | fix | performance | documentation | dependencies | maintenance -->

## Safety proof

- [ ] A failing test was added before production code, or this is docs-only.
- [ ] Every affected invariant has a direct production-function test.
- [ ] A production incident has a replay fixture where applicable.
- [ ] `go test -race -shuffle=on -count=1 ./...` passes.
- [ ] Statement coverage remains at least 99%.
- [ ] No secret, JIT configuration, mutable artifact URL, or shell-interpolated input was introduced.
- [ ] Observe/shadow/canary compatibility and rollback were considered.
- [ ] The PR title follows Conventional Commits and accurately classifies the release note.
