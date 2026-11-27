package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"sentinel/internal/metrics"
	"sentinel/internal/worker"
)

// JobRepository implements durable PostgreSQL-backed job claiming, leasing,
// fencing, and idempotent side-effect tracking.
type JobRepository struct {
	db   *sql.DB
	prom *metrics.SentinelMetrics
}

// NewJobRepository creates a JobRepository over an existing database pool.
func NewJobRepository(db *sql.DB) *JobRepository {
	return &JobRepository{db: db}
}

// SetMetrics attaches Prometheus-compatible DB operation metrics to this repository.
func (r *JobRepository) SetMetrics(m *metrics.SentinelMetrics) {
	r.prom = m
}

var _ worker.JobStore = (*JobRepository)(nil)

const jobColumns = `j.id::text, j.tenant_id, j.incident_id::text, j.status, j.attempts, j.max_attempts, j.available_at, j.locked_at, COALESCE(j.locked_by, ''), COALESCE(j.lease_token::text, ''), j.lease_expires_at, COALESCE(j.last_error, ''), j.created_at, j.updated_at, j.completed_at`

// ClaimNext atomically claims the next ready pending or failed job using FOR UPDATE SKIP LOCKED.
func (r *JobRepository) ClaimNext(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (worker.Job, bool, error) {
	nowUTC := now.UTC()
	leaseExpires := nowUTC.Add(leaseDuration)

	const query = `
WITH candidate AS (
	SELECT id
	FROM incident_jobs
	WHERE status IN ('pending', 'failed') AND available_at <= $1
	ORDER BY available_at ASC, created_at ASC
	FOR UPDATE SKIP LOCKED
	LIMIT 1
)
UPDATE incident_jobs j
SET status = 'processing',
	locked_by = $2,
	locked_at = $1,
	lease_token = gen_random_uuid(),
	lease_expires_at = $3,
	attempts = j.attempts + 1,
	updated_at = $1
FROM candidate
WHERE j.id = candidate.id
RETURNING ` + jobColumns

	job, err := scanJob(r.db.QueryRowContext(ctx, query, nowUTC, workerID, leaseExpires))
	if errors.Is(err, sql.ErrNoRows) {
		return worker.Job{}, false, nil
	}
	if err != nil {
		return worker.Job{}, false, fmt.Errorf("claim next job: %w", err)
	}
	return job, true, nil
}

// ReclaimExpired atomically reclaims a processing job whose lease has expired,
// issuing a fresh lease_token so the original stale worker cannot overwrite it.
func (r *JobRepository) ReclaimExpired(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (worker.Job, bool, error) {
	nowUTC := now.UTC()
	leaseExpires := nowUTC.Add(leaseDuration)

	const query = `
WITH candidate AS (
	SELECT id
	FROM incident_jobs
	WHERE status = 'processing' AND lease_expires_at <= $1
	ORDER BY lease_expires_at ASC
	FOR UPDATE SKIP LOCKED
	LIMIT 1
)
UPDATE incident_jobs j
SET locked_by = $2,
	locked_at = $1,
	lease_token = gen_random_uuid(),
	lease_expires_at = $3,
	attempts = j.attempts + 1,
	updated_at = $1
FROM candidate
WHERE j.id = candidate.id
RETURNING ` + jobColumns

	job, err := scanJob(r.db.QueryRowContext(ctx, query, nowUTC, workerID, leaseExpires))
	if errors.Is(err, sql.ErrNoRows) {
		return worker.Job{}, false, nil
	}
	if err != nil {
		return worker.Job{}, false, fmt.Errorf("reclaim expired job: %w", err)
	}
	return job, true, nil
}

// Complete marks a job as completed only if the caller still holds the active lease_token.
func (r *JobRepository) Complete(ctx context.Context, jobID, leaseToken string, now time.Time) error {
	nowUTC := now.UTC()
	const query = `
UPDATE incident_jobs
SET status = 'completed',
	completed_at = $3,
	updated_at = $3,
	lease_expires_at = NULL
WHERE id = $1::uuid
  AND status = 'processing'
  AND lease_token = $2::uuid`

	res, err := r.db.ExecContext(ctx, query, jobID, leaseToken, nowUTC)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete job rows affected: %w", err)
	}
	if affected == 0 {
		return worker.ErrLeaseLost
	}
	return nil
}

// Fail transitions a job to failed (for retry) or dead_letter only if lease_token matches.
func (r *JobRepository) Fail(ctx context.Context, jobID, leaseToken, errMsg string, nextAvailableAt time.Time, deadLetter bool, now time.Time) error {
	nowUTC := now.UTC()
	status := string(worker.StatusFailed)
	if deadLetter {
		status = string(worker.StatusDeadLetter)
	}

	const query = `
UPDATE incident_jobs
SET status = $4,
	last_error = $5,
	available_at = $6,
	updated_at = $3,
	lease_expires_at = NULL
WHERE id = $1::uuid
  AND status = 'processing'
  AND lease_token = $2::uuid`

	res, err := r.db.ExecContext(ctx, query, jobID, leaseToken, nowUTC, status, errMsg, nextAvailableAt.UTC())
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail job rows affected: %w", err)
	}
	if affected == 0 {
		return worker.ErrLeaseLost
	}
	return nil
}

// Release returns an interrupted job to pending during graceful shutdown only if lease_token matches.
func (r *JobRepository) Release(ctx context.Context, jobID, leaseToken string, now time.Time) error {
	nowUTC := now.UTC()
	const query = `
UPDATE incident_jobs
SET status = 'pending',
	attempts = GREATEST(0, attempts - 1),
	locked_by = NULL,
	locked_at = NULL,
	lease_token = NULL,
	lease_expires_at = NULL,
	updated_at = $3
WHERE id = $1::uuid
  AND status = 'processing'
  AND lease_token = $2::uuid`

	res, err := r.db.ExecContext(ctx, query, jobID, leaseToken, nowUTC)
	if err != nil {
		return fmt.Errorf("release job: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release job rows affected: %w", err)
	}
	if affected == 0 {
		return worker.ErrLeaseLost
	}
	return nil
}

// RecordEffect atomically records an idempotent processing side-effect for a job.
// Returns alreadyExecuted=true if an identical idempotency_key was already recorded.
func (r *JobRepository) RecordEffect(ctx context.Context, idempotencyKey, jobID, tenantID, effectName, summary string) (bool, error) {
	const query = `
INSERT INTO incident_job_effects (idempotency_key, job_id, tenant_id, effect_name, result_summary)
VALUES ($1, $2::uuid, $3, $4, $5)
ON CONFLICT (idempotency_key) DO NOTHING`

	res, err := r.db.ExecContext(ctx, query, idempotencyKey, jobID, tenantID, effectName, summary)
	if err != nil {
		return false, fmt.Errorf("record job effect: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record job effect rows affected: %w", err)
	}
	return affected == 0, nil
}

// QueueDepth returns the count of active/queued jobs (pending, failed, or processing).
func (r *JobRepository) QueueDepth(ctx context.Context) (int64, error) {
	start := time.Now()
	var depth int64
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM incident_jobs WHERE status IN ('pending', 'failed', 'processing')`).Scan(&depth)
	r.prom.ObserveDBOperation("job_queue_depth", time.Since(start), err)
	if err != nil {
		return 0, fmt.Errorf("query queue depth: %w", err)
	}
	return depth, nil
}

// OldestReadyJobAgeSeconds returns the age in seconds of the oldest ready job, or 0 if none are waiting.
func (r *JobRepository) OldestReadyJobAgeSeconds(ctx context.Context, now time.Time) (float64, error) {
	start := time.Now()
	var oldest sql.NullTime
	err := r.db.QueryRowContext(ctx, `SELECT min(available_at) FROM incident_jobs WHERE status IN ('pending', 'failed') AND available_at <= $1`, now.UTC()).Scan(&oldest)
	r.prom.ObserveDBOperation("job_oldest_age", time.Since(start), err)
	if err != nil {
		return 0, fmt.Errorf("query oldest ready job age: %w", err)
	}
	if !oldest.Valid {
		return 0, nil
	}
	age := now.UTC().Sub(oldest.Time.UTC()).Seconds()
	if age < 0 {
		return 0, nil
	}
	return age, nil
}

func scanJob(row rowScanner) (worker.Job, error) {
	var job worker.Job
	var status string
	var lockedAt sql.NullTime
	var leaseExpiresAt sql.NullTime
	var completedAt sql.NullTime
	err := row.Scan(
		&job.ID,
		&job.TenantID,
		&job.IncidentID,
		&status,
		&job.Attempts,
		&job.MaxAttempts,
		&job.AvailableAt,
		&lockedAt,
		&job.LockedBy,
		&job.LeaseToken,
		&leaseExpiresAt,
		&job.LastError,
		&job.CreatedAt,
		&job.UpdatedAt,
		&completedAt,
	)
	if err != nil {
		return worker.Job{}, err
	}
	job.Status = worker.JobStatus(status)
	job.AvailableAt = job.AvailableAt.UTC()
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()
	if lockedAt.Valid {
		t := lockedAt.Time.UTC()
		job.LockedAt = &t
	}
	if leaseExpiresAt.Valid {
		t := leaseExpiresAt.Time.UTC()
		job.LeaseExpiresAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time.UTC()
		job.CompletedAt = &t
	}
	return job, nil
}
