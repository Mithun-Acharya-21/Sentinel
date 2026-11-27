package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sentinel/internal/auth"
)

func TestTokenBucketInitialBurstAndRejection(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  5,
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	clientKey := "client-alpha"

	// First 5 requests should all be allowed immediately (burst capacity)
	for i := 1; i <= 5; i++ {
		allowed, wait := limiter.Allow(clientKey)
		if !allowed {
			t.Fatalf("request %d was unexpectedly rejected; wait=%v", i, wait)
		}
		if wait != 0 {
			t.Fatalf("request %d had non-zero wait: %v", i, wait)
		}
	}

	// 6th request at the exact same instant must be rejected
	allowed, wait := limiter.Allow(clientKey)
	if allowed {
		t.Fatalf("request 6 was allowed when burst capacity should be exhausted")
	}
	// At refillRate = 1.0 token/sec, deficit = 1.0, waitSec = 1
	if wait != 1*time.Second {
		t.Fatalf("wait = %v, want 1s", wait)
	}

	metrics := limiter.Metrics()
	if metrics.AllowedRequests != 5 {
		t.Fatalf("AllowedRequests = %d, want 5", metrics.AllowedRequests)
	}
	if metrics.RejectedRequests != 1 {
		t.Fatalf("RejectedRequests = %d, want 1", metrics.RejectedRequests)
	}
	if math.Abs(metrics.RejectionRate-(1.0/6.0)) > 1e-4 {
		t.Fatalf("RejectionRate = %f, want %f", metrics.RejectionRate, 1.0/6.0)
	}
}

func TestTokenBucketRefillOverTime(t *testing.T) {
	currentTime := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return currentTime }

	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 2.0, // 2 tokens per second
		BurstCapacity:  3,
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	clientKey := "client-refill"

	// Exhaust 3 tokens
	for i := 0; i < 3; i++ {
		allowed, _ := limiter.Allow(clientKey)
		if !allowed {
			t.Fatalf("burst request %d rejected", i)
		}
	}

	// Immediately rejected
	allowed, _ := limiter.Allow(clientKey)
	if allowed {
		t.Fatal("expected rejection when empty")
	}

	// Advance clock by 500ms -> 0.5s * 2.0 tokens/s = 1.0 token refilled
	currentTime = currentTime.Add(500 * time.Millisecond)

	allowed, wait := limiter.Allow(clientKey)
	if !allowed {
		t.Fatalf("expected admission after 500ms refill; wait=%v", wait)
	}

	// Next immediate request is rejected again
	allowed, _ = limiter.Allow(clientKey)
	if allowed {
		t.Fatal("expected rejection immediately after consuming refilled token")
	}

	// Advance clock by 10 seconds -> should cap at burst capacity (3 tokens)
	currentTime = currentTime.Add(10 * time.Second)

	for i := 0; i < 3; i++ {
		allowed, _ := limiter.Allow(clientKey)
		if !allowed {
			t.Fatalf("capped token %d rejected after idle", i)
		}
	}

	allowed, _ = limiter.Allow(clientKey)
	if allowed {
		t.Fatal("tokens exceeded burst capacity cap")
	}
}

func TestRetryAfterCalculation(t *testing.T) {
	currentTime := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return currentTime }

	// refillRate = 0.5 token/sec (1 token every 2 seconds)
	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 0.5,
		BurstCapacity:  1,
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	clientKey := "slow-client"

	// Consume the 1 token
	allowed, _ := limiter.Allow(clientKey)
	if !allowed {
		t.Fatal("initial request rejected")
	}

	// Now empty. Deficit = 1.0. 1.0 / 0.5 = 2.0s -> wait = 2s
	allowed, wait := limiter.Allow(clientKey)
	if allowed {
		t.Fatal("expected rejection")
	}
	if wait != 2*time.Second {
		t.Fatalf("wait = %v, want 2s", wait)
	}

	// Advance clock by 1s. Deficit = 0.5. 0.5 / 0.5 = 1.0s -> wait = 1s
	currentTime = currentTime.Add(1 * time.Second)
	allowed, wait = limiter.Allow(clientKey)
	if allowed {
		t.Fatal("expected rejection")
	}
	if wait != 1*time.Second {
		t.Fatalf("wait = %v, want 1s", wait)
	}
}

func TestClientIdentityResolution(t *testing.T) {
	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 10.0,
		BurstCapacity:  20,
		TrustedProxies: []string{"10.0.0.1", "172.16.0.0/16"},
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	t.Run("VerifiedPrincipalContextTakesPrecedenceAndIsHashed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req = req.WithContext(WithVerifiedPrincipal(req.Context(), "tenant-verified-123"))
		req.RemoteAddr = "192.168.1.1:54321"

		key := limiter.ExtractClientKey(req)
		if !strings.HasPrefix(key, "principal:") {
			t.Fatalf("key = %q, want prefix 'principal:'", key)
		}
		if strings.Contains(key, "tenant-verified-123") {
			t.Fatalf("key = %q leaked raw principal string", key)
		}
	})

	t.Run("UnverifiedBearerAndAPIKeyHeadersIgnored", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.Header.Set("Authorization", "Bearer fake-token-xyz")
		req.Header.Set("X-API-Key", "fake-api-key-123")
		req.RemoteAddr = "192.168.1.1:54321"

		key := limiter.ExtractClientKey(req)
		if key != "ip:192.168.1.1" {
			t.Fatalf("key = %q, want 'ip:192.168.1.1' (unverified auth headers must be ignored)", key)
		}
	})

	t.Run("PeerIPFromUntrustedRemoteAddrIgnoresSpoofedXFF", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "203.0.113.195:44321"
		req.Header.Set("X-Forwarded-For", "198.51.100.22, 1.1.1.1")

		key := limiter.ExtractClientKey(req)
		if key != "ip:203.0.113.195" {
			t.Fatalf("key = %q, want 'ip:203.0.113.195' (untrusted peer XFF must be ignored)", key)
		}
	})

	t.Run("TrustedProxyParsesXFF", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "10.0.0.1:44321" // Matches trusted proxy
		req.Header.Set("X-Forwarded-For", "203.0.113.44, 10.0.0.1")

		key := limiter.ExtractClientKey(req)
		if key != "ip:203.0.113.44" {
			t.Fatalf("key = %q, want 'ip:203.0.113.44' from trusted proxy chain", key)
		}
	})

	t.Run("TrustedCIDRParsesXFF", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "172.16.5.99:32111" // Matches 172.16.0.0/16
		req.Header.Set("X-Forwarded-For", "198.51.100.88")

		key := limiter.ExtractClientKey(req)
		if key != "ip:198.51.100.88" {
			t.Fatalf("key = %q, want 'ip:198.51.100.88'", key)
		}
	})
}

func TestIndependentClientBuckets(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	limiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  2,
		Clock:          clock,
	})

	// Client A exhausts their 2 tokens
	limiter.Allow("client-A")
	limiter.Allow("client-A")
	allowedA, _ := limiter.Allow("client-A")
	if allowedA {
		t.Fatal("client A should be exhausted")
	}

	// Client B should still have full capacity
	allowedB1, _ := limiter.Allow("client-B")
	allowedB2, _ := limiter.Allow("client-B")
	if !allowedB1 || !allowedB2 {
		t.Fatal("client B tokens should not be affected by client A")
	}
	allowedB3, _ := limiter.Allow("client-B")
	if allowedB3 {
		t.Fatal("client B should be exhausted on 3rd request")
	}
}

func TestBucketExpirationAndCleanup(t *testing.T) {
	currentTime := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return currentTime }

	ttl := 5 * time.Minute
	limiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 10.0,
		BurstCapacity:  20,
		BucketTTL:      ttl,
		Clock:          clock,
	})

	limiter.Allow("client-old")

	// Advance clock by 2 minutes and add new client
	currentTime = currentTime.Add(2 * time.Minute)
	limiter.Allow("client-recent")

	if metrics := limiter.Metrics(); metrics.ActiveBuckets != 2 {
		t.Fatalf("ActiveBuckets = %d, want 2", metrics.ActiveBuckets)
	}

	// Advance clock so client-old exceeds TTL (total 6 minutes since client-old, 4 minutes since client-recent)
	currentTime = currentTime.Add(4 * time.Minute)

	evicted := limiter.Cleanup(currentTime)
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}

	metrics := limiter.Metrics()
	if metrics.ActiveBuckets != 1 {
		t.Fatalf("ActiveBuckets = %d, want 1", metrics.ActiveBuckets)
	}

	// Verify client-recent was kept
	limiter.mu.RLock()
	_, oldExists := limiter.buckets["client-old"]
	_, recentExists := limiter.buckets["client-recent"]
	limiter.mu.RUnlock()

	if oldExists {
		t.Fatal("client-old should have been evicted")
	}
	if !recentExists {
		t.Fatal("client-recent should still exist")
	}
}

func TestConcurrentBucketAccess(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	limiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 10.0,
		BurstCapacity:  50,
		Clock:          clock,
	})

	const goroutines = 20
	const requestsPerGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for r := 0; r < requestsPerGoroutine; r++ {
				// All hammer the same client key
				limiter.Allow("shared-client")
			}
		}(g)
	}

	wg.Wait()

	metrics := limiter.Metrics()
	total := metrics.AllowedRequests + metrics.RejectedRequests
	if total != goroutines*requestsPerGoroutine {
		t.Fatalf("total requests = %d, want %d", total, goroutines*requestsPerGoroutine)
	}
	if metrics.AllowedRequests != 50 {
		t.Fatalf("AllowedRequests = %d, want 50 (burst capacity cap)", metrics.AllowedRequests)
	}
	if metrics.RejectedRequests != (goroutines*requestsPerGoroutine - 50) {
		t.Fatalf("RejectedRequests = %d, want %d", metrics.RejectedRequests, goroutines*requestsPerGoroutine-50)
	}
}

func TestRateLimitMiddlewareIntegration(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  2,
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	middleware := RateLimitMiddleware(limiter)
	handler := middleware(targetHandler)

	// First 2 requests should succeed (200 OK)
	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, rec.Code)
		}
	}

	// 3rd request should fail with 429
	req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 Too Many Requests; body=%s", rec.Code, rec.Body)
	}

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("missing Retry-After header")
	}
	sec, err := strconv.Atoi(retryAfter)
	if err != nil || sec < 1 {
		t.Fatalf("invalid Retry-After value: %q", retryAfter)
	}

	var errResp errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if errResp.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want 'rate_limit_exceeded'", errResp.Error.Code)
	}
}

func TestHealthEndpointsExemptFromRateLimiting(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	limiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 0.1,
		BurstCapacity:  1,
		Clock:          clock,
	})

	// Exhaust the client's quota
	limiter.Allow("ip:192.0.2.99")

	healthHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	})

	handler := RateLimitMiddleware(limiter)(healthHandler)

	// Liveness and readiness endpoints should succeed even when client is rate limited
	for _, path := range []string{"/health/live", "/health/ready"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.0.2.99:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("path %s status = %d, want 200 (health check must not be rate limited)", path, rec.Code)
		}
	}
}

func TestRateLimiterWithIncidentIdempotency(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	limiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  2,
		Clock:          clock,
	})

	repo := &fakeRepository{created: sampleIncident()}
	apiHandler := New(repo)
	handler := RateLimitMiddleware(limiter)(apiHandler)

	clientAddr := "192.168.1.100:8000"

	// 1. Initial creation (allowed)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(sampleJSON()))
	req1 = req1.WithContext(auth.WithPrincipal(req1.Context(), testPrincipal("tenant-demo")))
	req1.RemoteAddr = clientAddr
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("req1 status = %d, want 201", rec1.Code)
	}
	if repo.CreateCalls() != 1 {
		t.Fatalf("repo.CreateCalls() = %d, want 1", repo.CreateCalls())
	}

	// 2. Idempotent replay (allowed, consumes 2nd token)
	repo.SetCreateReplay(true)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(sampleJSON()))
	req2 = req2.WithContext(auth.WithPrincipal(req2.Context(), testPrincipal("tenant-demo")))
	req2.RemoteAddr = clientAddr
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200", rec2.Code)
	}
	if repo.CreateCalls() != 2 {
		t.Fatalf("repo.CreateCalls() = %d, want 2", repo.CreateCalls())
	}

	// 3. 3rd request should be rejected by rate limiter WITHOUT invoking repository
	req3 := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(sampleJSON()))
	req3 = req3.WithContext(auth.WithPrincipal(req3.Context(), testPrincipal("tenant-demo")))
	req3.RemoteAddr = clientAddr
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("req3 status = %d, want 429", rec3.Code)
	}
	// Repository CreateCalls must remain 2 (storage was protected from unneeded query)
	if repo.CreateCalls() != 2 {
		t.Fatalf("repo.CreateCalls() = %d, want 2 (repository should not be invoked when throttled)", repo.CreateCalls())
	}
}

func TestAdversarialHeaderRotationBypassPrevented(t *testing.T) {
	now := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  2,
		Clock:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	handler := RateLimitMiddleware(limiter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 1. Changing fake Bearer tokens from the same IP must share the same IP bucket
	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/incidents", nil)
		req.RemoteAddr = "198.51.100.77:5555"
		req.Header.Set("Authorization", "Bearer attacker-fake-token-"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, rec.Code)
		}
	}

	// 3rd request with a different fake Bearer token must be rejected
	reqBearer := httptest.NewRequest(http.MethodPost, "/v1/incidents", nil)
	reqBearer.RemoteAddr = "198.51.100.77:5555"
	reqBearer.Header.Set("Authorization", "Bearer attacker-fake-token-3")
	recBearer := httptest.NewRecorder()
	handler.ServeHTTP(recBearer, reqBearer)
	if recBearer.Code != http.StatusTooManyRequests {
		t.Fatalf("fake Bearer rotation bypassed rate limit: status = %d, want 429", recBearer.Code)
	}

	// 4th request from same IP switching to a fake X-API-Key must ALSO be rejected
	reqAPIKey := httptest.NewRequest(http.MethodPost, "/v1/incidents", nil)
	reqAPIKey.RemoteAddr = "198.51.100.77:5555"
	reqAPIKey.Header.Set("X-API-Key", "attacker-fake-api-key-999")
	recAPIKey := httptest.NewRecorder()
	handler.ServeHTTP(recAPIKey, reqAPIKey)
	if recAPIKey.Code != http.StatusTooManyRequests {
		t.Fatalf("fake X-API-Key rotation bypassed rate limit: status = %d, want 429", recAPIKey.Code)
	}

	// Verify only 1 bucket was created for that IP
	if metrics := limiter.Metrics(); metrics.ActiveBuckets != 1 {
		t.Fatalf("ActiveBuckets = %d, want 1 (header rotation must not allocate new buckets)", metrics.ActiveBuckets)
	}
}

func TestAdversarialProxyAndForwardingEdgeCases(t *testing.T) {
	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 10.0,
		BurstCapacity:  20,
		TrustedProxies: []string{"10.0.0.0/8", "fc00::/7"},
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	t.Run("UntrustedPeerSpoofsXFFAndXRealIP", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		req.Header.Set("X-Real-IP", "198.51.100.2")

		if key := limiter.ExtractClientKey(req); key != "ip:203.0.113.9" {
			t.Fatalf("key = %q, want ip:203.0.113.9", key)
		}
	})

	t.Run("MultipleTrustedProxiesAndMultipleXFFHeaders", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "10.1.2.3:443"
		req.Header.Add("X-Forwarded-For", "198.51.100.10, 203.0.113.50")
		req.Header.Add("X-Forwarded-For", "10.0.0.5, 10.0.0.6")

		// Right-to-left: 10.0.0.6 (trusted), 10.0.0.5 (trusted), 203.0.113.50 (first untrusted)
		if key := limiter.ExtractClientKey(req); key != "ip:203.0.113.50" {
			t.Fatalf("key = %q, want ip:203.0.113.50", key)
		}
	})

	t.Run("MalformedXFFBreaksSafelyToPeerIP", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "10.0.0.1:443"
		// Attacker injects 198.51.100.99 behind a malformed token
		req.Header.Set("X-Forwarded-For", "198.51.100.99, not-an-ip")

		if key := limiter.ExtractClientKey(req); key != "ip:10.0.0.1" {
			t.Fatalf("key = %q, want ip:10.0.0.1 (malformed XFF must not fall through to earlier untrusted IP)", key)
		}
	})

	t.Run("IPv6CanonicalizationAndBracketedRemoteAddr", func(t *testing.T) {
		req1 := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req1.RemoteAddr = "[2001:0db8:0000:0000:0000:0000:0000:0001]:54321"
		req2 := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req2.RemoteAddr = "[2001:db8::1]"

		k1 := limiter.ExtractClientKey(req1)
		k2 := limiter.ExtractClientKey(req2)
		if k1 != "ip:2001:db8::1" || k2 != "ip:2001:db8::1" {
			t.Fatalf("IPv6 keys not canonicalized: k1=%q k2=%q", k1, k2)
		}
	})

	t.Run("TrustedIPv6ProxyWithIPv6Client", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = "[fc00::1]:8443"
		req.Header.Set("X-Forwarded-For", "2001:db8::42, fc00::2")

		if key := limiter.ExtractClientKey(req); key != "ip:2001:db8::42" {
			t.Fatalf("key = %q, want ip:2001:db8::42", key)
		}
	})

	t.Run("EmptyRemoteAddrHandledSafely", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		req.RemoteAddr = ""

		if key := limiter.ExtractClientKey(req); key != "ip:unknown" {
			t.Fatalf("key = %q, want ip:unknown", key)
		}
	})
}

func TestBucketExhaustionBoundsMemoryAndPreservesActiveClients(t *testing.T) {
	currentTime := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  2,
		BucketTTL:      5 * time.Minute,
		MaxBuckets:     3,
		Clock:          func() time.Time { return currentTime },
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	// Fill up to MaxBuckets=3 with active clients
	for _, k := range []string{"ip:1.1.1.1", "ip:2.2.2.2", "ip:3.3.3.3"} {
		if allowed, _ := limiter.Allow(k); !allowed {
			t.Fatalf("expected %s to be allowed", k)
		}
	}

	// New unallocated key while table is full of active clients must fail closed
	if allowed, wait := limiter.Allow("ip:4.4.4.4"); allowed || wait < time.Second {
		t.Fatalf("expected new key to fail closed at MaxBuckets; allowed=%v wait=%v", allowed, wait)
	}
	if metrics := limiter.Metrics(); metrics.ActiveBuckets != 3 {
		t.Fatalf("ActiveBuckets = %d, want 3", metrics.ActiveBuckets)
	}

	// Existing active client ip:1.1.1.1 still has 1 token left and must be served fairly
	if allowed, _ := limiter.Allow("ip:1.1.1.1"); !allowed {
		t.Fatal("existing active client should not be evicted or blocked by MaxBuckets")
	}

	// Advance past BucketTTL so stale buckets can be reclaimed opportunistically
	currentTime = currentTime.Add(6 * time.Minute)
	if allowed, _ := limiter.Allow("ip:4.4.4.4"); !allowed {
		t.Fatal("expected new key to be admitted after stale buckets expired")
	}
	if metrics := limiter.Metrics(); metrics.ActiveBuckets != 1 {
		t.Fatalf("ActiveBuckets = %d, want 1 after opportunistic eviction", metrics.ActiveBuckets)
	}
}

func TestConcurrentCleanupAndRequests(t *testing.T) {
	var mu sync.Mutex
	currentTime := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return currentTime
	}

	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 100.0,
		BurstCapacity:  10,
		BucketTTL:      1 * time.Second,
		MaxBuckets:     500,
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				limiter.Allow("ip:192.0.2." + strconv.Itoa((id+i)%20))
				if i%10 == 0 {
					mu.Lock()
					currentTime = currentTime.Add(300 * time.Millisecond)
					now := currentTime
					mu.Unlock()
					limiter.Cleanup(now)
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestClockBackwardAndForwardJumps(t *testing.T) {
	currentTime := time.Date(2026, 11, 26, 11, 0, 10, 0, time.UTC)
	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 2.0,
		BurstCapacity:  2,
		BucketTTL:      10 * time.Second,
		Clock:          func() time.Time { return currentTime },
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	// Consume 1 of 2 tokens (1 token left)
	if allowed, _ := limiter.Allow("client"); !allowed {
		t.Fatal("first request should succeed")
	}

	// Clock moves backward by 1 hour: must NOT subtract tokens or regress lastSeen
	currentTime = currentTime.Add(-1 * time.Hour)
	if allowed, _ := limiter.Allow("client"); !allowed {
		t.Fatal("second request should succeed even when clock moves backward")
	}
	// Now 0 tokens left; immediate request must fail
	if allowed, _ := limiter.Allow("client"); allowed {
		t.Fatal("third request should fail")
	}

	// Exact refill boundary: +500ms at 2.0 req/s = exactly 1.0 token
	currentTime = time.Date(2026, 11, 26, 11, 0, 10, 0, time.UTC).Add(500 * time.Millisecond)
	if allowed, _ := limiter.Allow("client"); !allowed {
		t.Fatal("expected admission at exact 1.0 token refill boundary")
	}

	// Large forward clock jump (100 years): tokens must cap at BurstCapacity=2
	currentTime = currentTime.Add(100 * 365 * 24 * time.Hour)
	for i := 0; i < 2; i++ {
		if allowed, _ := limiter.Allow("client"); !allowed {
			t.Fatalf("request %d after large clock jump should succeed", i)
		}
	}
	if allowed, _ := limiter.Allow("client"); allowed {
		t.Fatal("tokens exceeded BurstCapacity after large forward jump")
	}
}

func TestRetryAfterFractionalRoundingAndOverflowGuard(t *testing.T) {
	currentTime := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	clock := func() time.Time { return currentTime }

	t.Run("FractionalWaitRoundsUp", func(t *testing.T) {
		// 1.0 req/sec -> after consuming 1 token, advance 0.8s -> 0.8 tokens -> deficit 0.2 -> wait 1s
		limiter, _ := NewLimiter(RateLimitConfig{
			Enabled:        true,
			RequestsPerSec: 1.0,
			BurstCapacity:  1,
			Clock:          clock,
		})
		limiter.Allow("c")
		currentTime = currentTime.Add(800 * time.Millisecond)
		allowed, wait := limiter.Allow("c")
		if allowed || wait != 1*time.Second {
			t.Fatalf("allowed=%v wait=%v, want false and 1s", allowed, wait)
		}

		// 0.4 req/sec -> deficit 1.0 -> 2.5s -> rounds up to 3s
		limiterSlow, _ := NewLimiter(RateLimitConfig{
			Enabled:        true,
			RequestsPerSec: 0.4,
			BurstCapacity:  1,
			Clock:          clock,
		})
		limiterSlow.Allow("c")
		allowed, wait = limiterSlow.Allow("c")
		if allowed || wait != 3*time.Second {
			t.Fatalf("allowed=%v wait=%v, want false and 3s", allowed, wait)
		}
	})

	t.Run("TinyRefillRateDoesNotOverflowDuration", func(t *testing.T) {
		limiter, _ := NewLimiter(RateLimitConfig{
			Enabled:        true,
			RequestsPerSec: 1e-12,
			BurstCapacity:  1,
			Clock:          clock,
		})
		limiter.Allow("c")
		allowed, wait := limiter.Allow("c")
		if allowed || wait <= 0 {
			t.Fatalf("wait overflowed or non-positive: allowed=%v wait=%v", allowed, wait)
		}
	})
}

func TestRateLimiterBackgroundCleanupAndStopIdempotency(t *testing.T) {
	limiter, err := NewLimiter(RateLimitConfig{
		Enabled:         true,
		RequestsPerSec:  10.0,
		BurstCapacity:   10,
		BucketTTL:       10 * time.Millisecond,
		CleanupInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	limiter.Allow("ephemeral-client")
	time.Sleep(30 * time.Millisecond)

	if metrics := limiter.Metrics(); metrics.ActiveBuckets != 0 {
		t.Fatalf("ActiveBuckets = %d, want 0 after background cleanup", metrics.ActiveBuckets)
	}

	// Calling Stop multiple times must not panic or deadlock
	limiter.Stop()
	limiter.Stop()
}
