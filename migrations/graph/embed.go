// Package graphmigrations exposes the per-tenant AGE graph migration
// files as an embed.FS. The files live alongside this Go source in
// migrations/graph/ — the embed directive is hosted here (rather than in
// internal/migrate) because Go's embed system does not permit parent-path
// traversal, and internal/migrate sits one level above this directory.
//
// Consumed by internal/migrate.ApplyTenantGraph, which iterates the
// files in lexicographic order and applies any whose version is not
// already recorded in <schema>.graph_schema_revisions.
package graphmigrations

import "embed"

// FS holds the V<digits>__*.sql migration files in this directory.
// The pattern intentionally excludes any non-migration files (e.g.,
// atlas.hcl, check.sql, embed.go itself).
//
//go:embed V[0-9]*__*.sql
var FS embed.FS
