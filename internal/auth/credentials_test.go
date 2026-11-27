package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type memCredentialStore struct {
	creds map[string]StoredCredential
	err   error
}

func (m *memCredentialStore) GetByKeyID(_ context.Context, keyID string) (StoredCredential, error) {
	if m.err != nil {
		return StoredCredential{}, m.err
	}
	c, ok := m.creds[keyID]
	if !ok {
		return StoredCredential{}, ErrCredentialNotFound
	}
	return c, nil
}

func (m *memCredentialStore) Create(_ context.Context, input CreateCredentialInput) (StoredCredential, error) {
	if m.err != nil {
		return StoredCredential{}, m.err
	}
	stored := StoredCredential{
		ID:          "cred-uuid-1",
		KeyID:       input.KeyID,
		SecretHash:  input.SecretHash,
		TenantID:    input.TenantID,
		PrincipalID: input.PrincipalID,
		Scopes:      input.Scopes,
		ExpiresAt:   input.ExpiresAt,
		CreatedAt:   time.Now().UTC(),
	}
	m.creds[input.KeyID] = stored
	return stored, nil
}

func (m *memCredentialStore) Revoke(_ context.Context, keyID string) error {
	c, ok := m.creds[keyID]
	if !ok {
		return ErrCredentialNotFound
	}
	now := time.Now().UTC()
	c.RevokedAt = &now
	m.creds[keyID] = c
	return nil
}

func TestGenerateAndVerifyAPIKey(t *testing.T) {
	prov, input, err := GenerateAPIKey("tenant-alpha", "svc-ingest", []string{ScopeIncidentsWrite, ScopeIncidentsRead})
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}
	if !strings.HasPrefix(prov.PlaintextToken, TokenPrefix) {
		t.Fatalf("token missing prefix: %q", prov.PlaintextToken)
	}
	if input.SecretHash == "" || strings.Contains(prov.PlaintextToken, input.SecretHash) {
		t.Fatal("secret hash must not match plaintext token")
	}

	store := &memCredentialStore{creds: make(map[string]StoredCredential)}
	if _, err := store.Create(context.Background(), input); err != nil {
		t.Fatalf("store.Create failed: %v", err)
	}

	verifier := NewStoreVerifier(store)

	// 1. Valid token
	p, err := verifier.Verify(context.Background(), prov.PlaintextToken)
	if err != nil {
		t.Fatalf("Verify valid token failed: %v", err)
	}
	if p.TenantID != "tenant-alpha" || p.PrincipalID != "svc-ingest" || p.AuthMethod != AuthMethodBearerAPIKey {
		t.Fatalf("unexpected principal: %#v", p)
	}
	if !p.HasScope(ScopeIncidentsWrite) || !p.HasScope(ScopeIncidentsRead) || p.HasScope("admin") {
		t.Fatalf("unexpected scopes: %v", p.Scopes)
	}

	// 2. Wrong secret with valid keyID
	keyID, _, _ := ParseToken(prov.PlaintextToken)
	if _, err := verifier.Verify(context.Background(), keyID+"_wrongsecretvalue123456"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong secret err = %v, want ErrInvalidCredentials", err)
	}

	// 3. Malformed token rejected without touching store
	store.err = errors.New("should not be called")
	if _, err := verifier.Verify(context.Background(), "not-a-sentinel-token"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("malformed token err = %v, want ErrInvalidCredentials", err)
	}
	store.err = nil

	// 4. Revoked token
	if err := store.Revoke(context.Background(), prov.KeyID); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), prov.PlaintextToken); !errors.Is(err, ErrRevokedCredentials) {
		t.Fatalf("revoked token err = %v, want ErrRevokedCredentials", err)
	}
}

func TestCredentialExpirationAndMetrics(t *testing.T) {
	currentTime := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return currentTime }

	store := &memCredentialStore{creds: make(map[string]StoredCredential)}
	verifier := NewStoreVerifierWithClock(store, clock)

	exp := currentTime.Add(1 * time.Hour)
	prov, input, err := GenerateAPIKeyWithExpiry("tenant-exp", "svc-exp", []string{ScopeIncidentsRead}, &exp)
	if err != nil {
		t.Fatalf("GenerateAPIKeyWithExpiry failed: %v", err)
	}
	if input.ExpiresAt == nil || !input.ExpiresAt.Equal(exp) {
		t.Fatalf("ExpiresAt = %v, want %v", input.ExpiresAt, exp)
	}
	if _, err := store.Create(context.Background(), input); err != nil {
		t.Fatalf("store.Create failed: %v", err)
	}

	// 1. Before expiration: succeeds
	if _, err := verifier.Verify(context.Background(), prov.PlaintextToken); err != nil {
		t.Fatalf("Verify before expiration failed: %v", err)
	}

	// 2. Exact expiration boundary (now == ExpiresAt): rejected as expired
	currentTime = exp
	if _, err := verifier.Verify(context.Background(), prov.PlaintextToken); !errors.Is(err, ErrExpiredCredentials) {
		t.Fatalf("Verify at exact expiry err = %v, want ErrExpiredCredentials", err)
	}

	// 3. Wrong secret on expired key returns ErrInvalidCredentials (hash checked first)
	keyID, _, _ := ParseToken(prov.PlaintextToken)
	if _, err := verifier.Verify(context.Background(), keyID+"_wrongsecretvalue123456"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong secret on expired key err = %v, want ErrInvalidCredentials", err)
	}

	// 4. Revoked + expired key returns ErrRevokedCredentials
	_ = store.Revoke(context.Background(), prov.KeyID)
	if _, err := verifier.Verify(context.Background(), prov.PlaintextToken); !errors.Is(err, ErrRevokedCredentials) {
		t.Fatalf("revoked+expired key err = %v, want ErrRevokedCredentials", err)
	}

	// 5. Store failure increments DBLookupErrors and returns ErrVerifierUnavailable
	store.err = errors.New("connection reset")
	if _, err := verifier.Verify(context.Background(), prov.PlaintextToken); !errors.Is(err, ErrVerifierUnavailable) {
		t.Fatalf("store error err = %v, want ErrVerifierUnavailable", err)
	}

	snap := verifier.Metrics()
	if snap.AuthSuccesses != 1 {
		t.Fatalf("AuthSuccesses = %d, want 1", snap.AuthSuccesses)
	}
	if snap.ExpiredRejections != 1 {
		t.Fatalf("ExpiredRejections = %d, want 1", snap.ExpiredRejections)
	}
	if snap.RevokedRejections != 1 {
		t.Fatalf("RevokedRejections = %d, want 1", snap.RevokedRejections)
	}
	if snap.DBLookupErrors != 1 {
		t.Fatalf("DBLookupErrors = %d, want 1", snap.DBLookupErrors)
	}
	if snap.AuthRejections != 3 {
		t.Fatalf("AuthRejections = %d, want 3 (1 expired + 1 wrong secret + 1 revoked)", snap.AuthRejections)
	}
}
