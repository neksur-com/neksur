package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubGate is the test double for AdminGate.
type stubGate struct {
	orgID string
	err   error
}

func (s *stubGate) LoadSessionOrgID(_ *http.Request) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.orgID, nil
}

// TestAdminOrgGateAllowsAdminOrg: a session whose org_id matches the
// configured internal_admin org id passes through to the wrapped
// handler. T-0.5-admin-org-bypass mitigation.
func TestAdminOrgGateAllowsAdminOrg(t *testing.T) {
	const adminOrg = "org_internal_admin"
	gate := &stubGate{orgID: adminOrg}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("admin-only"))
	})
	wrapped := AdminOrgGate(gate, adminOrg)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "admin-only") {
		t.Fatalf("expected admin-only body, got %q", body)
	}
}

// TestAdminOrgGateRejectsNonAdminOrg: a session whose org_id does NOT
// match returns 403. T-0.5-admin-org-bypass mitigation.
func TestAdminOrgGateRejectsNonAdminOrg(t *testing.T) {
	gate := &stubGate{orgID: "org_some_customer"}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("inner handler should not be reached")
	})
	wrapped := AdminOrgGate(gate, "org_internal_admin")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// TestAdminOrgGateRejectsUnauthorized: a session lookup error → 401.
func TestAdminOrgGateRejectsUnauthorized(t *testing.T) {
	gate := &stubGate{err: errors.New("session invalid")}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("inner handler should not be reached on unauthorized")
	})
	wrapped := AdminOrgGate(gate, "org_internal_admin")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// TestAdminOrgGateMisconfigured: empty internalAdminOrgID is a deploy
// error — return 500 rather than silently letting any session through.
func TestAdminOrgGateMisconfigured(t *testing.T) {
	gate := &stubGate{orgID: ""}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("inner handler should not be reached when gate is misconfigured")
	})
	wrapped := AdminOrgGate(gate, "" /* unset */)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (misconfigured), got %d", rec.Code)
	}
}

// TestEmbedPagerDutyIncidentsRenders: the iframe wrapper page contains
// the configured PagerDuty service ID in its HTML.
func TestEmbedPagerDutyIncidentsRenders(t *testing.T) {
	handler := EmbedPagerDutyIncidents("P01ABC2")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/incidents", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "P01ABC2") {
		t.Fatalf("expected PagerDuty service id in body, got: %s", body)
	}
	if !strings.Contains(body, "<iframe") {
		t.Fatalf("expected iframe embed, got: %s", body)
	}
}
