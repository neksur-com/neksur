// Neksur Core — Phase 0 backend monorepo per docs/phase-0-stack.md §2.1 + §6.
// Go is the single backend language for the Phase 0 monorepo (D-PHASE0-stack).
// Cross-language artifacts (Spark Extension in Scala, Python SDK) live in
// separate repos under the neksur-com org per the constraint document.

module github.com/neksur-com/neksur

go 1.25.0

require github.com/jackc/pgx/v5 v5.9.2

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
