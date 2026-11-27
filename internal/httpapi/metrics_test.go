package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/metrics"
)

func TestHTTPMetricsMiddlewareAndPrometheusLabelsSafety(t *testing.T) {
	prom := metrics.NewSentinelMetrics()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	preAuthLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  2,
		Clock:          clock,
		PromMetrics:    prom,
	})
	principalLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 10.0,
		BurstCapacity:  10,
		Clock:          clock,
		PromMetrics:    prom,
	})

	store := &stubCredentialStore{creds: make(map[string]auth.StoredCredential)}
	validKey := provisionTestKey(t, store, "secret-tenant-xyz", "svc-obs", []string{auth.ScopeIncidentsRead})
	verifier := auth.NewStoreVerifier(store)
	repo := &fakeRepository{got: sampleIncident()}

	root := http.NewServeMux()
	root.Handle("GET /metrics", prom.Registry.Handler())
	root.Handle("/", NewProtectedHandlerWithObservability(repo, verifier, preAuthLimiter, principalLimiter, prom))

	// 1. Authenticated GET -> 200
	reqOK := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqOK.RemoteAddr = "198.51.100.10:1111"
	reqOK.Header.Set("Authorization", "Bearer "+validKey.PlaintextToken)
	recOK := httptest.NewRecorder()
	root.ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recOK.Code)
	}

	// 2. Missing auth -> 401
	reqMissing := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqMissing.RemoteAddr = "198.51.100.11:1111"
	root.ServeHTTP(httptest.NewRecorder(), reqMissing)

	// 3. Invalid auth -> 401
	reqInvalid := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqInvalid.RemoteAddr = "198.51.100.11:1111"
	reqInvalid.Header.Set("Authorization", "Bearer snt_0000000000000000_invalidsecret123456")
	root.ServeHTTP(httptest.NewRecorder(), reqInvalid)

	// 4. 3rd request from 198.51.100.11 -> 429 pre_auth_ip
	reqLimit := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqLimit.RemoteAddr = "198.51.100.11:1111"
	recLimit := httptest.NewRecorder()
	root.ServeHTTP(recLimit, reqLimit)
	if recLimit.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recLimit.Code)
	}

	// Scrape /metrics
	recMetrics := httptest.NewRecorder()
	root.ServeHTTP(recMetrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recMetrics.Body.String()

	if !strings.Contains(body, `sentinel_http_requests_total{method="GET",route="/v1/incidents/{id}",status="200"} 1`) {
		t.Fatalf("missing 200 request metric in:\n%s", body)
	}
	if !strings.Contains(body, `sentinel_http_requests_total{method="GET",route="/v1/incidents/{id}",status="401"} 2`) {
		t.Fatalf("missing 401 request metric in:\n%s", body)
	}
	if !strings.Contains(body, `sentinel_http_requests_total{method="GET",route="/v1/incidents/{id}",status="429"} 1`) {
		t.Fatalf("missing 429 request metric in:\n%s", body)
	}
	if !strings.Contains(body, `sentinel_http_rate_limit_rejections_total{stage="pre_auth_ip"} 1`) {
		t.Fatalf("missing pre_auth_ip rejection metric in:\n%s", body)
	}
	if !strings.Contains(body, `sentinel_http_auth_failures_total{reason="missing"} 1`) ||
		!strings.Contains(body, `sentinel_http_auth_failures_total{reason="invalid"} 1`) {
		t.Fatalf("missing auth failure metrics in:\n%s", body)
	}

	// Verify no sensitive or high-cardinality values leaked into /metrics
	for _, forbidden := range []string{testID, "secret-tenant-xyz", validKey.PlaintextToken, validKey.KeyID, "198.51.100.10", "198.51.100.11"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics output leaked sensitive/unbounded value %q", forbidden)
		}
	}
	_ = context.Background()
}
