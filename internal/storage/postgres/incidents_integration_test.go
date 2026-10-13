package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"sentinel/internal/incident"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openIntegrationRepository(t *testing.T) (*sql.DB, *Repository) {
	t.Helper()
	dsn := os.Getenv("SENTINEL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SENTINEL_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL (apply migrations/001_create_incidents.up.sql first): %v", err)
	}
	return db, NewRepository(db)
}

func TestRepositoryCreateReplayConflictAndGet(t *testing.T) {
	db, repository := openIntegrationRepository(t)
	input := uniqueInput()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	created, replay, err := repository.Create(ctx, input)
	if err != nil {
		t.Fatalf("Create new incident: %v", err)
	}
	if replay {
		t.Fatal("first create was marked as a replay")
	}
	if created.ID == "" || created.CreatedAt.IsZero() {
		t.Fatalf("database-generated fields missing: %#v", created)
	}

	duplicate, replay, err := repository.Create(ctx, input)
	if err != nil {
		t.Fatalf("Create identical replay: %v", err)
	}
	if !replay || duplicate.ID != created.ID {
		t.Fatalf("duplicate = %#v, replay=%v; want original id %q and replay=true", duplicate, replay, created.ID)
	}

	changed := input
	changed.Title += " (changed)"
	if _, _, err := repository.Create(ctx, changed); err != incident.ErrIdempotencyConflict {
		t.Fatalf("conflicting payload error = %v, want ErrIdempotencyConflict", err)
	}

	got, err := repository.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get incident: %v", err)
	}
	if got.ID != created.ID || got.Title != input.Title {
		t.Fatalf("Get returned %#v; want original incident", got)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM incidents WHERE tenant_id = $1 AND source = $2 AND source_event_id = $3`, input.TenantID, input.Source, input.SourceEventID).Scan(&count); err != nil {
		t.Fatalf("count idempotency rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for idempotency key = %d, want 1", count)
	}
}

func TestRepositoryConcurrentIdenticalCreates(t *testing.T) {
	db, repository := openIntegrationRepository(t)
	input := uniqueInput()
	const writers = 12
	type result struct {
		incident incident.Incident
		replay   bool
		err      error
	}
	results := make(chan result, writers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, replay, err := repository.Create(ctx, input)
			results <- result{incident: got, replay: replay, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	var id string
	newCount, replayCount := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Create: %v", result.err)
		}
		if id == "" {
			id = result.incident.ID
		} else if result.incident.ID != id {
			t.Fatalf("concurrent requests returned ids %q and %q", id, result.incident.ID)
		}
		if result.replay {
			replayCount++
		} else {
			newCount++
		}
	}
	if newCount != 1 || replayCount != writers-1 {
		t.Fatalf("new creates=%d replays=%d; want 1 and %d", newCount, replayCount, writers-1)
	}

	var count int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM incidents WHERE tenant_id = $1 AND source = $2 AND source_event_id = $3`, input.TenantID, input.Source, input.SourceEventID).Scan(&count); err != nil {
		t.Fatalf("count concurrent idempotency rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for concurrent key = %d, want 1", count)
	}
}

func uniqueInput() incident.CreateInput {
	return incident.CreateInput{
		TenantID:      "integration-tenant",
		Source:        "integration-test",
		SourceEventID: fmt.Sprintf("event-%d", time.Now().UnixNano()),
		Service:       "payment-api",
		Title:         "Synthetic integration incident",
		Description:   "Created by the PostgreSQL integration test.",
		Severity:      incident.SeverityHigh,
		OccurredAt:    time.Date(2026, 10, 12, 10, 15, 0, 123456789, time.UTC),
	}
}
