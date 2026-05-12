// Package audit hosts the audit log subsystem. Per docs/phase-0-stack.md
// §6 this package will contain log.go — append-only audit record writer
// that captures every policy decision (allow / deny / mask) and every
// catalog mutation through the L1 gateway. Backed by a dedicated Postgres
// table (not the AGE graph). pgaudit at the DB layer captures the
// independent admin-level audit channel.
//
// Phase 0 status: placeholder. M3 lands the writer + REST query endpoint.
package audit
