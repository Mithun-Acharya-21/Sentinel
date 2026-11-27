package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"sentinel/internal/evaluation/llmagent"
	"sentinel/internal/incident"
)

const (
	ContractVersion = "1.0.0"
	PromptVersion   = "incident-triage-prompt-v1"
)

// Classification enumerates allowed incident triage categories.
type Classification string

const (
	ClassAvailabilityOutage Classification = "availability_outage"
	ClassLatencyDegradation Classification = "latency_degradation"
	ClassErrorSpike         Classification = "error_spike"
	ClassDependencyFailure  Classification = "dependency_failure"
	ClassSecurityAnomaly    Classification = "security_anomaly"
	ClassCapacityExhaustion Classification = "capacity_exhaustion"
	ClassUnknown            Classification = "unknown"
)

// ProposedAction enumerates bounded, non-executable recommendation categories.
// Arbitrary shell commands or unrestricted tool names are rejected.
type ProposedAction string

const (
	ActionNone                ProposedAction = "none"
	ActionRouteFailover       ProposedAction = "route_failover"
	ActionScaleService        ProposedAction = "scale_service"
	ActionRollbackDeploy      ProposedAction = "rollback_deploy"
	ActionRateLimitCaller     ProposedAction = "rate_limit_caller"
	ActionManualInvestigation ProposedAction = "manual_investigation"
)

// ApprovalStatus represents the explicit human approval state machine.
type ApprovalStatus string

const (
	ApprovalPendingReview ApprovalStatus = "pending_review"
	ApprovalApproved      ApprovalStatus = "approved"
	ApprovalRejected      ApprovalStatus = "rejected"
	ApprovalExpired       ApprovalStatus = "expired"
)

var (
	ErrTriageNotFound         = errors.New("triage result not found")
	ErrApprovalAlreadyDecided = errors.New("triage approval has already been decided")
	ErrApprovalExpired        = errors.New("triage approval has expired")
	ErrApprovalRequired       = errors.New("human approval is required before any action can proceed")
	ErrInvalidModelOutput     = errors.New("invalid or malformed triage model output")
	ErrTriageStoreUnavailable = errors.New("triage storage unavailable")
)

// ModelOutput is the strict JSON structure expected from the LLM triage adapter.
// Unknown fields are rejected during decoding.
type ModelOutput struct {
	Classification        Classification    `json:"classification"`
	RecommendedSeverity   incident.Severity `json:"recommended_severity"`
	Confidence            float64           `json:"confidence"`
	Evidence              []string          `json:"evidence"`
	SuspectedRootCause    string            `json:"suspected_root_cause"`
	RecommendedSteps      []string          `json:"recommended_steps"`
	ProposedAction        ProposedAction    `json:"proposed_action"`
	RequiresHumanApproval bool              `json:"requires_human_approval"`
}

// ProcessingMetadata captures model, prompt, and provider telemetry without secrets.
type ProcessingMetadata struct {
	Model             string         `json:"model"`
	Provider          string         `json:"provider"`
	PromptVersion     string         `json:"prompt_version"`
	ContractVersion   string         `json:"contract_version"`
	Attempts          int            `json:"attempts"`
	ProviderLatencyMs int64          `json:"provider_latency_ms"`
	Usage             llmagent.Usage `json:"usage"`
}

// TriageResult is the durable, tenant-scoped triage recommendation persisted in PostgreSQL.
type TriageResult struct {
	ID                    string             `json:"id"`
	TenantID              string             `json:"tenant_id"`
	IncidentID            string             `json:"incident_id"`
	JobID                 string             `json:"job_id"`
	ContractVersion       string             `json:"contract_version"`
	PromptVersion         string             `json:"prompt_version"`
	Model                 string             `json:"model"`
	Classification        Classification     `json:"classification"`
	RecommendedSeverity   incident.Severity  `json:"recommended_severity"`
	Confidence            float64            `json:"confidence"`
	Evidence              []string           `json:"evidence"`
	SuspectedRootCause    string             `json:"suspected_root_cause"`
	RecommendedSteps      []string           `json:"recommended_steps"`
	ProposedAction        ProposedAction     `json:"proposed_action"`
	RequiresHumanApproval bool               `json:"requires_human_approval"`
	Metadata              ProcessingMetadata `json:"processing_metadata"`
	CreatedAt             time.Time          `json:"created_at"`
}

// ApprovalRecord tracks the human approval state machine and audit trail for a triage result.
type ApprovalRecord struct {
	ID             string         `json:"id"`
	TriageID       string         `json:"triage_id"`
	TenantID       string         `json:"tenant_id"`
	IncidentID     string         `json:"incident_id"`
	Status         ApprovalStatus `json:"status"`
	ProposedAction ProposedAction `json:"proposed_action"`
	DecidedBy      string         `json:"decided_by,omitempty"`
	DecisionReason string         `json:"decision_reason,omitempty"`
	DecidedAt      *time.Time     `json:"decided_at,omitempty"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// TriageView combines the persisted triage recommendation and its human approval state.
type TriageView struct {
	Triage   TriageResult   `json:"triage"`
	Approval ApprovalRecord `json:"approval"`
}

// Store is the tenant-scoped persistence boundary for triage results and human approvals.
type Store interface {
	SaveTriageAndApproval(ctx context.Context, result TriageResult, approval ApprovalRecord) (TriageView, error)
	GetByIncidentID(ctx context.Context, tenantID, incidentID string) (TriageView, error)
	DecideApproval(ctx context.Context, tenantID, incidentID string, decision ApprovalStatus, decidedBy, reason string, now time.Time) (ApprovalRecord, error)
	PendingApprovalCount(ctx context.Context) (int64, error)
}

// IsValidClassification reports whether c is in the allowed classification enum.
func IsValidClassification(c Classification) bool {
	switch c {
	case ClassAvailabilityOutage, ClassLatencyDegradation, ClassErrorSpike,
		ClassDependencyFailure, ClassSecurityAnomaly, ClassCapacityExhaustion, ClassUnknown:
		return true
	default:
		return false
	}
}

// IsValidProposedAction reports whether a is in the allowed proposed-action enum.
func IsValidProposedAction(a ProposedAction) bool {
	switch a {
	case ActionNone, ActionRouteFailover, ActionScaleService,
		ActionRollbackDeploy, ActionRateLimitCaller, ActionManualInvestigation:
		return true
	default:
		return false
	}
}

// ParseAndValidateModelOutput strictly decodes raw JSON from the LLM and validates all enums,
// confidence bounds, and required evidence fields.
func ParseAndValidateModelOutput(raw string) (ModelOutput, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ModelOutput{}, fmt.Errorf("%w: empty model response", ErrInvalidModelOutput)
	}

	var out ModelOutput
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return ModelOutput{}, fmt.Errorf("%w: %v", ErrInvalidModelOutput, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ModelOutput{}, fmt.Errorf("%w: trailing content after JSON object", ErrInvalidModelOutput)
	}

	if !IsValidClassification(out.Classification) {
		return ModelOutput{}, fmt.Errorf("%w: invalid classification %q", ErrInvalidModelOutput, out.Classification)
	}
	switch out.RecommendedSeverity {
	case incident.SeverityLow, incident.SeverityMedium, incident.SeverityHigh, incident.SeverityCritical:
	default:
		return ModelOutput{}, fmt.Errorf("%w: invalid recommended_severity %q", ErrInvalidModelOutput, out.RecommendedSeverity)
	}
	if math.IsNaN(out.Confidence) || math.IsInf(out.Confidence, 0) || out.Confidence < 0.0 || out.Confidence > 1.0 {
		return ModelOutput{}, fmt.Errorf("%w: confidence must be between 0.0 and 1.0", ErrInvalidModelOutput)
	}
	if len(out.Evidence) == 0 {
		return ModelOutput{}, fmt.Errorf("%w: evidence must contain at least one item", ErrInvalidModelOutput)
	}
	for i, ev := range out.Evidence {
		cleaned := strings.TrimSpace(ev)
		if cleaned == "" {
			return ModelOutput{}, fmt.Errorf("%w: evidence[%d] cannot be blank", ErrInvalidModelOutput, i)
		}
		out.Evidence[i] = cleaned
	}
	out.SuspectedRootCause = strings.TrimSpace(out.SuspectedRootCause)
	if out.SuspectedRootCause == "" {
		return ModelOutput{}, fmt.Errorf("%w: suspected_root_cause is required", ErrInvalidModelOutput)
	}
	if len(out.RecommendedSteps) == 0 {
		return ModelOutput{}, fmt.Errorf("%w: recommended_steps must contain at least one item", ErrInvalidModelOutput)
	}
	for i, step := range out.RecommendedSteps {
		cleaned := strings.TrimSpace(step)
		if cleaned == "" {
			return ModelOutput{}, fmt.Errorf("%w: recommended_steps[%d] cannot be blank", ErrInvalidModelOutput, i)
		}
		out.RecommendedSteps[i] = cleaned
	}
	if !IsValidProposedAction(out.ProposedAction) {
		return ModelOutput{}, fmt.Errorf("%w: invalid proposed_action %q", ErrInvalidModelOutput, out.ProposedAction)
	}

	// Server-enforced policy: actionable recommendations always require human approval
	// regardless of what the model emitted.
	if out.ProposedAction != ActionNone && out.ProposedAction != ActionManualInvestigation {
		out.RequiresHumanApproval = true
	} else {
		out.RequiresHumanApproval = true
	}

	return out, nil
}
