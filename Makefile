# Mirrors .github/workflows/ci.yml. `make check` is the gate to pass before a push.
GOTOOLCHAIN := go1.27.3
export GOTOOLCHAIN

# The test database from docs/reference.md (Development), named so `make db` finds it again.
DB_CONTAINER := hfy-testdb
HFY_TEST_DB ?= postgres://postgres:hfy@127.0.0.1:54330/hfy
export HFY_TEST_DB

GOLANGCI_LINT := $(CURDIR)/bin/golangci-lint

.PHONY: check tidy-check lint fmt-check test db

check: tidy-check lint test

tidy-check:
	go mod tidy -diff

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

# Not in check: `golangci-lint run` does not report formatting, only `fmt` does.
fmt-check: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt --diff

# CI installs this exact version; rebuilt whenever this Makefile changes.
$(GOLANGCI_LINT): Makefile
	GOBIN=$(CURDIR)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0

test:
	HFY_TEST_REQUIRE_DB=1 go test -race -count=1 ./... || { echo 'make test failed; if the output says "no test postgres", run: make db' >&2; exit 1; }

db:
	@if [ -n "$$(docker ps -q -f 'name=^$(DB_CONTAINER)$$')" ]; then echo "$(DB_CONTAINER) is already running"; exit 0; fi; \
	docker start $(DB_CONTAINER) >/dev/null 2>&1 || docker run -d --name $(DB_CONTAINER) -p 127.0.0.1:54330:5432 -e POSTGRES_PASSWORD=hfy -e POSTGRES_DB=hfy pgvector/pgvector:pg17 || exit 1; \
	for i in $$(seq 60); do docker exec $(DB_CONTAINER) pg_isready -q -h 127.0.0.1 -U postgres -d hfy && exit 0; sleep 1; done; \
	echo "$(DB_CONTAINER) is not accepting connections" >&2; exit 1
