package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryRejectsForbiddenHighCardinalityLabels(t *testing.T) {
	reg := NewRegistry()
	forbidden := []string{"tenant_id", "incident_id", "job_id", "api_key", "key_id", "token", "secret", "principal_id", "ip", "client_ip"}
	for _, label := range forbidden {
		if _, err := reg.RegisterCounter("bad_counter", "help", label); err == nil {
			t.Fatalf("RegisterCounter accepted forbidden label %q", label)
		}
		if _, err := reg.RegisterGauge("bad_gauge", "help", label); err == nil {
			t.Fatalf("RegisterGauge accepted forbidden label %q", label)
		}
		if _, err := reg.RegisterHistogram("bad_hist", "help", nil, label); err == nil {
			t.Fatalf("RegisterHistogram accepted forbidden label %q", label)
		}
	}
}

func TestPrometheusExpositionFormatAndScrapeHooks(t *testing.T) {
	m := NewSentinelMetrics()

	m.HTTPRequestsTotal.Inc("POST", "/v1/incidents", "201")
	m.HTTPRequestsTotal.Inc("POST", "/v1/incidents", "201")
	m.HTTPRequestDuration.Observe(0.015, "POST", "/v1/incidents")
	m.HTTPRateLimitRejections.Inc("pre_auth_ip")
	m.HTTPAuthFailures.Inc("invalid")
	m.WorkerJobsClaimed.Inc()
	m.WorkerJobsCompleted.Inc()

	scraped := false
	m.Registry.OnScrape(func() {
		scraped = true
		m.WorkerQueueDepth.Set(3)
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Registry.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !scraped {
		t.Fatal("OnScrape hook was not invoked")
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain; version=0.0.4") {
		t.Fatalf("unexpected Content-Type: %q", ct)
	}

	body := rec.Body.String()
	expectedLines := []string{
		`# TYPE sentinel_http_requests_total counter`,
		`sentinel_http_requests_total{method="POST",route="/v1/incidents",status="201"} 2`,
		`# TYPE sentinel_http_request_duration_seconds histogram`,
		`sentinel_http_request_duration_seconds_count{method="POST",route="/v1/incidents"} 1`,
		`sentinel_http_rate_limit_rejections_total{stage="pre_auth_ip"} 1`,
		`sentinel_http_auth_failures_total{reason="invalid"} 1`,
		`sentinel_worker_queue_depth 3`,
	}
	for _, want := range expectedLines {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\nbody:\n%s", want, body)
		}
	}
}
