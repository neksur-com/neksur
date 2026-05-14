//go:build integration

// Plan 00.5-05 Task 3 — pgwire reachability smoke test for
// REQ-saas-cloud-topology + RESEARCH Pitfall 6 (cross-VPC DNS resolution).
//
// This is a NETWORK-TOPOLOGY probe that requires a real AWS sandbox
// environment to be meaningful (LocalStack only partially implements
// VPC peering). Plan 05's job is to ship the test code; the actual
// sandbox-attestation run happens in Plan 07 Task 4 (the
// checkpoint:human-verify gate).
//
// Skip-gate contract: when AWS_SANDBOX_ENABLED != "true" the test
// t.Skip()s with a message pointing at Plan 07 Task 4. When enabled,
// the test:
//   1. Resolves the Neksur pgwire endpoint (env NEKSUR_PGWIRE_HOST).
//   2. Asserts the resolved A record is in 10.0.0.0/16 (RESEARCH
//      Pitfall 6 line 1132 — proves DNS resolution is routed via VPC
//      peering, not the public internet).
//   3. TCP-dials port 5432 with a short backoff (RESEARCH Pitfall 5).
//
// The test runs from a "customer" subnet — caller's responsibility
// (Plan 07 attestation runbook) — and exercises the full peering
// chain end-to-end.

package integration

import (
	"net"
	"os"
	"testing"
	"time"
)

// TestPgwireReachableFromCustomerVPC — REQ-saas-cloud-topology /
// VALIDATION row "TestPgwireReachableFromCustomerVPC".
func TestPgwireReachableFromCustomerVPC(t *testing.T) {
	if os.Getenv("AWS_SANDBOX_ENABLED") != "true" {
		t.Skip("AWS_SANDBOX_ENABLED!=true; sandbox attestation deferred to Plan 07 Task 4")
	}

	host := os.Getenv("NEKSUR_PGWIRE_HOST")
	if host == "" {
		t.Fatal("AWS_SANDBOX_ENABLED=true but NEKSUR_PGWIRE_HOST is unset")
	}

	// 1. DNS resolution should yield an A record in 10.0.0.0/16.
	ips, err := net.LookupIP(host)
	if err != nil {
		t.Fatalf("LookupIP(%q): %v", host, err)
	}
	if len(ips) == 0 {
		t.Fatalf("LookupIP(%q) returned no IPs", host)
	}

	_, neksurNet, err := net.ParseCIDR("10.0.0.0/16")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}

	found := false
	for _, ip := range ips {
		ip4 := ip.To4()
		if ip4 == nil {
			continue // skip IPv6 records
		}
		if neksurNet.Contains(ip4) {
			found = true
			t.Logf("resolved %s → %s (in 10.0.0.0/16, via peering DNS — Pitfall 6 OK)", host, ip4.String())
			break
		}
	}
	if !found {
		var seen []string
		for _, ip := range ips {
			seen = append(seen, ip.String())
		}
		t.Fatalf("RESEARCH Pitfall 6 violation: %q resolved to %v; expected an A record in 10.0.0.0/16. "+
			"Check `allow_remote_vpc_dns_resolution=true` on BOTH sides and `enable_dns_hostnames=true` on the customer VPC.",
			host, seen)
	}

	// 2. TCP-dial port 5432 with a short backoff (Pitfall 5: RDS endpoint
	// may not have stabilised right after a fresh peering apply).
	const port = "5432"
	target := net.JoinHostPort(host, port)
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err == nil {
			_ = conn.Close()
			t.Logf("TCP dial %s succeeded", target)
			return
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("TCP dial %s never succeeded within 60s: %v", target, lastErr)
}
