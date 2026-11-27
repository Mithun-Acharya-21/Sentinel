package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"sentinel/internal/metrics"
	"sentinel/internal/remediation"
)

// RemediationRepository persists controlled remediation execution state and audit records in PostgreSQL.
type RemediationRepository struct {
	db   *sql.DB
	prom *metrics.SentinelMetrics
}

// NewRemediationRepository creates a RemediationRepository over an existing database pool.
func NewRemediationRepository(db *sql.DB) *RemediationRepository {
	return &RemediationRepository{db: db}
}

// SetMetrics attaches Prometheus-compatible DB operation metrics to this repository.
func (r *RemediationRepository) SetMetrics(m *metrics.SentinelMetrics) {
	r.prom = m
}

var _ remediation.ExecutionStore = (*RemediationRepository)(nil)

const remediationColumns = `id::text, tenant_id, incident_id::text, triage_id::text, approval_id::text, action_type, action_version, target_resource, parameters::text, idempotency_key, approver_id, status, attempts, COALESCE(execution_token::text, ''), lease_expires_at, verification_passed, verification_summary::text, failure_category, failure_message, requested_at, started_at, completed_at, created_at, updated_at`

// ClaimExecution atomically claims or retries an execution lease in PostgreSQL,
// returning alreadySucceeded=true if the execution has already completed successfully.
func (r *RemediationRepository) ClaimExecution(ctx context.Context, req remediation.ClaimRequest) (remediation.ExecutionRecord, bool, error) {
	start := time.Now()
	nowUTC := req.Now.UTC()
	leaseExpires := nowUTC.Add(req.LeaseDuration)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.prom.ObserveDBOperation("remediation_claim", time.Since(start), err)
		return remediation.ExecutionRecord{}, false, remediationStorageError("begin claim tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	const selectExisting = `SELECT ` + remediationColumns + ` FROM remediation_executions WHERE approval_id = $1::uuid AND tenant_id = $2 FOR UPDATE`
	existing, err := scanExecutionRecord(tx.QueryRowContext(ctx, selectExisting, req.ApprovalID, req.TenantID))
	if err == nil {
		if existing.Status == remediation.StatusSucceeded {
			_ = tx.Commit()
			r.prom.ObserveDBOperation("remediation_claim", time.Since(start), nil)
			return existing, true, nil
		}
		if existing.Status == remediation.StatusExecuting && existing.LeaseExpiresAt != nil && nowUTC.Before(existing.LeaseExpiresAt.UTC()) {
			r.prom.ObserveDBOperation("remediation_claim", time.Since(start), nil)
			return remediation.ExecutionRecord{}, false, remediation.ErrExecutionConflict
		}

		const reclaimSQL = `
UPDATE remediation_executions
SET status = 'executing',
	attempts = attempts + 1,
	execution_token = gen_random_uuid(),
	lease_expires_at = $3,
	started_at = $4,
	updated_at = $4
WHERE id = $1::uuid AND tenant_id = $2
RETURNING ` + remediationColumns

		reclaimed, updErr := scanExecutionRecord(tx.QueryRowContext(ctx, reclaimSQL, existing.ID, req.TenantID, leaseExpires, nowUTC))
		if updErr != nil {
			r.prom.ObserveDBOperation("remediation_claim", time.Since(start), updErr)
			return remediation.ExecutionRecord{}, false, remediationStorageError("reclaim execution", updErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			r.prom.ObserveDBOperation("remediation_claim", time.Since(start), commitErr)
			return remediation.ExecutionRecord{}, false, remediationStorageError("commit reclaim execution", commitErr)
		}
		r.prom.ObserveDBOperation("remediation_claim", time.Since(start), nil)
		return reclaimed, false, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		r.prom.ObserveDBOperation("remediation_claim", time.Since(start), err)
		return remediation.ExecutionRecord{}, false, remediationStorageError("select existing execution", err)
	}

	params := req.Action.Parameters
	if params == nil {
		params = map[string]string{}
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return remediation.ExecutionRecord{}, false, fmt.Errorf("marshal parameters: %w", err)
	}

	const insertSQL = `
INSERT INTO remediation_executions (
	tenant_id, incident_id, triage_id, approval_id, action_type, action_version,
	target_resource, parameters, idempotency_key, approver_id, status, attempts,
	execution_token, lease_expires_at, requested_at, started_at
) VALUES (
	$1, $2::uuid, $3::uuid, $4::uuid, $5, $6,
	$7, $8::jsonb, $9, $10, 'executing', 1,
	gen_random_uuid(), $11, $12, $13
)
RETURNING ` + remediationColumns

	created, insErr := scanExecutionRecord(tx.QueryRowContext(ctx, insertSQL,
		req.TenantID,
		req.IncidentID,
		req.TriageID,
		req.ApprovalID,
		string(req.Action.ActionType),
		req.Action.ActionVersion,
		req.Action.TargetResource,
		string(paramsJSON),
		req.Action.IdempotencyKey,
		req.ApproverID,
		leaseExpires,
		req.Action.RequestedAt.UTC(),
		nowUTC,
	))
	if insErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(insErr, &pgErr) && pgErr.Code == "23505" {
			r.prom.ObserveDBOperation("remediation_claim", time.Since(start), nil)
			return remediation.ExecutionRecord{}, false, remediation.ErrExecutionConflict
		}
		r.prom.ObserveDBOperation("remediation_claim", time.Since(start), insErr)
		return remediation.ExecutionRecord{}, false, remediationStorageError("insert execution", insErr)
	}

	if commitErr := tx.Commit(); commitErr != nil {
		r.prom.ObserveDBOperation("remediation_claim", time.Since(start), commitErr)
		return remediation.ExecutionRecord{}, false, remediationStorageError("commit execution claim", commitErr)
	}
	r.prom.ObserveDBOperation("remediation_claim", time.Since(start), nil)
	return created, false, nil
}

// CompleteExecution persists the final execution and verification outcome only if execution_token matches.
func (r *RemediationRepository) CompleteExecution(ctx context.Context, execID, executionToken string, status remediation.ExecutionStatus, verification remediation.PostActionVerification, failCategory, failMsg string, now time.Time) (remediation.ExecutionRecord, error) {
	start := time.Now()
	verJSON, err := json.Marshal(verification)
	if err != nil {
		return remediation.ExecutionRecord{}, fmt.Errorf("marshal verification summary: %w", err)
	}

	nowUTC := now.UTC()
	const query = `
UPDATE remediation_executions
SET status = $3,
	verification_passed = $4,
	verification_summary = $5::jsonb,
	failure_category = $6,
	failure_message = $7,
	lease_expires_at = NULL,
	completed_at = $8,
	updated_at = $8
WHERE id = $1::uuid
  AND status = 'executing'
  AND execution_token = $2::uuid
RETURNING ` + remediationColumns

	updated, err := scanExecutionRecord(r.db.QueryRowContext(ctx, query,
		execID,
		executionToken,
		string(status),
		verification.VerificationPassed,
		string(verJSON),
		failCategory,
		failMsg,
		nowUTC,
	))
	if errors.Is(err, sql.ErrNoRows) {
		r.prom.ObserveDBOperation("remediation_complete", time.Since(start), nil)
		return remediation.ExecutionRecord{}, remediation.ErrExecutionLeaseLost
	}
	r.prom.ObserveDBOperation("remediation_complete", time.Since(start), err)
	if err != nil {
		return remediation.ExecutionRecord{}, remediationStorageError("complete execution", err)
	}
	return updated, nil
}

// GetByIncidentID retrieves a tenant-scoped remediation execution record.
func (r *RemediationRepository) GetByIncidentID(ctx context.Context, tenantID, incidentID string) (remediation.ExecutionRecord, error) {
	if strings.TrimSpace(tenantID) == "" {
		return remediation.ExecutionRecord{}, remediation.ErrExecutionNotFound
	}
	start := time.Now()
	const query = `SELECT ` + remediationColumns + ` FROM remediation_executions WHERE incident_id = $1::uuid AND tenant_id = $2`
	rec, err := scanExecutionRecord(r.db.QueryRowContext(ctx, query, incidentID, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		r.prom.ObserveDBOperation("remediation_get", time.Since(start), nil)
		return remediation.ExecutionRecord{}, remediation.ErrExecutionNotFound
	}
	r.prom.ObserveDBOperation("remediation_get", time.Since(start), err)
	if err != nil {
		return remediation.ExecutionRecord{}, remediationStorageError("get remediation execution", err)
	}
	return rec, nil
}

func scanExecutionRecord(row rowScanner) (remediation.ExecutionRecord, error) {
	var rec remediation.ExecutionRecord
	var actionType, status string
	var paramsJSON, verJSON string
	var leaseExpiresAt, startedAt, completedAt sql.NullTime
	var verPassed sql.NullBool

	err := row.Scan(
		&rec.ID,
		&rec.TenantID,
		&rec.IncidentID,
		&rec.TriageID,
		&rec.ApprovalID,
		&actionType,
		&rec.ActionVersion,
		&rec.TargetResource,
		&paramsJSON,
		&rec.IdempotencyKey,
		&rec.ApproverID,
		&status,
		&rec.Attempts,
		&rec.ExecutionToken,
		&leaseExpiresAt,
		&verPassed,
		&verJSON,
		&rec.FailureCategory,
		&rec.FailureMessage,
		&rec.RequestedAt,
		&startedAt,
		&completedAt,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		return remediation.ExecutionRecord{}, err
	}

	rec.ActionType = remediation.ActionType(actionType)
	rec.Status = remediation.ExecutionStatus(status)
	rec.RequestedAt = rec.RequestedAt.UTC()
	rec.CreatedAt = rec.CreatedAt.UTC()
	rec.UpdatedAt = rec.UpdatedAt.UTC()
	if leaseExpiresAt.Valid {
		t := leaseExpiresAt.Time.UTC()
		rec.LeaseExpiresAt = &t
	}
	if startedAt.Valid {
		t := startedAt.Time.UTC()
		rec.StartedAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time.UTC()
		rec.CompletedAt = &t
	}
	if verPassed.Valid {
		b := verPassed.Bool
		rec.VerificationPassed = &b
	}
	if err := json.Unmarshal([]byte(paramsJSON), &rec.Parameters); err != nil {
		return remediation.ExecutionRecord{}, fmt.Errorf("unmarshal parameters: %w", err)
	}
	if err := json.Unmarshal([]byte(verJSON), &rec.VerificationSummary); err != nil {
		return remediation.ExecutionRecord{}, fmt.Errorf("unmarshal verification summary: %w", err)
	}
	return rec, nil
}

func remediationStorageError(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", remediation.ErrRemediationStoreError, operation, err)
}
