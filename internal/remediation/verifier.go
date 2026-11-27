package remediation

import "sentinel/internal/triage"

// VerificationCheck records a single deterministic post-action check.
type VerificationCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

// PostActionVerification separates recommendation, approval, execution, verification,
// and safety outcomes based on authoritative simulator state.
type PostActionVerification struct {
	RecommendationValid bool                `json:"recommendation_valid"`
	ApprovalValid       bool                `json:"approval_valid"`
	ExecutionSucceeded  bool                `json:"execution_succeeded"`
	VerificationPassed  bool                `json:"verification_passed"`
	SafetyPassed        bool                `json:"safety_passed"`
	Checks              []VerificationCheck `json:"checks"`
}

// VerifyPostAction independently inspects the simulated infrastructure snapshot after execution
// without trusting executor success messages or LLM claims.
func VerifyPostAction(before, after Snapshot, req ActionRequest, approval triage.ApprovalRecord, execErr error) PostActionVerification {
	var checks []VerificationCheck

	execOK := execErr == nil
	checks = append(checks, VerificationCheck{Name: "execution_returned_nil", Passed: execOK})

	stateVerified := false
	switch req.ActionType {
	case ActionSwitchPaymentRouteToBackup:
		routeSwitched := after.ActivePaymentRoute == "backup"
		backupHealthy := after.BackupProviderHealth
		checks = append(checks,
			VerificationCheck{Name: "active_route_is_backup", Passed: routeSwitched},
			VerificationCheck{Name: "backup_provider_healthy", Passed: backupHealthy},
		)
		stateVerified = routeSwitched && backupHealthy

	case ActionDisableUnhealthyProvider:
		disabled := !after.PrimaryProviderEnabled
		checks = append(checks, VerificationCheck{Name: "primary_provider_disabled", Passed: disabled})
		stateVerified = disabled

	case ActionRestartSimulatedWorker:
		workerHealthy := after.SimulatedWorkerHealthy
		restarted := after.WorkerRestartCount > before.WorkerRestartCount
		checks = append(checks,
			VerificationCheck{Name: "simulated_worker_healthy", Passed: workerHealthy},
			VerificationCheck{Name: "worker_restart_count_incremented", Passed: restarted},
		)
		stateVerified = workerHealthy && restarted
	}

	approvalValid := approval.Status == triage.ApprovalApproved && approval.DecidedBy != ""
	checks = append(checks, VerificationCheck{Name: "human_approval_verified", Passed: approvalValid})

	safetyPassed := approvalValid && IsAllowedActionType(req.ActionType)
	verificationPassed := execOK && stateVerified && safetyPassed

	return PostActionVerification{
		RecommendationValid: true,
		ApprovalValid:       approvalValid,
		ExecutionSucceeded:  execOK,
		VerificationPassed:  verificationPassed,
		SafetyPassed:        safetyPassed,
		Checks:              checks,
	}
}
