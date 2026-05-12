// Package engines hosts the engine-adapter abstraction — the Trino /
// Spark integration layer. Per docs/phase-0-stack.md §6 this package
// will contain adapter.go and trino/ subpackage; Spark adapter is M4
// and partially lives in the separate neksur-com/neksur-spark Scala
// repo (the Catalyst rule). Snowflake, Flink, Dremio, Athena, etc.
// land in Phase 1+ per §2.8.
//
// Phase 0 status: placeholder. M3 lands the Trino adapter for the
// read-path end-to-end demo.
package engines
