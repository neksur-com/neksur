// Package observability hosts the OpenTelemetry + Prometheus + slog wiring.
// Per docs/phase-0-stack.md §2.10 this package will contain tracing.go
// (OTel SDK setup for Go), metrics.go (Prometheus scrape endpoint), and
// logging.go (structured slog with correlation IDs propagated through
// context.Context). The Phase 0 alert rules already live as PromQL YAML
// under ops/prometheus/alerts/ — those are language-neutral and survive
// the Python→Go correction unchanged.
//
// Phase 0 status: placeholder. M1 lands the basic OTel + slog skeleton;
// every other package picks it up via dependency injection.
package observability
