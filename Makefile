# Neksur Core — Makefile
#
# Standard targets per docs/phase-0-stack.md §6. Each target is one `go ...`
# invocation; complex orchestration lives in the workflow / docker-compose
# layer, not here.

.PHONY: build test test-unit test-integration test-security lint tidy run-server run-worker run-cli migrate migrate-baseline migrate-apply migrate-status migrate-tenant clean

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

# --- Migrations (Phase 0.5+) ------------------------------------------
# Atlas (versioned mode) is the migration runner — D-0.5.17 + D-0.5.18.
# The Go wrapper at cmd/migrate enumerates tenant_<uuid> schemas and
# applies migrations into each, with public.atlas_schema_revisions as
# the shared audit table (RESEARCH §Pitfall 9).
#
# Phase 0 historical migrations (V0001 + V0030) are baseline-imported
# the first time Atlas runs against an existing database; subsequent
# applies skip them via the atlas.sum checksum manifest.
#
# DATABASE_URL must be set, e.g.:
#   export DATABASE_URL=postgres://neksur_app:neksur_app@localhost:5432/postgres?sslmode=disable
#
# `make migrate` is the headline entry point: applies public-tier
# migrations then iterates discovered tenant schemas.
migrate:
	@test -n "$$DATABASE_URL" || (echo "ERROR: DATABASE_URL is required. Example: export DATABASE_URL=postgres://user:pass@host:5432/db" && exit 1)
	go run ./cmd/migrate

# One-shot baseline import for an existing database where V0001 + V0030
# have already been applied via the Phase 0 raw-psql / sqitch pipeline.
# Pass VERSION=0030 (the Phase 0 high-water mark).
migrate-baseline:
	@test -n "$$DATABASE_URL" || (echo "ERROR: DATABASE_URL is required" && exit 1)
	@test -n "$(VERSION)" || (echo "ERROR: VERSION is required, e.g. VERSION=0030" && exit 1)
	atlas migrate apply \
		--url "$$DATABASE_URL" \
		--dir file://migrations/postgres \
		--exclude 'ag_catalog.*' \
		--revisions-schema public \
		--baseline $(VERSION)

# Apply the public-tier migrations only (skips per-tenant rollout).
# Useful in CI/dev where no tenant_<uuid> schemas exist yet.
migrate-apply:
	@test -n "$$DATABASE_URL" || (echo "ERROR: DATABASE_URL is required" && exit 1)
	atlas migrate apply \
		--url "$$DATABASE_URL" \
		--dir file://migrations/postgres \
		--exclude 'ag_catalog.*' \
		--revisions-schema public

# Show pending migrations and the current revision against DATABASE_URL.
migrate-status:
	@test -n "$$DATABASE_URL" || (echo "ERROR: DATABASE_URL is required" && exit 1)
	atlas migrate status \
		--url "$$DATABASE_URL" \
		--dir file://migrations/postgres \
		--revisions-schema public

# Apply migrations to a single tenant schema. Usage:
#   make migrate-tenant TENANT=tenant_aaaaaaaa_aaaa_4aaa_aaaa_aaaaaaaaaaaa
migrate-tenant:
	@test -n "$$DATABASE_URL" || (echo "ERROR: DATABASE_URL is required" && exit 1)
	@test -n "$(TENANT)" || (echo "ERROR: TENANT is required, e.g. TENANT=tenant_<uuid_underscored>" && exit 1)
	go run ./cmd/migrate --tenant $(TENANT)

# Wipe build artifacts.
clean:
	rm -rf bin/ dist/ coverage.out coverage.html profile.out
