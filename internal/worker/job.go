package worker

import (
	"context"
	"errors"
	"time"
)

// JobStatus represents the durable lifecycle state of an incident processing job.
type JobStatus string

const (
	StatusPending    JobStatus = "pending"
	StatusProcessing JobStatus = "processing"
	StatusCompleted  JobStatus = "completed"
	StatusFailed     JobStatus = "failed"
	StatusDeadLetter JobStatus = "dead_letter"
)

// ErrLeaseLost is returned when a worker attempts to mutate a job whose lease_token
// no longer matches the active lease owner in PostgreSQL (fencing protection).
var ErrLeaseLost = errors.New("job lease lost or expired")

// Job represents a durable incident-processing work item.
type Job struct {
	ID             string
	TenantID       string
	IncidentID     string
	Status         JobStatus
	Attempts       int
	MaxAttempts    int
	AvailableAt    time.Time
	LockedAt       *time.Time
	LockedBy       string
	LeaseToken     string
	LeaseExpiresAt *time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

// JobStore is the persistence boundary for durable job claiming, leasing, and state transitions.
type JobStore interface {
	ClaimNext(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (Job, bool, error)
	ReclaimExpired(ctx context.Context, workerID string, leaseDuration time.Duration, now time.Time) (Job, bool, error)
	Complete(ctx context.Context, jobID, leaseToken string, now time.Time) error
	Fail(ctx context.Context, jobID, leaseToken, errMsg string, nextAvailableAt time.Time, deadLetter bool, now time.Time) error
	Release(ctx context.Context, jobID, leaseToken string, now time.Time) error
	RecordEffect(ctx context.Context, idempotencyKey, jobID, tenantID, effectName, summary string) (alreadyExecuted bool, err error)
	QueueDepth(ctx context.Context) (int64, error)
}
