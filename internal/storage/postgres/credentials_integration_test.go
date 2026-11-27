package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"sentinel/internal/auth"
)

func TestCredentialRepositoryCreateVerifyExpireAndRevoke(t *testing.T) {
	db, _ := openIntegrationRepository(t)
	credRepo := NewCredentialRepository(db)

	now := time.Now().UTC()
	clock := func() time.Time { return now }
	verifier := auth.NewStoreVerifierWithClock(credRepo, clock)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tenantID := fmt.Sprintf("tenant-cred-%d", time.Now().UnixNano())
	prov, input, err := auth.GenerateAPIKey(tenantID, "svc-integration", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsWrite})
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM api_credentials WHERE tenant_id = $1`, tenantID)
	})

	created, err := credRepo.Create(ctx, input)
	if err != nil {
		t.Fatalf("Create credential (apply migrations 002 & 003 first): %v", err)
	}
	if created.ID == "" || created.KeyID != prov.KeyID || created.SecretHash != input.SecretHash || created.ExpiresAt != nil {
		t.Fatalf("unexpected created credential: %#v", created)
	}

	// 1. Valid verification
	principal, err := verifier.Verify(ctx, prov.PlaintextToken)
	if err != nil {
		t.Fatalf("Verify valid token failed: %v", err)
	}
	if principal.TenantID != tenantID || principal.PrincipalID != "svc-integration" || principal.CredentialID != created.ID {
		t.Fatalf("unexpected principal: %#v", principal)
	}

	// 2. Invalid secret rejection
	if _, err := verifier.Verify(ctx, prov.KeyID+"_wrongsecretvalue123456"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("invalid secret err = %v, want ErrInvalidCredentials", err)
	}

	// 3. Duplicate key_id handling
	if _, err := credRepo.Create(ctx, input); !errors.Is(err, auth.ErrDuplicateKeyID) {
		t.Fatalf("duplicate key_id err = %v, want ErrDuplicateKeyID", err)
	}

	// 4. Revocation and idempotent repeated revocation
	if err := credRepo.Revoke(ctx, prov.KeyID); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	if err := credRepo.Revoke(ctx, prov.KeyID); err != nil {
		t.Fatalf("Repeated Revoke failed (expected idempotent nil): %v", err)
	}
	if err := credRepo.Revoke(ctx, "snt_nonexistent0000"); !errors.Is(err, auth.ErrCredentialNotFound) {
		t.Fatalf("Revoke nonexistent err = %v, want ErrCredentialNotFound", err)
	}

	// 5. Verify revoked token fails immediately
	if _, err := verifier.Verify(ctx, prov.PlaintextToken); !errors.Is(err, auth.ErrRevokedCredentials) {
		t.Fatalf("Verify after revoke err = %v, want ErrRevokedCredentials", err)
	}

	// 6. Expiration lifecycle
	exp := now.Add(30 * time.Minute)
	expProv, expInput, err := auth.GenerateAPIKeyWithExpiry(tenantID, "svc-expiring", []string{auth.ScopeIncidentsRead}, &exp)
	if err != nil {
		t.Fatalf("GenerateAPIKeyWithExpiry failed: %v", err)
	}
	expCreated, err := credRepo.Create(ctx, expInput)
	if err != nil {
		t.Fatalf("Create expiring credential failed: %v", err)
	}
	if expCreated.ExpiresAt == nil {
		t.Fatal("expected non-nil ExpiresAt on stored credential")
	}

	if _, err := verifier.Verify(ctx, expProv.PlaintextToken); err != nil {
		t.Fatalf("Verify before expiration failed: %v", err)
	}

	now = exp.Add(1 * time.Second)
	if _, err := verifier.Verify(ctx, expProv.PlaintextToken); !errors.Is(err, auth.ErrExpiredCredentials) {
		t.Fatalf("Verify after expiration err = %v, want ErrExpiredCredentials", err)
	}
}

func TestCredentialRepositoryConcurrentVerifyAndRevoke(t *testing.T) {
	db, _ := openIntegrationRepository(t)
	credRepo := NewCredentialRepository(db)
	verifier := auth.NewStoreVerifier(credRepo)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tenantID := fmt.Sprintf("tenant-conc-%d", time.Now().UnixNano())
	prov, input, err := auth.GenerateAPIKey(tenantID, "svc-conc", []string{auth.ScopeIncidentsRead})
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM api_credentials WHERE tenant_id = $1`, tenantID)
	})
	if _, err := credRepo.Create(ctx, input); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	const workers = 10
	const iters = 15
	var wg sync.WaitGroup
	wg.Add(workers + 1)

	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, err := verifier.Verify(ctx, prov.PlaintextToken)
				if err != nil && !errors.Is(err, auth.ErrRevokedCredentials) {
					t.Errorf("unexpected Verify error: %v", err)
				}
			}
		}()
	}

	go func() {
		defer wg.Done()
		if err := credRepo.Revoke(ctx, prov.KeyID); err != nil {
			t.Errorf("concurrent Revoke failed: %v", err)
		}
	}()

	wg.Wait()

	if _, err := verifier.Verify(ctx, prov.PlaintextToken); !errors.Is(err, auth.ErrRevokedCredentials) {
		t.Fatalf("post-revocation Verify err = %v, want ErrRevokedCredentials", err)
	}
}
