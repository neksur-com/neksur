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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	workosauth "github.com/neksur-com/neksur/internal/auth/workos"
	"github.com/neksur-com/neksur/internal/admin"
	"github.com/neksur-com/neksur/internal/billing"
	"github.com/neksur-com/neksur/internal/catalog"
	iceberggw "github.com/neksur-com/neksur/internal/gateway/iceberg"
	"github.com/neksur-com/neksur/internal/graph"
	"github.com/neksur-com/neksur/internal/ingest"
	lineagehttp "github.com/neksur-com/neksur/internal/lineage/http"
	celpolicy "github.com/neksur-com/neksur/internal/policy/cel"
	policystore "github.com/neksur-com/neksur/internal/policy/store"
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

	gatewayDeps := iceberggw.Deps{
		Pool:        pool,
		Graph:       graphClient,
		CredStore:   catalog.NewRepo(pool),
		PolicyStore: policystore.NewAGEStore(graphClient),
		Evaluator:   celpolicy.NewEvaluator(celCompiler),
		IngestSvc:   ingest.NewService(graphClient),
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
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}
