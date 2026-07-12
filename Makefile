GO ?= go
COVERAGE_MIN ?= 99.0

.PHONY: fmt test race coverage verify

fmt:
	@files="$$(find . -name '*.go' -not -path './vendor/*')"; \
	if [ -n "$$files" ]; then $(GO)fmt -w $$files; fi

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

coverage:
	./scripts/check-coverage.sh $(COVERAGE_MIN)

verify: fmt test race coverage

