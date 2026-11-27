package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/incident"
)

func TestDatabaseFailureModes(t *testing.T) {
	db, incRepo := openIntegrationRepository(t)
	credRepo := NewCredentialRepository(db)
	verifier := auth.NewStoreVerifier(credRepo)

	t.Run("QueryCancellationAndDeadlineExceeded", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := credRepo.GetByKeyID(canceledCtx, "snt_0000000000000000"); !errors.Is(err, auth.ErrStoreUnavailable) {
			t.Fatalf("canceled GetByKeyID err = %v, want ErrStoreUnavailable", err)
		}
		if _, err := verifier.Verify(canceledCtx, "snt_0000000000000000_validsecretvalue123456"); !errors.Is(err, auth.ErrVerifierUnavailable) {
			t.Fatalf("canceled Verify err = %v, want ErrVerifierUnavailable (must not misclassify as invalid credentials)", err)
		}
		if err := credRepo.Revoke(canceledCtx, "snt_0000000000000000"); !errors.Is(err, auth.ErrStoreUnavailable) {
			t.Fatalf("canceled Revoke err = %v, want ErrStoreUnavailable", err)
		}

		expiredCtx, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
		defer cancelExpired()

		if _, err := incRepo.Get(expiredCtx, "tenant-a", "4b1dd0c8-72aa-4a57-96ac-c5b46a5582ea"); !errors.Is(err, incident.ErrUnavailable) {
			t.Fatalf("expired incident Get err = %v, want incident.ErrUnavailable", err)
		}
	})

	t.Run("ConnectionUnavailableOnClosedDB", func(t *testing.T) {
		dsn := os.Getenv("SENTINEL_TEST_DATABASE_URL")
		closedDB, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("sql.Open failed: %v", err)
		}
		_ = closedDB.Close()

		closedCredRepo := NewCredentialRepository(closedDB)
		closedVerifier := auth.NewStoreVerifier(closedCredRepo)
		if _, err := closedVerifier.Verify(context.Background(), "snt_0000000000000000_validsecretvalue123456"); !errors.Is(err, auth.ErrVerifierUnavailable) {
			t.Fatalf("closed DB Verify err = %v, want ErrVerifierUnavailable", err)
		}
		if err := closedCredRepo.Revoke(context.Background(), "snt_0000000000000000"); !errors.Is(err, auth.ErrStoreUnavailable) {
			t.Fatalf("closed DB Revoke err = %v, want ErrStoreUnavailable", err)
		}
	})

	t.Run("ConnectionPoolExhaustionReturnsUnavailable", func(t *testing.T) {
		dsn := os.Getenv("SENTINEL_TEST_DATABASE_URL")
		poolDB, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("sql.Open failed: %v", err)
		}
		defer poolDB.Close()
		poolDB.SetMaxOpenConns(1)

		tx, err := poolDB.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("BeginTx failed: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		poolCredRepo := NewCredentialRepository(poolDB)
		poolVerifier := auth.NewStoreVerifier(poolCredRepo)
		shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		if _, err := poolVerifier.Verify(shortCtx, "snt_0000000000000000_validsecretvalue123456"); !errors.Is(err, auth.ErrVerifierUnavailable) {
			t.Fatalf("pool exhaustion Verify err = %v, want ErrVerifierUnavailable", err)
		}
	})

	t.Run("TransactionRollbackLeavesNoPersistedRows", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx failed: %v", err)
		}
		tenantID := fmt.Sprintf("tenant-rollback-%d", time.Now().UnixNano())
		_, err = tx.ExecContext(ctx, `
INSERT INTO incidents (tenant_id, source, source_event_id, service, title, description, severity, occurred_at)
VALUES ($1, 'test', 'evt-rb', 'svc', 'title', 'desc', 'low', now())`, tenantID)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("tx insert failed: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("tx rollback failed: %v", err)
		}

		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM incidents WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
		if count != 0 {
			t.Fatalf("rolled back row persisted: count = %d, want 0", count)
		}
	})
}
