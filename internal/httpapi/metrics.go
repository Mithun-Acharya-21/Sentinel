package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"sentinel/internal/metrics"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// normalizeMetricRoute maps dynamic path parameters (such as incident UUIDs) and unknown
// scanner paths to a fixed, bounded set of route labels.
func normalizeMetricRoute(path string) string {
	switch {
	case path == "/v1/incidents":
		return "/v1/incidents"
	case strings.HasPrefix(path, "/v1/incidents/") && strings.HasSuffix(path, "/triage/approve"):
		return "/v1/incidents/{id}/triage/approve"
	case strings.HasPrefix(path, "/v1/incidents/") && strings.HasSuffix(path, "/triage/reject"):
		return "/v1/incidents/{id}/triage/reject"
	case strings.HasPrefix(path, "/v1/incidents/") && strings.HasSuffix(path, "/triage"):
		return "/v1/incidents/{id}/triage"
	case strings.HasPrefix(path, "/v1/incidents/") && strings.HasSuffix(path, "/remediate"):
		return "/v1/incidents/{id}/remediate"
	case strings.HasPrefix(path, "/v1/incidents/") && strings.HasSuffix(path, "/remediation"):
		return "/v1/incidents/{id}/remediation"
	case strings.HasPrefix(path, "/v1/incidents/"):
		return "/v1/incidents/{id}"
	case path == "/health/live":
		return "/health/live"
	case path == "/health/ready":
		return "/health/ready"
	case path == "/metrics":
		return "/metrics"
	default:
		return "/other"
	}
}

// MetricsMiddleware records bounded-cardinality HTTP request counts and duration histograms.
func MetricsMiddleware(m *metrics.SentinelMetrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == nil {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			route := normalizeMetricRoute(r.URL.Path)
			m.HTTPRequestsTotal.Inc(r.Method, route, strconv.Itoa(rec.status))
			m.HTTPRequestDuration.Observe(time.Since(start).Seconds(), r.Method, route)
		})
	}
}
