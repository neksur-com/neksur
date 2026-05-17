// Package coordination contains the cross-engine coordination subsystem
// for Neksur. It is a BSL-Core (L1) feature — no license gate is
// required at this layer (snapshot pinning is L1 per ROADMAP §3 SC §5).
//
// # Subtree structure
//
// The coordination/ subtree is organised by functional tier:
//
//   - L1 snapshot/ — SnapshotPin store (this plan). Provides the
//     PinStore, PinLRU, and Sweeper that Plans 03-09 (write-coordinator)
//     and 03-12 (compaction coordinator) consume.
//
//   - L2 schemacache/ — Schema/column-manifest cache (future plan).
//     Reduces AGE round-trips on the hot policy-evaluation path.
//
//   - L2 writeconflict/ — Optimistic write-conflict resolver (future plan).
//     Serialises concurrent Iceberg table writes across engines.
//
//   - L2 verifier/ — Cross-engine consistency verifier (Plan 03-10 territory).
//     Implements the D-3.05 hybrid sampler + differential mirroring.
//
//   - L3 partitionspec/ — Partition spec coordinator (future plan).
//     Propagates Iceberg partition spec changes across all registered
//     engines via the compaction scheduler.
//
//   - L3 compaction/ — Compaction coordinator (Plan 03-12 territory).
//     Orchestrates multi-engine snapshot compaction with pin-retention
//     guards.
//
// # Tier matrix
//
//	Tier | Package         | Plan   | Consumers
//	L1   | snapshot/       | 03-06  | write-coordinator (03-09), compaction (03-12)
//	L2   | schemacache/    | future | policy store hot path
//	L2   | writeconflict/  | future | gateway write interceptor
//	L2   | verifier/       | 03-10  | divergence dashboard
//	L3   | partitionspec/  | future | compaction scheduler
//	L3   | compaction/     | 03-12  | operator runbook, SRE dashboard
package coordination
