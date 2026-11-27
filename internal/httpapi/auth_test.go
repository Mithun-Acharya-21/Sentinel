package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/incident"
)

type stubCredentialStore struct {
	creds map[string]auth.StoredCredential
}

func (s *stubCredentialStore) GetByKeyID(_ context.Context, keyID string) (auth.StoredCredential, error) {
	c, ok := s.creds[keyID]
	if !ok {
		return auth.StoredCredential{}, auth.ErrCredentialNotFound
	}
	return c, nil
}

func (s *stubCredentialStore) Create(_ context.Context, input auth.CreateCredentialInput) (auth.StoredCredential, error) {
	stored := auth.StoredCredential{
		ID:          "cred-" + input.KeyID,
		KeyID:       input.KeyID,
		SecretHash:  input.SecretHash,
		TenantID:    input.TenantID,
		PrincipalID: input.PrincipalID,
		Scopes:      input.Scopes,
		ExpiresAt:   input.ExpiresAt,
		CreatedAt:   time.Now().UTC(),
	}
	s.creds[input.KeyID] = stored
	return stored, nil
}

func (s *stubCredentialStore) Revoke(_ context.Context, keyID string) error {
	c, ok := s.creds[keyID]
	if !ok {
		return auth.ErrCredentialNotFound
	}
	now := time.Now().UTC()
	c.RevokedAt = &now
	s.creds[keyID] = c
	return nil
}

func setupAuthTestHarness(t *testing.T, burst int) (http.Handler, *fakeRepository, *stubCredentialStore, *Limiter) {
	t.Helper()
	store := &stubCredentialStore{creds: make(map[string]auth.StoredCredential)}
	verifier := auth.NewStoreVerifier(store)
	repo := &fakeRepository{
		created: sampleIncident(),
		got:     sampleIncident(),
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	preAuthLimiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  burst,
		Clock:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewLimiter (preAuth) failed: %v", err)
	}
	principalLimiter, err := NewLimiter(RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 1.0,
		BurstCapacity:  burst,
		Clock:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewLimiter (principal) failed: %v", err)
	}

	root := http.NewServeMux()
	root.Handle("GET /health/live", LivenessHandler())
	root.Handle("GET /health/ready", ReadinessHandler(fakePinger{err: nil}))
	root.Handle("/", NewProtectedHandlerWithLimiters(repo, verifier, preAuthLimiter, principalLimiter))
	return root, repo, store, principalLimiter
}

func provisionTestKey(t *testing.T, store *stubCredentialStore, tenantID, principalID string, scopes []string) auth.ProvisionedCredential {
	t.Helper()
	prov, input, err := auth.GenerateAPIKey(tenantID, principalID, scopes)
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}
	if _, err := store.Create(context.Background(), input); err != nil {
		t.Fatalf("store.Create failed: %v", err)
	}
	return prov
}

func TestAuthenticationAndAuthorizationBoundaries(t *testing.T) {
	handler, repo, store, _ := setupAuthTestHarness(t, 20)

	validKey := provisionTestKey(t, store, "tenant-demo", "svc-demo", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsWrite})
	readOnlyKey := provisionTestKey(t, store, "tenant-demo", "svc-reader", []string{auth.ScopeIncidentsRead})
	writeOnlyKey := provisionTestKey(t, store, "tenant-demo", "svc-writer", []string{auth.ScopeIncidentsWrite})
	revokedKey := provisionTestKey(t, store, "tenant-demo", "svc-revoked", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsWrite})
	if err := store.Revoke(context.Background(), revokedKey.KeyID); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}

	t.Run("MissingCredentialsReturns401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusUnauthorized, "unauthorized")
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing WWW-Authenticate header")
		}
	})

	t.Run("InvalidCredentialsReturns401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		req.Header.Set("Authorization", "Bearer snt_0000000000000000_invalidsecret123456")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusUnauthorized, "invalid_credentials")
	})

	t.Run("ArbitraryAuthorizationHeaderReturns401", func(t *testing.T) {
		for _, header := range []string{"Bearer arbitrary-jwt-or-string", "Basic dXNlcjpwYXNz", "Token foo"} {
			req := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(sampleJSON()))
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assertError(t, rec, http.StatusUnauthorized, "invalid_credentials")
		}
	})

	t.Run("ArbitraryXAPIKeyHeaderIgnoredReturns401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		req.Header.Set("X-API-Key", validKey.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusUnauthorized, "unauthorized")
	})

	t.Run("RevokedCredentialsReturns401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		req.Header.Set("Authorization", "Bearer "+revokedKey.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusUnauthorized, "invalid_credentials")
	})

	t.Run("ExpiredCredentialsReturns401", func(t *testing.T) {
		past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		expProv, expIn, err := auth.GenerateAPIKeyWithExpiry("tenant-demo", "svc-expired", []string{auth.ScopeIncidentsRead}, &past)
		if err != nil {
			t.Fatalf("GenerateAPIKeyWithExpiry failed: %v", err)
		}
		if _, err := store.Create(context.Background(), expIn); err != nil {
			t.Fatalf("store.Create failed: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		req.Header.Set("Authorization", "Bearer "+expProv.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusUnauthorized, "invalid_credentials")
	})

	t.Run("ValidCredentialsSucceedsAndPropagatesPrincipal", func(t *testing.T) {
		// Omit tenant_id from JSON body to prove server derives it from Principal.
		bodyWithoutTenant := `{"source":"synthetic","source_event_id":"evt-1","service":"payment-api","title":"Outage","description":"Details","severity":"high","occurred_at":"2026-10-12T10:15:00Z"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(bodyWithoutTenant))
		req.Header.Set("Authorization", "Bearer "+validKey.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body)
		}
		if repo.lastInput.TenantID != "tenant-demo" {
			t.Fatalf("repo.lastInput.TenantID = %q, want tenant-demo", repo.lastInput.TenantID)
		}
		p, ok := auth.PrincipalFromContext(repo.lastContext)
		if !ok || p.PrincipalID != "svc-demo" || p.TenantID != "tenant-demo" {
			t.Fatalf("principal not propagated to repository context: %#v (ok=%v)", p, ok)
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Fatal("X-Request-ID header was not set")
		}
	})

	t.Run("ScopeEnforcementReturns403", func(t *testing.T) {
		// Read-only key cannot POST
		reqPost := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(sampleJSON()))
		reqPost.Header.Set("Authorization", "Bearer "+readOnlyKey.PlaintextToken)
		recPost := httptest.NewRecorder()
		handler.ServeHTTP(recPost, reqPost)
		assertError(t, recPost, http.StatusForbidden, "forbidden")

		// Write-only key cannot GET
		reqGet := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		reqGet.Header.Set("Authorization", "Bearer "+writeOnlyKey.PlaintextToken)
		recGet := httptest.NewRecorder()
		handler.ServeHTTP(recGet, reqGet)
		assertError(t, recGet, http.StatusForbidden, "forbidden")
	})

	t.Run("HealthEndpointsExemptFromAuthAndRateLimiting", func(t *testing.T) {
		for _, path := range []string{"/health/live", "/health/ready"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("health path %s status = %d, want 200", path, rec.Code)
			}
		}
	})
}

func TestTenantIsolationAndSubstitutionAttacks(t *testing.T) {
	handler, repo, store, _ := setupAuthTestHarness(t, 20)

	tenantAKey := provisionTestKey(t, store, "tenant-a", "svc-a", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsWrite})
	tenantBKey := provisionTestKey(t, store, "tenant-b", "svc-b", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsWrite})

	t.Run("RequestBodyTenantSubstitutionRejected", func(t *testing.T) {
		// Authenticated as tenant-a, attempting to create incident for tenant-b
		spoofedBody := strings.Replace(sampleJSON(), `"tenant-demo"`, `"tenant-b"`, 1)
		req := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(spoofedBody))
		req.Header.Set("Authorization", "Bearer "+tenantAKey.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusForbidden, "forbidden")
	})

	t.Run("QueryParameterTenantSubstitutionRejected", func(t *testing.T) {
		reqGet := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID+"?tenant_id=tenant-b", nil)
		reqGet.Header.Set("Authorization", "Bearer "+tenantAKey.PlaintextToken)
		recGet := httptest.NewRecorder()
		handler.ServeHTTP(recGet, reqGet)
		assertError(t, recGet, http.StatusForbidden, "forbidden")

		reqPost := httptest.NewRequest(http.MethodPost, "/v1/incidents?tenant_id=tenant-b", strings.NewReader(`{"source":"s","source_event_id":"e","service":"svc","title":"t","description":"d","severity":"low","occurred_at":"2026-10-12T10:15:00Z"}`))
		reqPost.Header.Set("Authorization", "Bearer "+tenantAKey.PlaintextToken)
		recPost := httptest.NewRecorder()
		handler.ServeHTTP(recPost, reqPost)
		assertError(t, recPost, http.StatusForbidden, "forbidden")
	})

	t.Run("CrossTenantIncidentAccessScopedToAuthenticatedTenant", func(t *testing.T) {
		// Simulate repository returning ErrNotFound when tenant-b queries tenant-a's incident ID.
		repo.getErr = incident.ErrNotFound
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		req.Header.Set("Authorization", "Bearer "+tenantBKey.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assertError(t, rec, http.StatusNotFound, "not_found")
		if repo.lastTenantID != "tenant-b" {
			t.Fatalf("repository Get called with tenantID = %q, want tenant-b", repo.lastTenantID)
		}
	})
}

func TestRateLimiterUsesSingleVerifiedPrincipalBucketAndThrottlesUnauthenticatedBruteForce(t *testing.T) {
	handler, _, store, limiter := setupAuthTestHarness(t, 2)

	keyA := provisionTestKey(t, store, "tenant-a", "principal-alpha", []string{auth.ScopeIncidentsRead})
	keyB := provisionTestKey(t, store, "tenant-b", "principal-beta", []string{auth.ScopeIncidentsRead})

	// 1. Authenticated principal-alpha from two different IPs shares ONE principal bucket (burst=2)
	req1 := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	req1.RemoteAddr = "198.51.100.10:1111"
	req1.Header.Set("Authorization", "Bearer "+keyA.PlaintextToken)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	req2.RemoteAddr = "198.51.100.11:2222"
	req2.Header.Set("Authorization", "Bearer "+keyA.PlaintextToken)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200", rec2.Code)
	}

	// 3rd request for principal-alpha is rate-limited (429)
	req3 := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	req3.RemoteAddr = "198.51.100.12:3333"
	req3.Header.Set("Authorization", "Bearer "+keyA.PlaintextToken)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	assertError(t, rec3, http.StatusTooManyRequests, "rate_limit_exceeded")

	// Exactly ONE bucket was created for principal-alpha (no IP + principal duplication)
	if m := limiter.Metrics(); m.ActiveBuckets != 1 {
		t.Fatalf("ActiveBuckets = %d, want 1", m.ActiveBuckets)
	}

	// 2. Different authenticated principal-beta behind same NAT IP has its own bucket
	reqB := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqB.RemoteAddr = "198.51.100.10:4444"
	reqB.Header.Set("Authorization", "Bearer "+keyB.PlaintextToken)
	recB := httptest.NewRecorder()
	handler.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("principal-beta status = %d, want 200", recB.Code)
	}

	// 3. Unauthenticated brute-force from one IP gets 401 for burst=2, then 429
	for i := 0; i < 2; i++ {
		reqUnauth := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
		reqUnauth.RemoteAddr = "203.0.113.55:9999"
		reqUnauth.Header.Set("Authorization", "Bearer snt_0000000000000000_bruteforce123456")
		recUnauth := httptest.NewRecorder()
		handler.ServeHTTP(recUnauth, reqUnauth)
		assertError(t, recUnauth, http.StatusUnauthorized, "invalid_credentials")
	}

	reqThrottled := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID, nil)
	reqThrottled.RemoteAddr = "203.0.113.55:9999"
	reqThrottled.Header.Set("Authorization", "Bearer snt_0000000000000000_bruteforce123456")
	recThrottled := httptest.NewRecorder()
	handler.ServeHTTP(recThrottled, reqThrottled)
	assertError(t, recThrottled, http.StatusTooManyRequests, "rate_limit_exceeded")
}
