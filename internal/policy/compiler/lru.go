// LRU compile-artifact cache for the cross-engine policy compiler.
//
// Mirrors internal/policy/cel/compile.go's pattern (Phase 1 D-1.07): a
// process-local hashicorp/golang-lru/v2.Cache keyed by a comparable
// struct, with the value being the immutable compiled artifact.
//
// Why the same shape:
//   - Hot path: the gateway's CompiledPolicy lookup is on the
//     commit-validation critical path; a cache miss is a Cypher RTT
//     against AGE (single-digit ms but still 100-1000× more expensive
//     than a map lookup).
//   - Invalidation: the cache key carries the policy ID + engine
//     kind/version + the SHA-256 of the source CEL/SQL text. A policy
//     edit produces a new hash → fresh cache key → cold compile path;
//     the stale entry ages out via LRU. No explicit invalidation API
//     is needed.
//
// Cache size: defaults to 4096 entries (matches cel.defaultCacheSize).
// At Phase 2 scale a tenant runs ~10s of policies × ~3 live engines
// (Trino, Spark + Dremio stub) = ~30 entries per tenant; 4096 entries
// covers ~130 tenants at zero eviction pressure.

package compiler

import (
	"crypto/sha256"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"
)

// defaultCacheSize matches internal/policy/cel/compile.go's default so
// the two LRUs scale together when reasoning about per-process memory.
const defaultCacheSize = 4096

// artifactCacheKey is the LRU key shape. All fields are comparable so
// the struct itself is comparable, satisfying golang-lru/v2's generic
// K constraint.
//
// Hashing SourceHash (not just PolicyID) is the load-bearing
// invalidation mechanism: a policy edit changes the source text → new
// SHA-256 → new key → cold compile populates a fresh entry.
type artifactCacheKey struct {
	PolicyID      string
	EngineKind    string
	EngineVersion string
	SourceHash    [32]byte
}

// ArtifactCache is the per-process compile-artifact cache. Construct
// ONCE at compiler startup via NewArtifactCache and share across all
// CompileAll invocations. Thread-safe — the underlying lru.Cache is
// concurrent.
//
// The cached value is a *CompiledArtifact (the in-memory projection
// of the AGE-stored CompiledPolicy.artifact_body); see compiler.go
// for the type definition.
type ArtifactCache struct {
	cache *lru.Cache[artifactCacheKey, *CompiledArtifact]
}

// NewArtifactCache constructs the cache with the given capacity.
// Values <= 0 default to defaultCacheSize (4096). Returns an error
// only if lru.New rejects the size (effectively never after the
// defaulting branch).
func NewArtifactCache(size int) (*ArtifactCache, error) {
	if size <= 0 {
		size = defaultCacheSize
	}
	c, err := lru.New[artifactCacheKey, *CompiledArtifact](size)
	if err != nil {
		return nil, fmt.Errorf("compiler: new lru cache: %w", err)
	}
	return &ArtifactCache{cache: c}, nil
}

// Get returns the cached artifact for the (policyID, engine,
// sourceText) triple, or (nil, false) on miss. The sourceText is
// hashed (SHA-256) so the caller does not have to pre-hash.
func (c *ArtifactCache) Get(policyID, engineKind, engineVersion, sourceText string) (*CompiledArtifact, bool) {
	if c == nil || c.cache == nil {
		return nil, false
	}
	return c.cache.Get(makeKey(policyID, engineKind, engineVersion, sourceText))
}

// Put inserts (or replaces) the artifact for the triple. Eviction of
// the LRU tail is automatic when at capacity.
func (c *ArtifactCache) Put(policyID, engineKind, engineVersion, sourceText string, art *CompiledArtifact) {
	if c == nil || c.cache == nil || art == nil {
		return
	}
	c.cache.Add(makeKey(policyID, engineKind, engineVersion, sourceText), art)
}

// Len reports the current entry count — useful for tests + metrics.
func (c *ArtifactCache) Len() int {
	if c == nil || c.cache == nil {
		return 0
	}
	return c.cache.Len()
}

func makeKey(policyID, engineKind, engineVersion, sourceText string) artifactCacheKey {
	return artifactCacheKey{
		PolicyID:      policyID,
		EngineKind:    engineKind,
		EngineVersion: engineVersion,
		SourceHash:    sha256.Sum256([]byte(sourceText)),
	}
}
