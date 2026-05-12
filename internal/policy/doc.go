// Package policy hosts the Policy Service: YAML→Rego compiler, OPA
// evaluator, and policy CRUD store. Per docs/phase-0-stack.md §6 this
// package will contain compiler.go, evaluator.go (OPA wrapper as
// embedded Go library), and store.go.
//
// Phase 0 status: placeholder. M2 lands the compiler + evaluator + CRUD;
// M3 wires it through the L1 gateway and SQL proxy.
package policy
