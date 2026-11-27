package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sentinel/internal/incident"
	"sentinel/internal/metrics"
	"sentinel/internal/triage"
)

// TriageRepository persists structured LLM triage results and human approval decisions in PostgreSQL.
type TriageRepository struct {
	db   *sql.DB
	prom *metrics.SentinelMetrics
}

// NewTriageRepository creates a TriageRepository over an existing database pool.
func NewTriageRepository(db *sql.DB) *TriageRepository {
	return &TriageRepository{db: db}
}

// SetMetrics attaches Prometheus-compatible DB operation metrics to this repository.
func (r *TriageRepository) SetMetrics(m *metrics.SentinelMetrics) {
	r.prom = m
}

var _ triage.Store = (*TriageRepository)(nil)

const triageResultColumns = `id::text, tenant_id, incident_id::text, job_id::text, contract_version, prompt_version, model, classification, recommended_severity, confidence, evidence::text, suspected_root_cause, recommended_steps::text, proposed_action, requires_human_approval, processing_metadata::text, created_at`
const approvalColumns = `id::text, triage_id::text, tenant_id, incident_id::text, status, proposed_action, COALESCE(decided_by, ''), decision_reason, decided_at, expires_at, created_at, updated_at`

// SaveTriageAndApproval atomically persists a triage result and its initial pending_review approval.
// If a triage result already exists for the incident, it returns the existing view idempotently.
func (r *TriageRepository) SaveTriageAndApproval(ctx context.Context, result triage.TriageResult, approval triage.ApprovalRecord) (triage.TriageView, error) {
	start := time.Now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.prom.ObserveDBOperation("triage_save", time.Since(start), err)
		return triage.TriageView{}, triageStorageError("begin triage tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	evidenceJSON, err := json.Marshal(result.Evidence)
	if err != nil {
		return triage.TriageView{}, fmt.Errorf("marshal evidence: %w", err)
	}
	stepsJSON, err := json.Marshal(result.RecommendedSteps)
	if err != nil {
		return triage.TriageView{}, fmt.Errorf("marshal recommended_steps: %w", err)
	}
	metaJSON, err := json.Marshal(result.Metadata)
	if err != nil {
		return triage.TriageView{}, fmt.Errorf("marshal processing_metadata: %w", err)
	}

	const insertTriage = `
INSERT INTO incident_triage_results (
	tenant_id, incident_id, job_id, contract_version, prompt_version, model,
	classification, recommended_severity, confidence, evidence, suspected_root_cause,
	recommended_steps, proposed_action, requires_human_approval, processing_metadata
) VALUES (
	$1, $2::uuid, $3::uuid, $4, $5, $6,
	$7, $8, $9, $10::jsonb, $11,
	$12::jsonb, $13, $14, $15::jsonb
)
ON CONFLICT (incident_id) DO NOTHING
RETURNING ` + triageResultColumns

	savedResult, err := scanTriageResult(tx.QueryRowContext(ctx, insertTriage,
		result.TenantID,
		result.IncidentID,
		result.JobID,
		result.ContractVersion,
		result.PromptVersion,
		result.Model,
		string(result.Classification),
		string(result.RecommendedSeverity),
		result.Confidence,
		string(evidenceJSON),
		result.SuspectedRootCause,
		string(stepsJSON),
		string(result.ProposedAction),
		result.RequiresHumanApproval,
		string(metaJSON),
	))
	if errors.Is(err, sql.ErrNoRows) {
		existing, getErr := getTriageViewTx(ctx, tx, result.TenantID, result.IncidentID)
		if getErr != nil {
			r.prom.ObserveDBOperation("triage_save", time.Since(start), getErr)
			return triage.TriageView{}, getErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			r.prom.ObserveDBOperation("triage_save", time.Since(start), commitErr)
			return triage.TriageView{}, triageStorageError("commit existing triage", commitErr)
		}
		r.prom.ObserveDBOperation("triage_save", time.Since(start), nil)
		return existing, nil
	}
	if err != nil {
		r.prom.ObserveDBOperation("triage_save", time.Since(start), err)
		return triage.TriageView{}, triageStorageError("insert triage result", err)
	}

	var expiresAt sql.NullTime
	if approval.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: approval.ExpiresAt.UTC(), Valid: true}
	}

	const insertApproval = `
INSERT INTO incident_triage_approvals (
	triage_id, tenant_id, incident_id, status, proposed_action, expires_at
) VALUES (
	$1::uuid, $2, $3::uuid, $4, $5, $6
)
RETURNING ` + approvalColumns

	savedApproval, err := scanApprovalRecord(tx.QueryRowContext(ctx, insertApproval,
		savedResult.ID,
		savedResult.TenantID,
		savedResult.IncidentID,
		string(triage.ApprovalPendingReview),
		string(savedResult.ProposedAction),
		expiresAt,
	))
	if err != nil {
		r.prom.ObserveDBOperation("triage_save", time.Since(start), err)
		return triage.TriageView{}, triageStorageError("insert triage approval", err)
	}

	if commitErr := tx.Commit(); commitErr != nil {
		r.prom.ObserveDBOperation("triage_save", time.Since(start), commitErr)
		return triage.TriageView{}, triageStorageError("commit triage and approval", commitErr)
	}
	r.prom.ObserveDBOperation("triage_save", time.Since(start), nil)
	return triage.TriageView{Triage: savedResult, Approval: savedApproval}, nil
}

// GetByIncidentID retrieves a tenant-scoped triage result and its approval state.
func (r *TriageRepository) GetByIncidentID(ctx context.Context, tenantID, incidentID string) (triage.TriageView, error) {
	if strings.TrimSpace(tenantID) == "" {
		return triage.TriageView{}, triage.ErrTriageNotFound
	}
	start := time.Now()
	const queryTriage = `SELECT ` + triageResultColumns + ` FROM incident_triage_results WHERE incident_id = $1::uuid AND tenant_id = $2`
	res, err := scanTriageResult(r.db.QueryRowContext(ctx, queryTriage, incidentID, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		r.prom.ObserveDBOperation("triage_get", time.Since(start), nil)
		return triage.TriageView{}, triage.ErrTriageNotFound
	}
	if err != nil {
		r.prom.ObserveDBOperation("triage_get", time.Since(start), err)
		return triage.TriageView{}, triageStorageError("get triage result", err)
	}

	const queryApproval = `SELECT ` + approvalColumns + ` FROM incident_triage_approvals WHERE triage_id = $1::uuid AND tenant_id = $2`
	app, err := scanApprovalRecord(r.db.QueryRowContext(ctx, queryApproval, res.ID, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		r.prom.ObserveDBOperation("triage_get", time.Since(start), nil)
		return triage.TriageView{}, triage.ErrTriageNotFound
	}
	r.prom.ObserveDBOperation("triage_get", time.Since(start), err)
	if err != nil {
		return triage.TriageView{}, triageStorageError("get triage approval", err)
	}
	return triage.TriageView{Triage: res, Approval: app}, nil
}

// DecideApproval transitions a pending_review approval to approved or rejected atomically
// for the authenticated human principal within the owning tenant.
func (r *TriageRepository) DecideApproval(ctx context.Context, tenantID, incidentID string, decision triage.ApprovalStatus, decidedBy, reason string, now time.Time) (triage.ApprovalRecord, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(decidedBy) == "" {
		return triage.ApprovalRecord{}, triage.ErrTriageNotFound
	}
	if decision != triage.ApprovalApproved && decision != triage.ApprovalRejected {
		return triage.ApprovalRecord{}, errors.New("decision must be approved or rejected")
	}

	start := time.Now()
	nowUTC := now.UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.prom.ObserveDBOperation("triage_decide", time.Since(start), err)
		return triage.ApprovalRecord{}, triageStorageError("begin approval tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	const selectForUpdate = `SELECT ` + approvalColumns + ` FROM incident_triage_approvals WHERE incident_id = $1::uuid AND tenant_id = $2 FOR UPDATE`
	current, err := scanApprovalRecord(tx.QueryRowContext(ctx, selectForUpdate, incidentID, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		r.prom.ObserveDBOperation("triage_decide", time.Since(start), nil)
		return triage.ApprovalRecord{}, triage.ErrTriageNotFound
	}
	if err != nil {
		r.prom.ObserveDBOperation("triage_decide", time.Since(start), err)
		return triage.ApprovalRecord{}, triageStorageError("lock approval row", err)
	}

	if current.Status != triage.ApprovalPendingReview {
		r.prom.ObserveDBOperation("triage_decide", time.Since(start), nil)
		if current.Status == triage.ApprovalExpired {
			return triage.ApprovalRecord{}, triage.ErrApprovalExpired
		}
		return triage.ApprovalRecord{}, triage.ErrApprovalAlreadyDecided
	}

	if current.ExpiresAt != nil && !nowUTC.Before(current.ExpiresAt.UTC()) {
		const expireSQL = `UPDATE incident_triage_approvals SET status = 'expired', updated_at = $3 WHERE id = $1::uuid AND tenant_id = $2`
		if _, expErr := tx.ExecContext(ctx, expireSQL, current.ID, tenantID, nowUTC); expErr != nil {
			r.prom.ObserveDBOperation("triage_decide", time.Since(start), expErr)
			return triage.ApprovalRecord{}, triageStorageError("expire approval", expErr)
		}
		_ = tx.Commit()
		r.prom.ObserveDBOperation("triage_decide", time.Since(start), nil)
		return triage.ApprovalRecord{}, triage.ErrApprovalExpired
	}

	const updateSQL = `
UPDATE incident_triage_approvals
SET status = $3,
	decided_by = $4,
	decision_reason = $5,
	decided_at = $6,
	updated_at = $6
WHERE id = $1::uuid AND tenant_id = $2
RETURNING ` + approvalColumns

	updated, err := scanApprovalRecord(tx.QueryRowContext(ctx, updateSQL,
		current.ID,
		tenantID,
		string(decision),
		strings.TrimSpace(decidedBy),
		strings.TrimSpace(reason),
		nowUTC,
	))
	if err != nil {
		r.prom.ObserveDBOperation("triage_decide", time.Since(start), err)
		return triage.ApprovalRecord{}, triageStorageError("update approval decision", err)
	}

	if commitErr := tx.Commit(); commitErr != nil {
		r.prom.ObserveDBOperation("triage_decide", time.Since(start), commitErr)
		return triage.ApprovalRecord{}, triageStorageError("commit approval decision", commitErr)
	}
	r.prom.ObserveDBOperation("triage_decide", time.Since(start), nil)
	return updated, nil
}

// PendingApprovalCount returns the total number of triage recommendations awaiting human review.
func (r *TriageRepository) PendingApprovalCount(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM incident_triage_approvals WHERE status = 'pending_review'`).Scan(&count)
	if err != nil {
		return 0, triageStorageError("count pending approvals", err)
	}
	return count, nil
}

func getTriageViewTx(ctx context.Context, tx *sql.Tx, tenantID, incidentID string) (triage.TriageView, error) {
	const queryTriage = `SELECT ` + triageResultColumns + ` FROM incident_triage_results WHERE incident_id = $1::uuid AND tenant_id = $2`
	res, err := scanTriageResult(tx.QueryRowContext(ctx, queryTriage, incidentID, tenantID))
	if err != nil {
		return triage.TriageView{}, triageStorageError("select existing triage", err)
	}
	const queryApproval = `SELECT ` + approvalColumns + ` FROM incident_triage_approvals WHERE triage_id = $1::uuid AND tenant_id = $2`
	app, err := scanApprovalRecord(tx.QueryRowContext(ctx, queryApproval, res.ID, tenantID))
	if err != nil {
		return triage.TriageView{}, triageStorageError("select existing approval", err)
	}
	return triage.TriageView{Triage: res, Approval: app}, nil
}

func scanTriageResult(row rowScanner) (triage.TriageResult, error) {
	var out triage.TriageResult
	var classification, severity, proposedAction string
	var evidenceJSON, stepsJSON, metaJSON string
	err := row.Scan(
		&out.ID,
		&out.TenantID,
		&out.IncidentID,
		&out.JobID,
		&out.ContractVersion,
		&out.PromptVersion,
		&out.Model,
		&classification,
		&severity,
		&out.Confidence,
		&evidenceJSON,
		&out.SuspectedRootCause,
		&stepsJSON,
		&proposedAction,
		&out.RequiresHumanApproval,
		&metaJSON,
		&out.CreatedAt,
	)
	if err != nil {
		return triage.TriageResult{}, err
	}
	out.Classification = triage.Classification(classification)
	out.RecommendedSeverity = incident.Severity(severity)
	out.ProposedAction = triage.ProposedAction(proposedAction)
	out.CreatedAt = out.CreatedAt.UTC()
	if err := json.Unmarshal([]byte(evidenceJSON), &out.Evidence); err != nil {
		return triage.TriageResult{}, fmt.Errorf("unmarshal evidence: %w", err)
	}
	if err := json.Unmarshal([]byte(stepsJSON), &out.RecommendedSteps); err != nil {
		return triage.TriageResult{}, fmt.Errorf("unmarshal recommended_steps: %w", err)
	}
	if err := json.Unmarshal([]byte(metaJSON), &out.Metadata); err != nil {
		return triage.TriageResult{}, fmt.Errorf("unmarshal processing_metadata: %w", err)
	}
	return out, nil
}

func scanApprovalRecord(row rowScanner) (triage.ApprovalRecord, error) {
	var out triage.ApprovalRecord
	var status, proposedAction string
	var decidedAt, expiresAt sql.NullTime
	err := row.Scan(
		&out.ID,
		&out.TriageID,
		&out.TenantID,
		&out.IncidentID,
		&status,
		&proposedAction,
		&out.DecidedBy,
		&out.DecisionReason,
		&decidedAt,
		&expiresAt,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return triage.ApprovalRecord{}, err
	}
	out.Status = triage.ApprovalStatus(status)
	out.ProposedAction = triage.ProposedAction(proposedAction)
	out.CreatedAt = out.CreatedAt.UTC()
	out.UpdatedAt = out.UpdatedAt.UTC()
	if decidedAt.Valid {
		t := decidedAt.Time.UTC()
		out.DecidedAt = &t
	}
	if expiresAt.Valid {
		t := expiresAt.Time.UTC()
		out.ExpiresAt = &t
	}
	return out, nil
}

func triageStorageError(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", triage.ErrTriageStoreUnavailable, operation, err)
}
