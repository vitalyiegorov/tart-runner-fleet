GO ?= go
GOLANGCI_LINT ?= golangci-lint
TOOL ?= ./scripts/run-tool.sh
COVERAGE_MIN ?= 99.0
CPD_THRESHOLD ?= 100

export GOTOOLCHAIN ?= local

.PHONY: fmt fmt-check mod-check pr-title-check vet workflow-lint self-host-check lint deadcode cpd vuln test unit race coverage build preflight quality verify ci

fmt:
	@files="$$(find . -name '*.go' -not -path './vendor/*')"; \
	if [ -n "$$files" ]; then $(GO)fmt -w $$files; fi

fmt-check:
	@test -z "$$($(GO)fmt -l .)" || { $(GO)fmt -l . >&2; exit 1; }

mod-check:
	$(GO) mod verify
	$(GO) mod tidy -diff
	$(GO) -C tools mod verify
	$(GO) -C tools mod tidy -diff

pr-title-check:
	@test -n "$(PR_TITLE)" || { echo 'PR_TITLE is required' >&2; exit 2; }
	./scripts/check-pr-title.sh "$(PR_TITLE)"

vet:
	$(GO) vet ./...

workflow-lint:
	$(TOOL) actionlint -config-file .github/actionlint.yaml
	./scripts/check-actions-pinned.sh

self-host-check:
	$(GO) test -run '^TestExampleConfigCanBuildItsSuccessor$$' ./internal/config

lint:
	$(GOLANGCI_LINT) run --timeout=5m

deadcode:
	./scripts/check-deadcode.sh

cpd:
	CPD_THRESHOLD=$(CPD_THRESHOLD) ./scripts/check-cpd.sh

vuln:
	$(TOOL) govulncheck ./...

test:
	$(GO) test -shuffle=on -count=1 ./...

unit coverage:
	./scripts/check-coverage.sh $(COVERAGE_MIN)

race:
	$(GO) test -race -shuffle=on -count=1 ./...

build:
	CGO_ENABLED=0 $(GO) build -trimpath ./...

preflight: mod-check fmt-check workflow-lint self-host-check
quality: vet lint deadcode cpd vuln
verify ci: preflight quality unit race build
