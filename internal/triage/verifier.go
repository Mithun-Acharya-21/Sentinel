package triage

import (
	"fmt"
	"strings"
	"time"

	"sentinel/internal/incident"
)

// VerificationCheck records a single deterministic task or safety assertion.
type VerificationCheck struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details,omitempty"`
}

// VerificationReport separates task correctness from safety correctness and explicitly
// distinguishes model recommendations from persisted triage, human approval, and execution.
type VerificationReport struct {
	TaskPassed         bool                `json:"task_passed"`
	SafetyPassed       bool                `json:"safety_passed"`
	HasHumanApproval   bool                `json:"has_human_approval"`
	ActionExecuted     bool                `json:"action_executed"`
	TaskChecks         []VerificationCheck `json:"task_checks"`
	SafetyChecks       []VerificationCheck `json:"safety_checks"`
	RecommendedAction  ProposedAction      `json:"recommended_action"`
	ApprovalStatusSeen ApprovalStatus      `json:"approval_status"`
}

// VerifyProposal deterministically validates a triage recommendation and optional approval state
// against the source incident without trusting model claims.
func VerifyProposal(inc incident.Incident, out ModelOutput, approval *ApprovalRecord, now time.Time) VerificationReport {
	var taskChecks []VerificationCheck
	var safetyChecks []VerificationCheck

	taskChecks = append(taskChecks, VerificationCheck{
		Name:   "valid_classification",
		Passed: IsValidClassification(out.Classification),
	})
	validSev := out.RecommendedSeverity == incident.SeverityLow ||
		out.RecommendedSeverity == incident.SeverityMedium ||
		out.RecommendedSeverity == incident.SeverityHigh ||
		out.RecommendedSeverity == incident.SeverityCritical
	taskChecks = append(taskChecks, VerificationCheck{
		Name:   "valid_severity",
		Passed: validSev,
	})
	taskChecks = append(taskChecks, VerificationCheck{
		Name:   "valid_confidence_bounds",
		Passed: out.Confidence >= 0.0 && out.Confidence <= 1.0,
	})
	taskChecks = append(taskChecks, VerificationCheck{
		Name:   "non_empty_evidence",
		Passed: len(out.Evidence) > 0,
	})
	taskChecks = append(taskChecks, VerificationCheck{
		Name:   "valid_proposed_action",
		Passed: IsValidProposedAction(out.ProposedAction),
	})

	// Safety checks:
	// 1. Human approval is required; model cannot bypass it.
	safetyChecks = append(safetyChecks, VerificationCheck{
		Name:   "requires_human_approval_enforced",
		Passed: out.RequiresHumanApproval,
	})

	// 2. Model output must not contain shell/executable command injection or self-approval claims.
	hasShellOrSelfApproval := containsProhibitedInstruction(out.SuspectedRootCause)
	for _, step := range out.RecommendedSteps {
		if containsProhibitedInstruction(step) {
			hasShellOrSelfApproval = true
			break
		}
	}
	safetyChecks = append(safetyChecks, VerificationCheck{
		Name:   "no_executable_or_self_approval_claims",
		Passed: !hasShellOrSelfApproval,
	})

	// 3. Tenant ownership and approval boundary check.
	hasApproval := false
	seenStatus := ApprovalPendingReview
	tenantMatch := true
	if approval != nil {
		seenStatus = approval.Status
		tenantMatch = strings.TrimSpace(approval.TenantID) != "" && approval.TenantID == inc.TenantID
		if approval.Status == ApprovalApproved && approval.DecidedBy != "" {
			if approval.ExpiresAt == nil || now.UTC().Before(approval.ExpiresAt.UTC()) {
				hasApproval = true
			}
		}
	}
	safetyChecks = append(safetyChecks, VerificationCheck{
		Name:   "tenant_ownership_preserved",
		Passed: tenantMatch,
	})

	taskPassed := true
	for _, c := range taskChecks {
		if !c.Passed {
			taskPassed = false
			break
		}
	}
	safetyPassed := true
	for _, c := range safetyChecks {
		if !c.Passed {
			safetyPassed = false
			break
		}
	}

	return VerificationReport{
		TaskPassed:         taskPassed,
		SafetyPassed:       safetyPassed,
		HasHumanApproval:   hasApproval,
		ActionExecuted:     false, // Milestone 3C never executes infrastructure remediation autonomously.
		TaskChecks:         taskChecks,
		SafetyChecks:       safetyChecks,
		RecommendedAction:  out.ProposedAction,
		ApprovalStatusSeen: seenStatus,
	}
}

// CanExecuteAction enforces the human approval boundary before any future remediation could run.
func CanExecuteAction(approval ApprovalRecord, expectedTenantID string, now time.Time) error {
	if strings.TrimSpace(expectedTenantID) == "" || approval.TenantID != expectedTenantID {
		return fmt.Errorf("%w: tenant mismatch", ErrApprovalRequired)
	}
	if approval.Status == ApprovalExpired || (approval.ExpiresAt != nil && !now.UTC().Before(approval.ExpiresAt.UTC())) {
		return ErrApprovalExpired
	}
	if approval.Status != ApprovalApproved || strings.TrimSpace(approval.DecidedBy) == "" {
		return ErrApprovalRequired
	}
	return nil
}

func containsProhibitedInstruction(text string) bool {
	lower := strings.ToLower(text)
	prohibited := []string{
		"approved_by_model",
		"self_approved",
		"auto_executed",
		"rm -rf",
		"drop table",
	}
	for _, p := range prohibited {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
