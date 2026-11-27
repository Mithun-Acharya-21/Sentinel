package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"sentinel/internal/worker"
)

func TestAtomicIncidentAndJobCreationAndIdempotentReplay(t *testing.T) {
	db, incRepo := openIntegrationRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	input := uniqueInput()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM incidents WHERE tenant_id = $1`, input.TenantID)
	})

	created, replay, err := incRepo.Create(ctx, input)
	if err != nil {
		t.Fatalf("Create failed (apply migrations/004_create_incident_jobs.up.sql first): %v", err)
	}
	if replay {
		t.Fatal("first Create should not be replay")
	}

	var jobCount int
	var status string
	if err := db.QueryRowContext(ctx, `SELECT count(*), min(status) FROM incident_jobs WHERE incident_id = $1::uuid AND tenant_id = $2`, created.ID, input.TenantID).Scan(&jobCount, &status); err != nil {
		t.Fatalf("query incident_jobs failed: %v", err)
	}
	if jobCount != 1 || status != string(worker.StatusPending) {
		t.Fatalf("jobCount=%d status=%q, want 1 and 'pending'", jobCount, status)
	}

	// Idempotent replay must NOT create a second job
	replayed, replay2, err := incRepo.Create(ctx, input)
	if err != nil || !replay2 || replayed.ID != created.ID {
		t.Fatalf("replay failed: id=%q replay=%v err=%v", replayed.ID, replay2, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM incident_jobs WHERE incident_id = $1::uuid`, created.ID).Scan(&jobCount); err != nil {
		t.Fatalf("count jobs after replay failed: %v", err)
	}
	if jobCount != 1 {
		t.Fatalf("duplicate job created on replay: jobCount=%d, want 1", jobCount)
	}
}

func TestJobRepositoryClaimReclaimFencingAndEffects(t *testing.T) {
	db, incRepo := openIntegrationRepository(t)
	jobRepo := NewJobRepository(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	input := uniqueInput()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM incidents WHERE tenant_id = $1`, input.TenantID)
	})

	created, _, err := incRepo.Create(ctx, input)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	now := time.Now().UTC()
	// 1. Worker 1 claims the job
	job1, ok, err := jobRepo.ClaimNext(ctx, "worker-1", 10*time.Second, now)
	if err != nil || !ok {
		t.Fatalf("ClaimNext failed: ok=%v err=%v", ok, err)
	}
	if job1.IncidentID != created.ID || job1.Status != worker.StatusProcessing || job1.LeaseToken == "" {
		t.Fatalf("unexpected claimed job1: %#v", job1)
	}

	// 2. Concurrent Worker 2 cannot claim the active job
	if _, ok2, err := jobRepo.ClaimNext(ctx, "worker-2", 10*time.Second, now); err != nil || ok2 {
		t.Fatalf("worker-2 claimed already locked job: ok=%v err=%v", ok2, err)
	}

	// 3. After lease expires, Worker 2 reclaims with a NEW lease_token
	expiredNow := now.Add(15 * time.Second)
	job2, okReclaim, err := jobRepo.ReclaimExpired(ctx, "worker-2", 10*time.Second, expiredNow)
	if err != nil || !okReclaim {
		t.Fatalf("ReclaimExpired failed: ok=%v err=%v", okReclaim, err)
	}
	if job2.ID != job1.ID || job2.LeaseToken == job1.LeaseToken || job2.Attempts != 2 {
		t.Fatalf("unexpected reclaimed job2: %#v (prev token=%q)", job2, job1.LeaseToken)
	}

	// 4. Stale Worker 1 completion is rejected with ErrLeaseLost
	if err := jobRepo.Complete(ctx, job1.ID, job1.LeaseToken, expiredNow); !errors.Is(err, worker.ErrLeaseLost) {
		t.Fatalf("stale Complete err = %v, want ErrLeaseLost", err)
	}

	// 5. Idempotent effect recording
	idemKey := fmt.Sprintf("job:%s:effect:triage", job2.ID)
	dup1, err := jobRepo.RecordEffect(ctx, idemKey, job2.ID, job2.TenantID, "triage", "done")
	if err != nil || dup1 {
		t.Fatalf("first RecordEffect dup=%v err=%v", dup1, err)
	}
	dup2, err := jobRepo.RecordEffect(ctx, idemKey, job2.ID, job2.TenantID, "triage", "done")
	if err != nil || !dup2 {
		t.Fatalf("second RecordEffect dup=%v err=%v, want dup=true", dup2, err)
	}

	// 6. Worker 2 completes with active lease_token
	if err := jobRepo.Complete(ctx, job2.ID, job2.LeaseToken, expiredNow.Add(time.Second)); err != nil {
		t.Fatalf("active Complete failed: %v", err)
	}
}

func TestJobRepositoryConcurrentSkipLockedClaiming(t *testing.T) {
	db, incRepo := openIntegrationRepository(t)
	jobRepo := NewJobRepository(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tenantID := fmt.Sprintf("tenant-skip-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM incidents WHERE tenant_id = $1`, tenantID)
	})

	const total = 10
	for i := 0; i < total; i++ {
		in := uniqueInput()
		in.TenantID = tenantID
		in.SourceEventID = fmt.Sprintf("%s-%d", in.SourceEventID, i)
		if _, _, err := incRepo.Create(ctx, in); err != nil {
			t.Fatalf("Create %d failed: %v", i, err)
		}
	}

	var mu sync.Mutex
	claimedIDs := make(map[string]int)
	var wg sync.WaitGroup
	start := make(chan struct{})
	now := time.Now().UTC()

	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for {
				job, ok, err := jobRepo.ClaimNext(ctx, fmt.Sprintf("w-%d", id), 30*time.Second, now)
				if err != nil {
					t.Errorf("ClaimNext failed: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimedIDs[job.ID]++
				mu.Unlock()
			}
		}(w)
	}

	close(start)
	wg.Wait()

	if len(claimedIDs) < total {
		t.Fatalf("claimed %d jobs, expected at least %d", len(claimedIDs), total)
	}
	for id, count := range claimedIDs {
		if count != 1 {
			t.Fatalf("job %s claimed %d times, want 1", id, count)
		}
	}
}
