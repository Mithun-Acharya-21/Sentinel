package remediation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"sentinel/internal/triage"
)

const ActionVersion = "1.0.0"

// ActionType enumerates the strictly allowlisted simulated remediation actions.
type ActionType string

const (
	ActionSwitchPaymentRouteToBackup ActionType = "switch_payment_route_to_backup"
	ActionDisableUnhealthyProvider   ActionType = "disable_unhealthy_provider"
	ActionRestartSimulatedWorker     ActionType = "restart_simulated_worker"
)

// ExecutionStatus represents the durable remediation execution state machine.
type ExecutionStatus string

const (
	StatusPending            ExecutionStatus = "pending"
	StatusAuthorized         ExecutionStatus = "authorized"
	StatusExecuting          ExecutionStatus = "executing"
	StatusSucceeded          ExecutionStatus = "succeeded"
	StatusFailed             ExecutionStatus = "failed"
	StatusVerificationFailed ExecutionStatus = "verification_failed"
)

var (
	ErrInvalidAction         = errors.New("invalid remediation action or parameters")
	ErrActionMismatch        = errors.New("action does not match approved triage recommendation")
	ErrExecutionConflict     = errors.New("remediation execution already in progress or completed")
	ErrExecutionLeaseLost    = errors.New("remediation execution lease lost")
	ErrExecutionNotFound     = errors.New("remediation execution not found")
	ErrPreconditionFailed    = errors.New("simulator precondition failed")
	ErrRemediationStoreError = errors.New("remediation storage unavailable")
)

// ActionRequest defines the versioned, validated remediation request contract.
type ActionRequest struct {
	ActionType     ActionType        `json:"action_type"`
	ActionVersion  string            `json:"action_version"`
	TargetResource string            `json:"target_resource"`
	Parameters     map[string]string `json:"parameters,omitempty"`
	ApprovalID     string            `json:"approval_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	RequestedAt    time.Time         `json:"requested_at"`
}

// ExecutionRecord is the durable PostgreSQL audit record of a controlled remediation attempt.
type ExecutionRecord struct {
	ID                  string                 `json:"id"`
	TenantID            string                 `json:"tenant_id"`
	IncidentID          string                 `json:"incident_id"`
	TriageID            string                 `json:"triage_id"`
	ApprovalID          string                 `json:"approval_id"`
	ActionType          ActionType             `json:"action_type"`
	ActionVersion       string                 `json:"action_version"`
	TargetResource      string                 `json:"target_resource"`
	Parameters          map[string]string      `json:"parameters,omitempty"`
	IdempotencyKey      string                 `json:"idempotency_key"`
	ApproverID          string                 `json:"approver_id"`
	Status              ExecutionStatus        `json:"status"`
	Attempts            int                    `json:"attempts"`
	ExecutionToken      string                 `json:"-"`
	LeaseExpiresAt      *time.Time             `json:"lease_expires_at,omitempty"`
	VerificationPassed  *bool                  `json:"verification_passed,omitempty"`
	VerificationSummary PostActionVerification `json:"verification_summary"`
	FailureCategory     string                 `json:"failure_category,omitempty"`
	FailureMessage      string                 `json:"failure_message,omitempty"`
	RequestedAt         time.Time              `json:"requested_at"`
	StartedAt           *time.Time             `json:"started_at,omitempty"`
	CompletedAt         *time.Time             `json:"completed_at,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	UpdatedAt           time.Time              `json:"updated_at"`
}

// ClaimRequest holds validated parameters for atomically claiming an execution in PostgreSQL.
type ClaimRequest struct {
	TenantID      string
	IncidentID    string
	TriageID      string
	ApprovalID    string
	ApproverID    string
	Action        ActionRequest
	LeaseDuration time.Duration
	Now           time.Time
}

// ExecutionStore is the persistence boundary for durable remediation state transitions.
type ExecutionStore interface {
	ClaimExecution(ctx context.Context, req ClaimRequest) (record ExecutionRecord, alreadySucceeded bool, err error)
	CompleteExecution(ctx context.Context, execID, executionToken string, status ExecutionStatus, verification PostActionVerification, failCategory, failMsg string, now time.Time) (ExecutionRecord, error)
	GetByIncidentID(ctx context.Context, tenantID, incidentID string) (ExecutionRecord, error)
}

// IsAllowedActionType reports whether t is in the supported remediation allowlist.
func IsAllowedActionType(t ActionType) bool {
	switch t {
	case ActionSwitchPaymentRouteToBackup, ActionDisableUnhealthyProvider, ActionRestartSimulatedWorker:
		return true
	default:
		return false
	}
}

// ValidateActionRequest strictly validates the action contract, rejecting unknown action types,
// invalid parameters, and unsafe strings (shell, SQL, URLs).
func ValidateActionRequest(req ActionRequest) error {
	if strings.TrimSpace(req.ActionVersion) != ActionVersion {
		return fmt.Errorf("%w: unsupported action_version %q", ErrInvalidAction, req.ActionVersion)
	}
	if !IsAllowedActionType(req.ActionType) {
		return fmt.Errorf("%w: action_type %q is not allowlisted", ErrInvalidAction, req.ActionType)
	}
	if !isSafeIdentifier(req.TargetResource) {
		return fmt.Errorf("%w: invalid target_resource", ErrInvalidAction)
	}
	if strings.TrimSpace(req.ApprovalID) == "" {
		return fmt.Errorf("%w: approval_id is required", ErrInvalidAction)
	}
	if !isSafeIdentifier(req.IdempotencyKey) {
		return fmt.Errorf("%w: invalid idempotency_key", ErrInvalidAction)
	}
	if req.RequestedAt.IsZero() {
		return fmt.Errorf("%w: requested_at is required", ErrInvalidAction)
	}

	for k, v := range req.Parameters {
		if !isSafeIdentifier(k) || !isSafeIdentifier(v) {
			return fmt.Errorf("%w: unsafe parameter %q", ErrInvalidAction, k)
		}
	}

	switch req.ActionType {
	case ActionSwitchPaymentRouteToBackup:
		for k, v := range req.Parameters {
			if k != "target_route" {
				return fmt.Errorf("%w: unsupported parameter %q for %s", ErrInvalidAction, k, req.ActionType)
			}
			if v != "backup" {
				return fmt.Errorf("%w: target_route must be 'backup'", ErrInvalidAction)
			}
		}
	case ActionDisableUnhealthyProvider:
		for k, v := range req.Parameters {
			if k != "provider" {
				return fmt.Errorf("%w: unsupported parameter %q for %s", ErrInvalidAction, k, req.ActionType)
			}
			if v != "primary" && v != "secondary" {
				return fmt.Errorf("%w: provider must be 'primary' or 'secondary'", ErrInvalidAction)
			}
		}
	case ActionRestartSimulatedWorker:
		for k, v := range req.Parameters {
			if k != "mode" {
				return fmt.Errorf("%w: unsupported parameter %q for %s", ErrInvalidAction, k, req.ActionType)
			}
			if v != "graceful" {
				return fmt.Errorf("%w: mode must be 'graceful'", ErrInvalidAction)
			}
		}
	}
	return nil
}

// ActionMatchesApprovedRecommendation enforces that human approval authorizes only the
// allowlisted action corresponding to the approved triage recommendation.
func ActionMatchesApprovedRecommendation(actionType ActionType, proposed triage.ProposedAction) error {
	switch proposed {
	case triage.ActionRouteFailover:
		if actionType == ActionSwitchPaymentRouteToBackup {
			return nil
		}
	case triage.ActionRateLimitCaller:
		if actionType == ActionDisableUnhealthyProvider {
			return nil
		}
	case triage.ActionScaleService, triage.ActionRollbackDeploy:
		if actionType == ActionRestartSimulatedWorker {
			return nil
		}
	}
	return fmt.Errorf("%w: action %q is not authorized by approved recommendation %q", ErrActionMismatch, actionType, proposed)
}

func isSafeIdentifier(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || len(trimmed) > 128 {
		return false
	}
	if strings.Contains(trimmed, "://") || strings.ContainsAny(trimmed, ";|&$`<>\"'\\\n\r\t ") {
		return false
	}
	return true
}
