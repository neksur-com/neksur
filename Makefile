# Neksur Core — Makefile
#
# Standard targets per docs/phase-0-stack.md §6. Each target is one `go ...`
# invocation; complex orchestration lives in the workflow / docker-compose
# layer, not here.

.PHONY: build test test-unit test-integration test-security lint tidy run-server run-worker run-cli migrate clean

# Default — what `make` with no args does.
all: build test

# Build all binaries (./cmd/...) into ./bin/<name>.
build:
	mkdir -p bin
	go build -o bin/ ./cmd/...

# Full test suite — unit + integration + security with race detector.
# CI uses this. NOTE: integration + security need Docker for testcontainers.
test:
	go test ./... -race -count=1

# Unit-only: short-mode, no Docker required.
test-unit:
	go test ./internal/... -race -count=1 -short

# Integration tier — testcontainers-go spins up apache/age:release_PG16_1.6.0.
test-integration:
	go test ./tests/integration/... -race -count=1

# Security tier — RLS isolation + Cypher injection + parameter passthrough.
# Also runs against the testcontainer; same Docker requirement as integration.
test-security:
	go test ./tests/security/... -race -count=1

# Lint via golangci-lint. Phase 0 uses default lints; tune in Phase 1.
lint:
	golangci-lint run

# Refresh go.sum after adding/removing imports.
tidy:
	go mod tidy

# Stub entry-point invocations — these binaries currently print a Phase 0
# placeholder message. They'll be wired in M1/M2/M3 per the stack doc.
run-server: build
	./bin/neksur-server

run-worker: build
	./bin/neksur-worker

run-cli: build
	./bin/neksur-cli

# Apply migrations against a Postgres+AGE 1.6.0 instance.
# The script handles both sqitch (production / CI) and psql-direct fallback
# (developer laptop) — see infra/migrations/run-migrations.sh.
# Usage: make migrate DB_URL=postgresql://user:pass@host:5432/dbname
migrate:
	@test -n "$(DB_URL)" || (echo "ERROR: DB_URL is required. Example: make migrate DB_URL=postgresql://user:pass@host:5432/db" && exit 1)
	bash infra/migrations/run-migrations.sh --db-url "$(DB_URL)"

# Wipe build artifacts.
clean:
	rm -rf bin/ dist/ coverage.out coverage.html profile.out
