# Testing strategy

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

