// neksur-server — main backend binary entry point.
//
// Phase 0 stub. M1 wires up the REST API skeleton + Iceberg REST proxy
// foundation; M2 adds the MCP server + policy CRUD; M3 adds the pgwire
// SQL proxy + L1 Catalog Gateway full validation; M4 adds the Spark
// write-path integration. See docs/phase-0-stack.md §5 for the milestone
// breakdown, and §6 for the planned internal/ package layout this binary
// will compose.
//
// Plan 00-05 (Wave 4) addition: when NEKSUR_OBSERVABILITY=1 is set the
// binary wires up the OTLP gRPC trace exporter and the Prometheus
// /metrics HTTP server defined in internal/graph/telemetry.go.
//
// Plan 00.5-03 addition: when NEKSUR_SAAS_AUTH=1 is set the binary wires
// up the WorkOS auth middleware in front of /api/* + a /webhooks/workos
// endpoint that verifies signatures BEFORE checking SCIM_ENABLED. Both
// feature flags default OFF so the Phase 0 dev workflow (no WorkOS keys,
// no Postgres) keeps building & running cleanly.
//
// Plan 00.5-05 addition: under NEKSUR_SAAS_AUTH=1 the SaaS path also
// mounts:
//   - /webhooks/stripe → billing.WebhookHandler (verifies signature
//     BEFORE BILLING_ENABLED check; D-0.5.21 T-0.5-stripe-spoof).
//   - /admin/*         → admin handlers gated on WorkOS internal_admin
//     org membership (T-0.5-admin-org-bypass).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/google/uuid"

	workosauth "github.com/neksur-com/neksur/internal/auth/workos"
	"github.com/neksur-com/neksur/internal/admin"
	"github.com/neksur-com/neksur/internal/alerts"
	"github.com/neksur-com/neksur/internal/billing"
	"github.com/neksur-com/neksur/internal/catalog"
	"github.com/neksur-com/neksur/internal/credvend"
	"github.com/neksur-com/neksur/internal/crypto/kms"
	"github.com/neksur-com/neksur/internal/detect/dispatch"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/iceberg"
	"github.com/neksur-com/neksur/internal/ingest"
	lineagehttp "github.com/neksur-com/neksur/internal/lineage/http"
	"github.com/neksur-com/neksur/internal/observability"
	celpolicy "github.com/neksur-com/neksur/internal/policy/cel"
	"github.com/neksur-com/neksur/internal/policy/compiler"
	"github.com/neksur-com/neksur/internal/policy/compiler/dialect"
	policystore "github.com/neksur-com/neksur/internal/policy/store"
	"github.com/neksur-com/neksur/internal/sqlproxy"
	sqlproxydialect "github.com/neksur-com/neksur/internal/sqlproxy/dialect"
	"github.com/neksur-com/neksur/internal/tenant"
)

// workosAdminGate adapts *workosauth.Client into the admin.AdminGate
// interface so admin.AdminOrgGate can ask "what org is this session?"
// without admin/ taking a dependency on workos/.
type workosAdminGate struct{ c *workosauth.Client }

func (g workosAdminGate) LoadSessionOrgID(r *http.Request) (string, error) {
	s, err := g.c.LoadSession(r)
	if err != nil {
		return "", err
	}
	return s.OrganizationID, nil
}

func main() {
	fmt.Println("Neksur Server (placeholder — Phase 0 stub; M1 will wire up REST API, MCP server, SQL proxy).")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if os.Getenv("NEKSUR_OBSERVABILITY") == "1" {
		if err := runWithObservability(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "observability bootstrap failed: %v\n", err)
			os.Exit(1)
		}
	}

	if os.Getenv("NEKSUR_SAAS_AUTH") == "1" {
		if err := runWithSaasAuth(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "saas auth bootstrap failed: %v\n", err)
			os.Exit(1)
		}
	}
}

// runWithObservability wires the OTel SDK + Prometheus metrics server
// per the Plan 00-05 D-001.14 contract.
//
//   - OTLP gRPC trace exporter (defaults to localhost:4317, the OTel
//     collector port from infra/otel/docker-compose.observability.yml).
//   - sdktrace.NewTracerProvider with WithBatcher — production-grade
//     batching, not the dev WithSyncer.
//   - otel.SetTracerProvider so internal/graph.ExecuteCypher's
//     otel.Tracer("neksur.graph") resolves to this provider.
//   - Prometheus metrics server on :9100, matching the
//     infra/prometheus/prometheus.yml neksur-graph scrape target.
//
// The function blocks on ctx until SIGINT/SIGTERM, then drains both
// the metrics HTTP server (5s grace) and the trace exporter (5s grace).
func runWithObservability(ctx context.Context) error {
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return fmt.Errorf("otlptracegrpc.New: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(tp)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
	}()

	// Start the Prometheus /metrics server in a goroutine. The error
	// channel surfaces a non-graceful exit so we can fail-fast at boot
	// time if (e.g.) port 9100 is already taken.
	addr := os.Getenv("NEKSUR_METRICS_ADDR")
	if addr == "" {
		addr = ":9100"
	}
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- graph.StartMetricsServer(ctx, addr) }()

	select {
	case <-ctx.Done():
		// Drain the metrics server's error (StartMetricsServer returns
		// ctx.Err() on cancellation, which we expect here).
		<-metricsErr
		return nil
	case err := <-metricsErr:
		return fmt.Errorf("metrics server: %w", err)
	}
}

// runWithSaasAuth wires the Phase 0.5 SaaS auth stack:
//
//   - pgxpool.Pool with graph.WithBeforeAcquireDiscardAll applied to its
//     config (the canonical session-bleed prevention — Task 1).
//   - workosauth.Client constructed from WORKOS_API_KEY / WORKOS_CLIENT_ID
//     / cookie domain (".neksur.com").
//   - tenant.Repo constructed against the pool.
//   - http.ServeMux with three routes:
//     - /api/  → workosauth.TenantMiddleware wrapping a placeholder handler.
//     - /callback → /callback handler that exchanges code → session →
//       sets the HttpOnly + Secure + SameSite=Lax + Domain cookie.
//     - /webhooks/workos → workosClient.HandleWebhook(WORKOS_WEBHOOK_SECRET)
//       which verifies signatures BEFORE checking SCIM_ENABLED.
//
// Configuration sources:
//   - Secrets (WORKOS_API_KEY, WORKOS_CLIENT_ID, WORKOS_WEBHOOK_SECRET,
//     DATABASE_URL) come from AWS Secrets Manager in production (Plan 01
//     Terraform) — Phase 0.5 reads env vars for local-dev / test bootstrap.
//   - The SaaS auth path is intentionally separate from the Phase 0
//     observability/REST path so the two can be brought up independently.
func runWithSaasAuth(ctx context.Context) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL must be set when NEKSUR_SAAS_AUTH=1")
	}
	apiKey := os.Getenv("WORKOS_API_KEY")
	clientID := os.Getenv("WORKOS_CLIENT_ID")
	webhookSecret := os.Getenv("WORKOS_WEBHOOK_SECRET")
	if apiKey == "" || clientID == "" || webhookSecret == "" {
		return errors.New("WORKOS_API_KEY + WORKOS_CLIENT_ID + WORKOS_WEBHOOK_SECRET required when NEKSUR_SAAS_AUTH=1")
	}

	// Pool config — applies BeforeAcquire DISCARD ALL (Plan 03 Task 1
	// + RESEARCH Pitfall 1). Without this hook, session state leaks
	// across pool acquisitions, causing cross-tenant data leaks.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("pgxpool.ParseConfig: %w", err)
	}
	graph.WithBeforeAcquireDiscardAll(cfg)
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pgxpool.NewWithConfig: %w", err)
	}
	defer pool.Close()

	// WorkOS client + tenant repo.
	workosClient, err := workosauth.NewClient(apiKey, clientID, ".neksur.com")
	if err != nil {
		return fmt.Errorf("workos.NewClient: %w", err)
	}
	tenantRepo := tenant.NewRepo(pool)

	// Phase 1 L1 Catalog Gateway dependencies (Plan 01-06).
	// Build the AGE-aware graph client + the policy + ingest substrate
	// ONCE at startup; every gateway handler shares the references.
	// All components reuse the existing pool — DO NOT introduce a
	// second pool here (Phase 0.5 must_have: BeforeAcquire DISCARD ALL
	// is the ONLY enforcement of session-bleed prevention).
	graphClient, err := graph.NewGraphClient(ctx, dsn)
	if err != nil {
		return fmt.Errorf("graph.NewGraphClient: %w", err)
	}
	defer graphClient.Close()

	celEnv, err := celpolicy.NewEnv()
	if err != nil {
		return fmt.Errorf("cel.NewEnv: %w", err)
	}
	celCompiler, err := celpolicy.NewCompiler(celEnv, 0) // 0 → defaultCacheSize 4096
	if err != nil {
		return fmt.Errorf("cel.NewCompiler: %w", err)
	}

	// Plan 02-04 Part B (Fix #3 cross-plan seam) — cross-engine compiler
	// + ABAC AttributeResolver wire-up. Constructed BEFORE gatewayDeps
	// so AttributeResolver can be threaded into the iceberg gateway
	// Deps (handler.go + multi_table.go Step 10 use it to populate
	// cel.Inputs.AttributeResolver — Plan 02-03 declared the field;
	// this commit wires it).
	attrResolver := policystore.NewAttributeResolver(graphClient, pool)
	engineRegistry := policystore.NewEngineRegistry(graphClient)
	compiledStore := policystore.NewCompiledStore(graphClient)
	policyAGEStore := policystore.NewAGEStore(graphClient)

	gatewayDeps := iceberggw.Deps{
		Pool:              pool,
		Graph:             graphClient,
		CredStore:         catalog.NewRepo(pool),
		PolicyStore:       policyAGEStore,
		Evaluator:         celpolicy.NewEvaluator(celCompiler),
		IngestSvc:         ingest.NewService(graphClient),
		AttributeResolver: attrResolver,
	}

	// Dialect registry — per-engine SQL emitters. Each constructor is
	// stateless; one instance per process is shared across all
	// CompileAll invocations.
	dialects := map[string]dialect.DialectCompiler{
		"trino":  dialect.NewTrinoCompiler(),
		"spark":  dialect.NewSparkCompiler(),
		"dremio": dialect.NewDremioCompiler(),
	}

	// ProbeRunner — Phase 2 ships with an empty executor map. The live
	// Trino / Spark probe clients land in Plan 02-05 (SQL proxy)
	// where the engine HTTP clients are first constructed. A nil-/empty-
	// executor probe runner causes the compiler to skip the synthetic
	// probe (probe_skipped log line) and persist CompiledPolicy nodes
	// in `pending` → `active` directly; this is correct for the
	// trigger-driven re-compile path in Phase 2.
	probeRunner := compiler.NewProbeRunner(nil)

	comp, err := compiler.NewCompiler(compiler.CompilerConfig{
		Dialects:       dialects,
		Probes:         probeRunner,
		CompiledStore:  compiledStore,
		EngineRegistry: engineRegistry,
		CELEnv:         celEnv,
		CELCompiler:    celCompiler,
	})
	if err != nil {
		return fmt.Errorf("compiler.NewCompiler: %w", err)
	}

	// Trigger consumer — LISTEN policy_changed on the admin pool,
	// dispatch every NOTIFY to comp.CompileAll. Validates the
	// tenant_id against tenantRepo (defence-in-depth against forged
	// NOTIFY payloads). The PolicyLoader is a thin adapter — the
	// store API for "load PolicySource by ID" lands in Plan 02-05;
	// for now a fail-safe stub logs the receipt and returns
	// ErrLoaderNotWired so the trigger plumbing is exercised end-to-end
	// without invoking compile on a not-yet-implemented loader.
	trig := compiler.NewTrigger(
		pool,
		comp,
		&tenantExistsAdapter{pool: pool},
		&policyLoaderStub{},
	)
	go func() {
		if err := trig.Listen(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("policy/compiler/trigger: Listen exited", "err", err)
		}
	}()

	// Plan 02-05 Wave 2 — sqlproxy mTLS listener wiring. The proxy lives
	// on a SEPARATE listener (NEKSUR_SQLPROXY_ADDR; default :8443) per
	// D-2.08; it is NOT mounted onto the main mux. The sqlproxy mux is
	// wrapped in workosauth.TenantMiddleware before being served (CC1).
	//
	// Conditional start: if any of the three required mTLS env vars
	// is unset, the listener is DISABLED and a structured log line is
	// emitted. Phase 2 dev/test runs do not require mTLS material out
	// of the box; integration tests inject real cert paths via the
	// Phase2Fixture (tests/testfixture/mtls_cert.go).
	var sqlProxyShutdown func(context.Context) error
	{
		sqlProxyCache, err := lru.New[sqlproxy.CacheKey, sqlproxy.ArtifactEntry](4096)
		if err != nil {
			return fmt.Errorf("sqlproxy cache: %w", err)
		}
		injectorDeps := sqlproxy.InjectorDeps{Store: compiledStore, Cache: sqlProxyCache}
		trinoInjector, err := sqlproxydialect.BuildInjector("trino", injectorDeps)
		if err != nil {
			return fmt.Errorf("sqlproxy build trino: %w", err)
		}
		sparkInjector, err := sqlproxydialect.BuildInjector("spark", injectorDeps)
		if err != nil {
			return fmt.Errorf("sqlproxy build spark: %w", err)
		}
		dremioInjector, err := sqlproxydialect.BuildInjector("dremio", injectorDeps)
		if err != nil {
			return fmt.Errorf("sqlproxy build dremio: %w", err)
		}
		sqlInjectors := map[string]sqlproxy.Injector{
			"trino":  trinoInjector,
			"spark":  sparkInjector,
			"dremio": dremioInjector,
		}

		certPath := os.Getenv("NEKSUR_TLS_CERT_PATH")
		keyPath := os.Getenv("NEKSUR_TLS_KEY_PATH")
		caBundlePath := os.Getenv("NEKSUR_CA_BUNDLE_PATH")
		sqlProxyAddr := os.Getenv("NEKSUR_SQLPROXY_ADDR")
		if sqlProxyAddr == "" {
			sqlProxyAddr = ":8443"
		}

		if certPath == "" || keyPath == "" || caBundlePath == "" {
			slog.Info("sqlproxy: mTLS material missing, listener disabled",
				"cert_path_set", certPath != "",
				"key_path_set", keyPath != "",
				"ca_bundle_path_set", caBundlePath != "",
			)
		} else {
			certWatcher, err := sqlproxy.NewCertWatcher(certPath, keyPath)
			if err != nil {
				return fmt.Errorf("sqlproxy cert watcher: %w", err)
			}
			go func() {
				if err := certWatcher.Watch(ctx); err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("sqlproxy: cert watcher exited", "err", err)
				}
			}()
			tlsConfig, err := sqlproxy.NewTLSConfig(certWatcher, caBundlePath)
			if err != nil {
				return fmt.Errorf("sqlproxy tls config: %w", err)
			}
			sqlServer, err := sqlproxy.NewServer(sqlproxy.Deps{
				Injectors: sqlInjectors,
				TLSConfig: tlsConfig,
				Logger:    slog.Default(),
			})
			if err != nil {
				return fmt.Errorf("sqlproxy.NewServer: %w", err)
			}
			// Wrap sqlproxy mux in TenantMiddleware before serving
			// (D-2.08 + Plan 02-05 spec). We bypass sqlServer.ListenAndServeTLS
			// in favor of a locally-constructed *http.Server so the wrapper
			// can interpose without modifying the sqlproxy package.
			wrappedHandler := workosauth.TenantMiddleware(workosClient, tenantRepo)(sqlServer.Handler())
			// WR-02: Clone the TLSConfig so the wrapped *http.Server owns
			// an independent value. The go std library docs explicitly
			// warn to Clone() before passing to http.Server — a future
			// refactor that mutates the original via sqlproxy.Server's
			// fields would otherwise race the live handshake path.
			wrappedSrv := &http.Server{
				Addr:              sqlProxyAddr,
				Handler:           wrappedHandler,
				TLSConfig:         tlsConfig.Clone(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			go func() {
				slog.Info("sqlproxy: starting mTLS listener", "addr", sqlProxyAddr)
				if err := wrappedSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
					slog.Error("sqlproxy: ListenAndServeTLS exited", "err", err)
				}
			}()
			sqlProxyShutdown = wrappedSrv.Shutdown
		}
	}

	// Wave 3 (Plan 02-07) — L4 credential vending service wiring.
	//
	// credvend.Service wraps the STS LRU cache + Prometheus counters.
	// The handler builds a per-tenant adapter per-request from CredStore
	// (same pattern as gateway/handler.go's adapterFor method).
	// kms.Client and kms.BatchCache support the Go-side KMS path (Pitfall 10
	// mitigation; Phase 4 encryption surface — not used in the current write path).
	//
	// CC3: reuse the existing awsConfig (when available) — DO NOT construct a
	// second AWS session. The KMS client uses the same config that SQS uses.
	//
	// The credvend cache is constructed unconditionally; if no AWS config is
	// available (local dev without NEKSUR_S3_EVENTS_QUEUE_URL), kmsClient is nil
	// and the KMS path is inactive. credvend.Service does NOT depend on KMS.
	credCache, err := credvend.NewCache(0) // 0 → defaultCacheSize 4096
	if err != nil {
		return fmt.Errorf("credvend cache: %w", err)
	}
	credService := credvend.NewService(
		credCache,
		observability.L4TokenIssuedTotal,
		observability.L4TokenRefreshTotal,
	)

	// KMS batch cache for Go-side DEK derivation (Pitfall 10 mitigation).
	// 10-minute TTL mirrors the JVM-side KmsKeyProvider batch lifetime.
	kmsBatchCache, err := kms.NewBatchCache(0, 10*time.Minute) // 0 → defaultCacheSize 4096
	if err != nil {
		return fmt.Errorf("kms batch cache: %w", err)
	}
	_ = kmsBatchCache // reserved for Phase 4 Go-side encryption path

	// CR-03 fail-fast startup guard: refuse to boot if any active
	// tenant is configured for a Phase-1-unsupported catalog kind
	// (glue, unity). The V0060 CHECK constraint permits these kinds
	// for future Phase 3 support, but BuildAdapter returns a stub
	// that fails every state-mutating call with iceberg.ErrAdapterStub
	// — leaving customers with a tenant on those kinds silently
	// 502-ing every commit. Failing the boot surfaces the
	// misconfiguration to operators immediately. The check is at the
	// admin-pool level (no per-tenant schema visits needed); the
	// query reads catalog_credentials rows from every tenant schema
	// via information_schema discovery.
	if err := assertNoUnsupportedCatalogs(ctx, pool); err != nil {
		return fmt.Errorf("phase 1 catalog guard: %w", err)
	}

	// HTTP router.
	mux := http.NewServeMux()

	// Authenticated API — every handler under /api/ runs under the
	// tenant middleware. apiHandler is a placeholder until M1 wires
	// the real REST surface.
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Demonstrate tenant ctx is present.
		tid, ok := tenant.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "tenant missing", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "ok tenant=%s", tid)
	})
	mux.Handle("/api/", workosauth.TenantMiddleware(workosClient, tenantRepo)(apiHandler))

	// Phase 1 L1 Catalog Gateway (Plan 01-06) — three new routes all
	// behind workosauth.TenantMiddleware (CC1 enforcement; the gateway
	// handler asserts tenant.IDFromContext as a defence-in-depth check).
	// Body cap is enforced inside each handler (http.MaxBytesReader 16MB).
	// AGE access goes through gateway.Deps.Graph (the same GraphClient
	// constructed above) so RLS + DISCARD ALL hold across the
	// per-request transaction lifecycle.
	mux.Handle("POST /v1/iceberg/{prefix}/namespaces/{namespace}/tables/{table}",
		workosauth.TenantMiddleware(workosClient, tenantRepo)(iceberggw.CommitHandler(gatewayDeps)))
	mux.Handle("POST /v1/iceberg/{prefix}/transactions/commit",
		workosauth.TenantMiddleware(workosClient, tenantRepo)(iceberggw.MultiTableCommitHandler(gatewayDeps)))
	// Plan 01-04 OpenLineage v2 receiver — wired here so it shares the
	// same TenantMiddleware enforcement as the gateway endpoints.
	mux.Handle("POST /v1/lineage",
		workosauth.TenantMiddleware(workosClient, tenantRepo)(lineagehttp.Handler(pool, gatewayDeps.IngestSvc)))

	// Plan 02-07 Wave 3 — L4 credential vending endpoint.
	// POST /v1/credvend/sts — behind workosauth.TenantMiddleware (D-2.09).
	// Request: {catalog_nickname, table_namespace, table_name, region}.
	// Response: {access_key_id, secret_access_key, session_token, expiration, region}.
	// Fail-closed to 503 on any Polaris error (D-1.09 carryover).
	mux.Handle("POST /v1/credvend/sts",
		workosauth.TenantMiddleware(workosClient, tenantRepo)(credvend.Handler(credvend.Deps{
			Service:   credService,
			CredStore: gatewayDeps.CredStore,
			// WR-13: surface the AdapterBuilder explicitly. Without the
			// field set the handler falls back to iceberggw.BuildAdapter
			// internally, but the dependency was invisible from the wiring
			// layer. Making it explicit lets the wire-layer review
			// catch a future caller that wants to inject an alternative.
			AdapterBuilder: iceberggw.BuildAdapter,
		})))

	// Phase 1 L3 detection (Plan 01-07) — goroutine pool + 3 trigger
	// sources (30s poller / Polaris webhook / S3 ObjectCreated SNS+SQS).
	// All sources push Hit{} to ONE channel; the pool's sync.Map dedup
	// ensures one scan per metadata_location per replica; cross-replica
	// dedup is the V0062 detection_runs.snapshot_metadata_location
	// UNIQUE constraint (Pitfall 10).
	slackClient := alerts.NewSlack(os.Getenv("NEKSUR_SLACK_WEBHOOK_URL"), "neksur-server")
	scanner := newRegexScanner(pool, graphClient, gatewayDeps.CredStore, slackClient)
	// WR-11: dispatch channel buffer size. The poller pushes up to
	// pollerMaxPerTenant=100 per tenant per 30s; with 100 tenants
	// that's 10,000 hits in one cycle. A buffer of 256 (the old
	// default) fills in milliseconds and the poller blocks on `case
	// in <- h` if workers stall. Default 1024 gives a 4x headroom
	// over the old size; production deployments with high-tenant
	// counts should bump via NEKSUR_L3_DISPATCH_BUFFER. Note: this
	// buffer is the queue between producers (poller / webhook /
	// s3-events) and consumers (the worker pool); a fuller queue is
	// observable via len(dispatchChan) if a metric is added later
	// (deferred).
	dispatchBuf := 1024
	if raw := os.Getenv("NEKSUR_L3_DISPATCH_BUFFER"); raw != "" {
		if n, parseErr := strconv.Atoi(raw); parseErr == nil && n > 0 {
			dispatchBuf = n
		}
	}
	dispatchChan := make(chan dispatch.Hit, dispatchBuf)
	dispatchPool := dispatch.NewPool(dispatchChan, scanner)
	go dispatchPool.Run(ctx)
	go dispatch.RunPoller(ctx, pool, graphClient, dispatchChan, dispatch.DefaultPollerInterval)

	// Polaris webhook handler — mounted OUTSIDE TenantMiddleware (HMAC
	// signature verification IS the auth check; per-tenant secret in
	// catalog_credentials.config_json[webhook_secret]).
	mux.Handle("POST /v1/webhooks/polaris", dispatch.WebhookHandler(pool, dispatchChan))

	// S3 ObjectCreated event consumer — OPTIONAL via env. When
	// NEKSUR_S3_EVENTS_QUEUE_URL is unset the consumer goroutine is
	// not started; the poller + webhook still cover the gateway-mediated
	// path. Phase 1 simplification: one queue per tenant, tenant ID
	// supplied via NEKSUR_S3_EVENTS_TENANT_ID env.
	if queueURL := os.Getenv("NEKSUR_S3_EVENTS_QUEUE_URL"); queueURL != "" {
		s3TenantID := os.Getenv("NEKSUR_S3_EVENTS_TENANT_ID")
		if s3TenantID == "" {
			fmt.Fprintln(os.Stderr,
				"NEKSUR_S3_EVENTS_QUEUE_URL is set but NEKSUR_S3_EVENTS_TENANT_ID is not — S3 event consumer not started")
		} else {
			awsCfg, err := config.LoadDefaultConfig(ctx)
			if err != nil {
				return fmt.Errorf("aws config: %w", err)
			}
			sqsClient := awssqs.NewFromConfig(awsCfg)
			go dispatch.RunS3EventConsumer(ctx, sqsClient, queueURL, s3TenantID, dispatchChan)
		}
	}

	// /callback — OAuth code → session cookie. Cookie attrs per
	// D-0.5.21 T-0.5-session-hijack: HttpOnly + Secure + SameSite=Lax
	// + Domain=.neksur.com + MaxAge=7d.
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		sess, err := workosClient.AuthenticateWithCode(r.Context(), code)
		if err != nil {
			http.Error(w, "authenticate failed", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     workosauth.CookieName,
			Value:    sess.AccessToken,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			Domain:   workosClient.CookieDomain(),
			Path:     "/",
			MaxAge:   7 * 24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	})

	// /webhooks/workos — sig verified BEFORE SCIM_ENABLED check.
	mux.HandleFunc("/webhooks/workos", workosClient.HandleWebhook(webhookSecret))

	// /webhooks/stripe (Plan 00.5-05) — sig verified BEFORE BILLING_ENABLED
	// check (D-0.5.21 T-0.5-stripe-spoof). The webhook secret MUST be set
	// even when BILLING_ENABLED=false; noopBilling still verifies sigs.
	stripeWebhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	billingCfg := billing.Config{
		Enabled:       os.Getenv("BILLING_ENABLED") == "true",
		APIKey:        os.Getenv("STRIPE_API_KEY"),
		WebhookSecret: stripeWebhookSecret,
	}
	billingInstance := billing.NewBilling(billingCfg)
	mux.HandleFunc("/webhooks/stripe", billing.WebhookHandler(billingInstance, billingCfg.WebhookSecret))

	// /admin/* (Plan 00.5-05) — admin UI gated on WorkOS internal_admin
	// org membership (T-0.5-admin-org-bypass). The internal_admin org id
	// comes from WORKOS_INTERNAL_ADMIN_ORG_ID env var (operator-provisioned
	// WorkOS Org for Neksur staff).
	internalAdminOrgID := os.Getenv("WORKOS_INTERNAL_ADMIN_ORG_ID")
	pagerdutyServiceID := os.Getenv("PAGERDUTY_SERVICE_ID")
	if pagerdutyServiceID == "" {
		pagerdutyServiceID = "P000000"
	}
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/admin/tenants", admin.ListTenants(pool))
	adminMux.HandleFunc("/admin/tenants/audit", admin.ViewTenantAuditLog(pool))
	adminMux.HandleFunc("/admin/incidents", admin.EmbedPagerDutyIncidents(pagerdutyServiceID))
	mux.Handle("/admin/", admin.AdminOrgGate(workosAdminGate{c: workosClient}, internalAdminOrgID)(adminMux))

	addr := os.Getenv("NEKSUR_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
		defer sc()
		_ = srv.Shutdown(shutdownCtx)
		if sqlProxyShutdown != nil {
			_ = sqlProxyShutdown(shutdownCtx)
		}
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}

// assertNoUnsupportedCatalogs scans every active tenant's
// `catalog_credentials` table looking for `catalog_kind IN ('glue',
// 'unity')` — the Phase-1-stub kinds whose BuildAdapter returns
// iceberg.ErrAdapterStub on every state-mutating call (REVIEW.md
// CR-03). Returns an error that lists every offending
// `(tenant_id, nickname, kind)` triple so operators can remediate
// before retry.
//
// Implementation: enumerate `tenant_*` schemas via
// information_schema.tables (one round-trip), then SELECT from
// each schema's catalog_credentials. Each tenant query runs as the
// admin pool — no per-tenant role / GUC dance needed because we
// are only checking config metadata.
//
// Phase 3 will lift this guard when the live Glue + Unity adapters
// ship (the BuildAdapter dispatch in
// internal/gateway/iceberg/forwarder.go will return a real adapter
// instead of the stub).
func assertNoUnsupportedCatalogs(ctx context.Context, adminPool *pgxpool.Pool) error {
	// Step 1 — discover tenant schemas via information_schema. The
	// canonical name shape is `tenant_<uuid-with-dashes-replaced>`
	// per internal/tenant/id.go::SchemaName.
	schemaRows, err := adminPool.Query(ctx, `
		SELECT schema_name
		FROM information_schema.schemata
		WHERE schema_name LIKE 'tenant\_%' ESCAPE '\'
	`)
	if err != nil {
		return fmt.Errorf("discover tenant schemas: %w", err)
	}
	var schemas []string
	for schemaRows.Next() {
		var s string
		if err := schemaRows.Scan(&s); err != nil {
			schemaRows.Close()
			return fmt.Errorf("scan schema row: %w", err)
		}
		schemas = append(schemas, s)
	}
	schemaRows.Close()
	if err := schemaRows.Err(); err != nil {
		return fmt.Errorf("schemas rows.Err: %w", err)
	}

	// Step 2 — for each schema, look for unsupported catalog kinds.
	// Schema names from information_schema are server-controlled but
	// we still pgx.Identifier.Sanitize for defence-in-depth.
	type offender struct {
		schema   string
		nickname string
		kind     string
	}
	var offenders []offender
	for _, schema := range schemas {
		qSchema := pgx.Identifier{schema}.Sanitize()
		// catalog_credentials may not exist on schemas that haven't
		// been migrated through Plan 01-06 (V0060). Use to_regclass
		// to check first; missing tables are NOT an error here (they
		// indicate an old / partial tenant, which the relational
		// migrate path handles separately).
		var hasTable bool
		err := adminPool.QueryRow(ctx, fmt.Sprintf(
			"SELECT to_regclass('%s.catalog_credentials') IS NOT NULL", schema,
		)).Scan(&hasTable)
		if err != nil || !hasTable {
			continue
		}
		rows, err := adminPool.Query(ctx, fmt.Sprintf(
			"SELECT nickname, catalog_kind FROM %s.catalog_credentials WHERE catalog_kind IN ('glue','unity')",
			qSchema,
		))
		if err != nil {
			return fmt.Errorf("query %s.catalog_credentials: %w", schema, err)
		}
		for rows.Next() {
			var nick, kind string
			if err := rows.Scan(&nick, &kind); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s row: %w", schema, err)
			}
			offenders = append(offenders, offender{schema: schema, nickname: nick, kind: kind})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("%s rows.Err: %w", schema, err)
		}
	}

	if len(offenders) == 0 {
		return nil
	}
	var msg strings.Builder
	msg.WriteString("Phase 1 only supports catalog_kind IN ('polaris','nessie'). ")
	msg.WriteString("The following tenant catalog_credentials rows are configured for a stub kind ")
	msg.WriteString("whose adapter returns iceberg.ErrAdapterStub on every commit:\n")
	for _, o := range offenders {
		fmt.Fprintf(&msg, "  - schema=%s nickname=%q kind=%s\n", o.schema, o.nickname, o.kind)
	}
	msg.WriteString("Remediation: UPDATE catalog_credentials SET catalog_kind = 'polaris' (or 'nessie') ")
	msg.WriteString("and re-supply config_json. Glue + Unity adapters arrive in Phase 3.")
	return errors.New(msg.String())
}

// tenantExistsAdapter satisfies compiler.TenantValidator. The lookup
// is a single SELECT against public.tenants (admin-scoped row; the
// admin pool is the same pool the rest of runWithSaasAuth uses).
type tenantExistsAdapter struct {
	pool *pgxpool.Pool
}

func (a *tenantExistsAdapter) TenantExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := a.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM public.tenants WHERE id = $1 AND lifecycle_state = 'active')`,
		id,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("tenantExistsAdapter.TenantExists: %w", err)
	}
	return exists, nil
}

// policyLoaderStub satisfies compiler.PolicyLoader. The production
// loader (a thin wrapper over policystore.AGEStore exposing a
// LoadPolicyByID surface) lands in Plan 02-05 alongside the SQL
// proxy's per-policy fetch path. Until then the trigger logs the
// NOTIFY and returns errPolicyLoaderNotWired; the compiler.Trigger
// treats this as a skip (logs + continues) so the LISTEN goroutine
// stays alive for end-to-end smoke tests.
type policyLoaderStub struct{}

var errPolicyLoaderNotWired = errors.New("policy loader: not yet wired (Plan 02-05)")

func (policyLoaderStub) LoadPolicyForCompile(ctx context.Context, policyID string) (compiler.PolicySource, iceberg.TableRef, error) {
	_ = ctx
	_ = policyID
	return compiler.PolicySource{}, iceberg.TableRef{}, errPolicyLoaderNotWired
}
