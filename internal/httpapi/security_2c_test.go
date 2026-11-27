package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sentinel/internal/auth"
)

type countingVerifier struct {
	inner auth.CredentialVerifier
	calls atomic.Int64
	err   error
}

func (c *countingVerifier) Verify(ctx context.Context, token string) (auth.Principal, error) {
	c.calls.Add(1)
	if c.err != nil {
		return auth.Principal{}, c.err
	}
	return c.inner.Verify(ctx, token)
}

type syncCredentialStore struct {
	mu    sync.RWMutex
	creds map[string]auth.StoredCredential
}

func newSyncCredentialStore() *syncCredentialStore {
	return &syncCredentialStore{creds: make(map[string]auth.StoredCredential)}
}

func (s *syncCredentialStore) GetByKeyID(_ context.Context, keyID string) (auth.StoredCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.creds[keyID]
	if !ok {
		return auth.StoredCredential{}, auth.ErrCredentialNotFound
	}
	return c, nil
}

func (s *syncCredentialStore) Create(_ context.Context, input auth.CreateCredentialInput) (auth.StoredCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := auth.StoredCredential{
		ID:          "cred-" + input.KeyID,
		KeyID:       input.KeyID,
		SecretHash:  input.SecretHash,
		TenantID:    input.TenantID,
		PrincipalID: input.PrincipalID,
		Scopes:      input.Scopes,
		CreatedAt:   time.Now().UTC(),
	}
	s.creds[input.KeyID] = stored
	return stored, nil
}

func (s *syncCredentialStore) Revoke(_ context.Context, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[keyID]
	if !ok {
		return auth.ErrCredentialNotFound
	}
	now := time.Now().UTC()
	c.RevokedAt = &now
	s.creds[keyID] = c
	return nil
}

func TestPreAuthFloodProtectionStopsCredentialVerification(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	preAuthLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  3,
		Clock:          clock,
		TrustedProxies: []string{"10.0.0.1"},
	})
	principalLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 10.0,
		BurstCapacity:  20,
		Clock:          clock,
	})

	store := newSyncCredentialStore()
	verifier := &countingVerifier{inner: auth.NewStoreVerifier(store)}
	repo := &fakeRepository{created: sampleIncident(), got: sampleIncident()}
	handler := NewProtectedHandlerWithLimiters(repo, verifier, preAuthLimiter, principalLimiter)

	t.Run("SyntacticallyValidNonexistentKeysThrottledBeforeVerifier", func(t *testing.T) {
		verifier.calls.Store(0)
		// First 3 requests within pre-auth burst=3 reach AuthenticateMiddleware -> verifier called 3 times -> 401
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
			req.RemoteAddr = "198.51.100.10:1234"
			req.Header.Set("Authorization", "Bearer snt_000000000000000"+strconv.Itoa(i)+"_nonexistentsecret123456")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assertError(t, rec, http.StatusUnauthorized, "invalid_credentials")
		}
		if got := verifier.calls.Load(); got != 3 {
			t.Fatalf("verifier.calls = %d, want 3", got)
		}

		// Next 10 flood requests (rotating keys + spoofed XFF) MUST be dropped at Stage 1 (429)
		// WITHOUT invoking verifier at all!
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
			req.RemoteAddr = "198.51.100.10:1234"
			req.Header.Set("Authorization", "Bearer snt_aaaaaaaaaaaaaaaa_rotatedsecret"+strconv.Itoa(i)+"123456")
			req.Header.Set("X-API-Key", "rotated-api-key-"+strconv.Itoa(i))
			req.Header.Set("X-Forwarded-For", "203.0.113."+strconv.Itoa(i+1))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assertError(t, rec, http.StatusTooManyRequests, "rate_limit_exceeded")
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("missing Retry-After header on Stage 1 rejection")
			}
		}
		if got := verifier.calls.Load(); got != 3 {
			t.Fatalf("verifier.calls = %d after flood, want 3 (pre-auth limiter failed to stop DB lookups)", got)
		}
	})

	t.Run("MalformedKeysAndMissingAuthThrottledByIP", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
			req.RemoteAddr = "198.51.100.20:5555"
			if i%2 == 0 {
				req.Header.Set("Authorization", "Bearer malformed-token")
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("req %d status = %d, want 401", i, rec.Code)
			}
		}

		reqFlood := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		reqFlood.RemoteAddr = "198.51.100.20:5555"
		recFlood := httptest.NewRecorder()
		handler.ServeHTTP(recFlood, reqFlood)
		assertError(t, recFlood, http.StatusTooManyRequests, "rate_limit_exceeded")
	})

	t.Run("TrustedProxyResolvesClientIPForStage1", func(t *testing.T) {
		verifier.calls.Store(0)
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
			req.RemoteAddr = "10.0.0.1:443" // Trusted proxy
			req.Header.Set("X-Forwarded-For", "203.0.113.77, 10.0.0.1")
			req.Header.Set("Authorization", "Bearer snt_0000000000000000_nonexistentsecret123456")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assertError(t, rec, http.StatusUnauthorized, "invalid_credentials")
		}

		// 4th request from same forwarded client via trusted proxy is throttled before verifier
		req4 := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		req4.RemoteAddr = "10.0.0.1:443"
		req4.Header.Set("X-Forwarded-For", "203.0.113.77, 10.0.0.1")
		req4.Header.Set("Authorization", "Bearer snt_0000000000000000_nonexistentsecret123456")
		rec4 := httptest.NewRecorder()
		handler.ServeHTTP(rec4, req4)
		assertError(t, rec4, http.StatusTooManyRequests, "rate_limit_exceeded")
		if got := verifier.calls.Load(); got != 3 {
			t.Fatalf("verifier.calls = %d, want 3", got)
		}
	})
}

func TestAuthenticatedPrincipalRateLimitingTwoStage(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// High Stage 1 IP burst (100), tight Stage 2 principal burst (2)
	preAuthLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 100.0,
		BurstCapacity:  100,
		Clock:          clock,
	})
	principalLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  2,
		Clock:          clock,
	})

	store := newSyncCredentialStore()
	provA, inA, _ := auth.GenerateAPIKey("tenant-a", "svc-shared", []string{auth.ScopeIncidentsRead})
	provB, inB, _ := auth.GenerateAPIKey("tenant-b", "svc-shared", []string{auth.ScopeIncidentsRead})
	_, _ = store.Create(context.Background(), inA)
	_, _ = store.Create(context.Background(), inB)

	verifier := auth.NewStoreVerifier(store)
	repo := &fakeRepository{got: sampleIncident()}
	handler := NewProtectedHandlerWithLimiters(repo, verifier, preAuthLimiter, principalLimiter)

	// Principal A consumes 2 tokens across different IPs and rotated unverified headers
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		req.RemoteAddr = "198.51.100." + strconv.Itoa(10+i) + ":1000"
		req.Header.Set("Authorization", "Bearer "+provA.PlaintextToken)
		req.Header.Set("X-API-Key", "ignored-"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("principal A req %d status = %d, want 200", i, rec.Code)
		}
	}

	// 3rd request for Principal A is blocked by Stage 2 even from a brand-new IP
	reqA3 := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqA3.RemoteAddr = "198.51.100.99:9999"
	reqA3.Header.Set("Authorization", "Bearer "+provA.PlaintextToken)
	recA3 := httptest.NewRecorder()
	handler.ServeHTTP(recA3, reqA3)
	assertError(t, recA3, http.StatusTooManyRequests, "rate_limit_exceeded")

	// Principal B (same PrincipalID "svc-shared", different tenant "tenant-b") has an independent quota!
	reqB1 := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqB1.RemoteAddr = "198.51.100.10:1000"
	reqB1.Header.Set("Authorization", "Bearer "+provB.PlaintextToken)
	recB1 := httptest.NewRecorder()
	handler.ServeHTTP(recB1, reqB1)
	if recB1.Code != http.StatusOK {
		t.Fatalf("principal B status = %d, want 200 (independent tenant-scoped principal bucket)", recB1.Code)
	}

	if got := principalLimiter.Metrics().ActiveBuckets; got != 2 {
		t.Fatalf("principalLimiter ActiveBuckets = %d, want 2", got)
	}
}

func TestVerifierOutageReturns503AndFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	preAuthLimiter, _ := NewLimiter(RateLimitConfig{Enabled: true, RequestsPerSec: 10, BurstCapacity: 10, Clock: func() time.Time { return now }})
	principalLimiter, _ := NewLimiter(RateLimitConfig{Enabled: true, RequestsPerSec: 10, BurstCapacity: 10, Clock: func() time.Time { return now }})

	verifier := &countingVerifier{err: errors.Join(auth.ErrVerifierUnavailable, errors.New("db connection refused"))}
	repo := &fakeRepository{got: sampleIncident()}
	handler := NewProtectedHandlerWithLimiters(repo, verifier, preAuthLimiter, principalLimiter)

	req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	req.RemoteAddr = "198.51.100.50:1234"
	req.Header.Set("Authorization", "Bearer snt_0000000000000000_validformat12345678")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusServiceUnavailable, "auth_unavailable")
	if repo.GetCalls() != 0 {
		t.Fatalf("repo.GetCalls() = %d, want 0 (handler must not run on verifier outage)", repo.GetCalls())
	}
}

func TestConcurrencyTwoStageLimitersAndCredentialRevocation(t *testing.T) {
	var mu sync.Mutex
	currentTime := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return currentTime
	}

	preAuthLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 50.0,
		BurstCapacity:  100,
		BucketTTL:      1 * time.Second,
		MaxBuckets:     1000,
		Clock:          clock,
	})
	principalLimiter, _ := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 50.0,
		BurstCapacity:  100,
		BucketTTL:      1 * time.Second,
		MaxBuckets:     1000,
		Clock:          clock,
	})

	store := newSyncCredentialStore()
	prov, in, _ := auth.GenerateAPIKey("tenant-conc", "svc-conc", []string{auth.ScopeIncidentsRead})
	_, _ = store.Create(context.Background(), in)
	verifier := auth.NewStoreVerifier(store)
	repo := &fakeRepository{got: sampleIncident()}
	handler := NewProtectedHandlerWithLimiters(repo, verifier, preAuthLimiter, principalLimiter)

	const workers = 12
	const iters = 30
	var wg sync.WaitGroup
	wg.Add(workers + 1)

	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
				req.RemoteAddr = "198.51.100." + strconv.Itoa(id%3+1) + ":5000"
				req.Header.Set("Authorization", "Bearer "+prov.PlaintextToken)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusTooManyRequests {
					t.Errorf("unexpected status code %d", rec.Code)
				}
				if i%10 == 0 {
					mu.Lock()
					currentTime = currentTime.Add(500 * time.Millisecond)
					now := currentTime
					mu.Unlock()
					preAuthLimiter.Cleanup(now)
					principalLimiter.Cleanup(now)
				}
			}
		}(w)
	}

	// Concurrent revocation mid-flight
	go func() {
		defer wg.Done()
		_ = store.Revoke(context.Background(), prov.KeyID)
	}()

	wg.Wait()

	// After revocation completes, verification must fail with ErrRevokedCredentials
	if _, err := verifier.Verify(context.Background(), prov.PlaintextToken); !errors.Is(err, auth.ErrRevokedCredentials) {
		t.Fatalf("post-revocation Verify err = %v, want ErrRevokedCredentials", err)
	}
}
