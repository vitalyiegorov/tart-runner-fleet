# Testing strategy

## CI quality gates

`make ci` is the local equivalent of the blocking pipeline:

- module verification and `go mod tidy -diff`;
- non-mutating `gofmt` check and `go vet`;
- actionlint plus immutable Action SHA enforcement;
- a self-hosting invariant proving this repository is a three-slot fleet target
  whose CI fanout exactly fills the configured Linux envelope;
- curated golangci-lint v2 correctness and source-security analysis;
- official RTA-based `deadcode -test` analysis;
- reachable-vulnerability analysis with `govulncheck`;
- shuffled atomic coverage with an exact 99% statement gate;
- shuffled race detection and native build.

Release CI independently repeats the race and coverage gates, rejects invalid
versions, rebuilds both macOS ARM64 binaries twice, compares their bytes, and
does the same for deterministic CycloneDX 1.6 SBOMs before producing SHA-256
manifests and an immutable archive.

After a trusted `main` push completes Required CI, a separate least-privilege
publisher downloads that exact run's verified artifact. It validates the commit,
run-derived production SemVer, version manifest, allowlisted files, archive
members, and every SHA-256 digest before creating a normal GitHub Release.
Publication is serialized and idempotent; retries can repair partial asset
uploads only when the existing tag still resolves to the same commit.

The CI DAG fans quality (2 CPU/4 GiB), unit coverage (2 CPU/4 GiB), and race
(4 CPU/8 GiB) out after a small preflight. Together they exactly match the
host's 8 CPU/16 GiB Linux envelope. No job uses a GitHub-hosted macOS runner.
The final required job runs after upstream success or failure and explicitly
rejects every result other than `success`. It uses `!cancelled()` so superseded
runs terminate cleanly instead of preserving a queued aggregator indefinitely.

The 99% line is a floor, not the safety argument. The suite is layered:

1. domain/state-machine table tests;
2. scheduler property and exhaustive small-state tests;
3. golden incident replays from the incumbent manager;
4. GitHub API and Scale Set contract tests with faults and redelivery;
5. SQLite crash/restart and concurrent lease tests;
6. Tart command adapter tests with deadlines and hostile input;
7. integration tests using fakes for every external boundary;
8. deterministic simulation of the whole single-host fleet (ADR 0031);
9. canary lifecycle tests on disposable real VMs before promotion.

Layer 8 is the answer to the defect class the other layers structurally cannot
see: a composition that is correct function by function and wrong across passes
and ticks. `tests/simulation` runs the real planner, the real reconcile
controller, and a real SQLite store against a simulated scale-set broker, REST
scope, and host, with every source of nondeterminism drawn from one seed. Eleven
properties are checked after every tick, and a violation shrinks to a minimal
event trace. `make unit` runs a small default seed range; the pull-request gate
widens it to about a minute; `.github/workflows/nightly-simulation.yml` explores
the rest.

A refactor that claims **zero behaviour change** proves it against the same
harness. `TestSimCorpus` reduces a whole seed sweep to counts — plans, applied
plans, spawns, drains, distinct instances, findings — plus a digest folding every
plan identity in tick order, and its own contract is that three sweeps of one arm
reduce identically. Run it on the merge base and on the branch and compare:

```sh
go test ./tests/simulation -run TestSimCorpus -v \
  -corpus-seeds=64 -corpus-ticks=200 -corpus-runs=3
```

A digest that survives a refactor is evidence no admission, no plan, and no
lifecycle transition moved. A digest that changes is a behaviour change, whether
or not it was intended.

Every production incident must first become a failing replay. CI runs formatting,
vet, shuffled tests, the race detector, and atomic coverage. Code generated from
upstream schemas is excluded only if it is mechanically generated and separately
validated; no handwritten production package is exempt from the coverage gate.
