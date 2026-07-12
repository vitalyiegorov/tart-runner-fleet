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

The CI DAG fans quality (2 CPU/4 GiB), unit coverage (2 CPU/4 GiB), and race
(4 CPU/8 GiB) out after a small preflight. Together they exactly match the
host's 8 CPU/16 GiB Linux envelope. No job uses a GitHub-hosted macOS runner.
The final required job runs even after upstream failures or skips and explicitly
rejects every result other than `success`, preventing branch-protection bypasses.

The 99% line is a floor, not the safety argument. The suite is layered:

1. domain/state-machine table tests;
2. scheduler property and exhaustive small-state tests;
3. golden incident replays from the incumbent manager;
4. GitHub API and Scale Set contract tests with faults and redelivery;
5. SQLite crash/restart and concurrent lease tests;
6. Tart command adapter tests with deadlines and hostile input;
7. integration tests using fakes for every external boundary;
8. canary lifecycle tests on disposable real VMs before promotion.

Every production incident must first become a failing replay. CI runs formatting,
vet, shuffled tests, the race detector, and atomic coverage. Code generated from
upstream schemas is excluded only if it is mechanically generated and separately
validated; no handwritten production package is exempt from the coverage gate.
