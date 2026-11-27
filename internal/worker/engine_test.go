package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memJobStore struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string
	effects map[string]string
	seq     int
	failErr error
}

func newMemJobStore() *memJobStore {
	return &memJobStore{
		jobs:    make(map[string]*Job),
		effects: make(map[string]string),
	}
}

func (m *memJobStore) AddPending(tenantID, incidentID string, maxAttempts int, availableAt time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("job-%d", m.seq)
	m.jobs[id] = &Job{
		ID:          id,
		TenantID:    tenantID,
		IncidentID:  incidentID,
		Status:      StatusPending,
		MaxAttempts: maxAttempts,
		AvailableAt: availableAt,
		CreatedAt:   availableAt,
		UpdatedAt:   availableAt,
	}
	m.order = append(m.order, id)
	return id
}

func (m *memJobStore) Get(id string) Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.jobs[id]
}

func (m *memJobStore) ClaimNext(_ context.Context, workerID string, leaseDuration time.Duration, now time.Time) (Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failErr != nil {
		return Job{}, false, m.failErr
	}
	for _, id := range m.order {
		j := m.jobs[id]
		if (j.Status == StatusPending || j.Status == StatusFailed) && !j.AvailableAt.After(now) {
			m.seq++
			j.Status = StatusProcessing
			j.LockedBy = workerID
			t := now
			j.LockedAt = &t
			j.LeaseToken = fmt.Sprintf("lease-%d", m.seq)
			exp := now.Add(leaseDuration)
			j.LeaseExpiresAt = &exp
			j.Attempts++
			j.UpdatedAt = now
			return *j, true, nil
		}
	}
	return Job{}, false, nil
}

func (m *memJobStore) ReclaimExpired(_ context.Context, workerID string, leaseDuration time.Duration, now time.Time) (Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failErr != nil {
		return Job{}, false, m.failErr
	}
	for _, id := range m.order {
		j := m.jobs[id]
		if j.Status == StatusProcessing && j.LeaseExpiresAt != nil && !j.LeaseExpiresAt.After(now) {
			m.seq++
			j.LockedBy = workerID
			t := now
			j.LockedAt = &t
			j.LeaseToken = fmt.Sprintf("lease-%d", m.seq)
			exp := now.Add(leaseDuration)
			j.LeaseExpiresAt = &exp
			j.Attempts++
			j.UpdatedAt = now
			return *j, true, nil
		}
	}
	return Job{}, false, nil
}

func (m *memJobStore) Complete(_ context.Context, jobID, leaseToken string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failErr != nil {
		return m.failErr
	}
	j, ok := m.jobs[jobID]
	if !ok || j.Status != StatusProcessing || j.LeaseToken != leaseToken {
		return ErrLeaseLost
	}
	j.Status = StatusCompleted
	t := now
	j.CompletedAt = &t
	j.UpdatedAt = now
	j.LeaseExpiresAt = nil
	return nil
}

func (m *memJobStore) Fail(_ context.Context, jobID, leaseToken, errMsg string, nextAvailableAt time.Time, deadLetter bool, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failErr != nil {
		return m.failErr
	}
	j, ok := m.jobs[jobID]
	if !ok || j.Status != StatusProcessing || j.LeaseToken != leaseToken {
		return ErrLeaseLost
	}
	if deadLetter {
		j.Status = StatusDeadLetter
	} else {
		j.Status = StatusFailed
	}
	j.LastError = errMsg
	j.AvailableAt = nextAvailableAt
	j.UpdatedAt = now
	j.LeaseExpiresAt = nil
	return nil
}

func (m *memJobStore) Release(_ context.Context, jobID, leaseToken string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobID]
	if !ok || j.Status != StatusProcessing || j.LeaseToken != leaseToken {
		return ErrLeaseLost
	}
	j.Status = StatusPending
	if j.Attempts > 0 {
		j.Attempts--
	}
	j.LockedBy = ""
	j.LockedAt = nil
	j.LeaseToken = ""
	j.LeaseExpiresAt = nil
	j.UpdatedAt = now
	return nil
}

func (m *memJobStore) RecordEffect(_ context.Context, idempotencyKey, _, _, effectName, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.effects[idempotencyKey]; exists {
		return true, nil
	}
	m.effects[idempotencyKey] = effectName
	return false, nil
}

func (m *memJobStore) QueueDepth(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for _, j := range m.jobs {
		if j.Status == StatusPending || j.Status == StatusFailed || j.Status == StatusProcessing {
			count++
		}
	}
	return count, nil
}

type funcProcessor struct {
	fn func(ctx context.Context, job Job) error
}

func (f funcProcessor) Process(ctx context.Context, job Job) error {
	return f.fn(ctx, job)
}

func TestLeaseExpirationReclaimAndStaleCompletionRejected(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	store := newMemJobStore()
	jobID := store.AddPending("tenant-a", "inc-1", 5, now)

	worker1Started := make(chan struct{})
	allowWorker1Finish := make(chan struct{})
	var worker1Calls atomic.Int32

	proc := funcProcessor{fn: func(_ context.Context, _ Job) error {
		if worker1Calls.Add(1) == 1 {
			close(worker1Started)
			<-allowWorker1Finish
		}
		return nil
	}}

	engine := NewEngine(store, proc, Config{
		WorkerCount:   2,
		LeaseDuration: 10 * time.Second,
		Clock:         clock,
	})

	// 1. Worker 1 claims the job and hangs inside Process
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = engine.ProcessOne(context.Background(), context.Background(), "worker-1")
	}()
	<-worker1Started

	// 2. Lease expires
	mu.Lock()
	now = now.Add(15 * time.Second)
	mu.Unlock()

	// 3. Worker 2 reclaims the expired lease and completes the job
	processed, err := engine.ProcessOne(context.Background(), context.Background(), "worker-2")
	if err != nil || !processed {
		t.Fatalf("worker-2 ProcessOne failed: processed=%v err=%v", processed, err)
	}
	if got := store.Get(jobID); got.Status != StatusCompleted || got.LockedBy != "worker-2" {
		t.Fatalf("after worker-2, job = %#v", got)
	}

	// 4. Original worker-1 wakes up and attempts to Complete with its stale lease_token -> rejected!
	close(allowWorker1Finish)
	wg.Wait()

	if got := store.Get(jobID); got.Status != StatusCompleted || got.LockedBy != "worker-2" {
		t.Fatalf("stale worker overwrote job state: %#v", got)
	}
	metrics := engine.Metrics()
	if metrics.LeaseRecoveries != 1 {
		t.Fatalf("LeaseRecoveries = %d, want 1", metrics.LeaseRecoveries)
	}
	if metrics.StaleLeaseRejections != 1 {
		t.Fatalf("StaleLeaseRejections = %d, want 1", metrics.StaleLeaseRejections)
	}
	if metrics.JobsCompleted != 1 {
		t.Fatalf("JobsCompleted = %d, want 1", metrics.JobsCompleted)
	}
}

func TestRetrySchedulingMaxAttemptsAndDeadLetter(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	store := newMemJobStore()
	jobID := store.AddPending("tenant-a", "inc-retry", 3, now)

	proc := funcProcessor{fn: func(_ context.Context, _ Job) error {
		return errors.New("transient downstream timeout")
	}}

	engine := NewEngine(store, proc, Config{
		LeaseDuration: 30 * time.Second,
		RetryPolicy: RetryPolicy{
			MaxAttempts: 3,
			BaseDelay:   2 * time.Second,
			MaxDelay:    10 * time.Second,
			RandFloat64: func() float64 { return 1.0 }, // deterministic upper bound
		},
		Clock: clock,
	})

	// Attempt 1 -> failed, scheduled for now + 2s
	_, _ = engine.ProcessOne(context.Background(), context.Background(), "w1")
	j := store.Get(jobID)
	if j.Status != StatusFailed || j.Attempts != 1 || !j.AvailableAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("after attempt 1: %#v", j)
	}

	// Before available_at -> not claimed
	if claimed, _ := engine.ProcessOne(context.Background(), context.Background(), "w1"); claimed {
		t.Fatal("job claimed before available_at")
	}

	// Attempt 2 -> failed, scheduled for now + 4s
	now = j.AvailableAt
	_, _ = engine.ProcessOne(context.Background(), context.Background(), "w1")
	j = store.Get(jobID)
	if j.Status != StatusFailed || j.Attempts != 2 || !j.AvailableAt.Equal(now.Add(4*time.Second)) {
		t.Fatalf("after attempt 2: %#v", j)
	}

	// Attempt 3 (max_attempts=3) -> dead_letter!
	now = j.AvailableAt
	_, _ = engine.ProcessOne(context.Background(), context.Background(), "w1")
	j = store.Get(jobID)
	if j.Status != StatusDeadLetter || j.Attempts != 3 || j.LastError != "transient downstream timeout" {
		t.Fatalf("after attempt 3: %#v", j)
	}

	// Non-retryable error -> immediate dead_letter on attempt 1
	job2 := store.AddPending("tenant-a", "inc-perm", 5, now)
	permEngine := NewEngine(store, funcProcessor{fn: func(_ context.Context, _ Job) error {
		return MarkNonRetryable(errors.New("invalid incident schema"))
	}}, Config{Clock: clock})
	_, _ = permEngine.ProcessOne(context.Background(), context.Background(), "w1")
	j2 := store.Get(job2)
	if j2.Status != StatusDeadLetter || j2.Attempts != 1 {
		t.Fatalf("non-retryable job should dead-letter immediately: %#v", j2)
	}
}

func TestIdempotentProcessorPreventsDuplicateExternalEffects(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	store := newMemJobStore()
	jobID := store.AddPending("tenant-a", "inc-idem", 3, now)
	job := store.Get(jobID)

	proc := NewDefaultIncidentProcessor(store)
	if err := proc.Process(context.Background(), job); err != nil {
		t.Fatalf("first Process failed: %v", err)
	}
	if err := proc.Process(context.Background(), job); err != nil {
		t.Fatalf("duplicate Process failed: %v", err)
	}
	if len(store.effects) != 1 {
		t.Fatalf("effects count = %d, want 1", len(store.effects))
	}
}

func TestGracefulShutdownReleasesInterruptedLease(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	store := newMemJobStore()
	jobID := store.AddPending("tenant-a", "inc-shutdown", 3, now)

	started := make(chan struct{})
	proc := funcProcessor{fn: func(ctx context.Context, _ Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}

	engine := NewEngine(store, proc, Config{
		WorkerCount:   1,
		PollInterval:  5 * time.Millisecond,
		LeaseDuration: 30 * time.Second,
		ShutdownGrace: 20 * time.Millisecond,
		Clock:         func() time.Time { return now },
	})

	engine.Start(context.Background())
	<-started

	err := engine.Stop(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop err = %v, want DeadlineExceeded after grace timeout", err)
	}

	// Interrupted job must be released back to pending with attempts restored to 0
	j := store.Get(jobID)
	if j.Status != StatusPending || j.Attempts != 0 || j.LeaseToken != "" {
		t.Fatalf("interrupted job was not released cleanly: %#v", j)
	}
}

func TestConcurrentWorkersClaimDisjointJobs(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	store := newMemJobStore()
	const totalJobs = 25
	for i := 0; i < totalJobs; i++ {
		store.AddPending("tenant-a", fmt.Sprintf("inc-%d", i), 3, now)
	}

	var mu sync.Mutex
	seen := make(map[string]int)
	proc := funcProcessor{fn: func(_ context.Context, job Job) error {
		mu.Lock()
		seen[job.ID]++
		mu.Unlock()
		return nil
	}}

	engine := NewEngine(store, proc, Config{
		WorkerCount:   5,
		PollInterval:  2 * time.Millisecond,
		LeaseDuration: 30 * time.Second,
		ShutdownGrace: 2 * time.Second,
		Clock:         func() time.Time { return now },
	})

	engine.Start(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if engine.Metrics().JobsCompleted == totalJobs {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if len(seen) != totalJobs {
		t.Fatalf("processed %d unique jobs, want %d", len(seen), totalJobs)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("job %s processed %d times, want 1", id, count)
		}
	}
}
