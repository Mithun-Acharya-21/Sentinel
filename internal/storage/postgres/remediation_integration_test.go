package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"sentinel/internal/evaluation/llmagent"
	"sentinel/internal/incident"
	"sentinel/internal/remediation"
	"sentinel/internal/triage"
)

func TestRemediationRepositoryClaimCompleteLeaseAndIdempotency(t *testing.T) {
	db, incRepo := openIntegrationRepository(t)
	jobRepo := NewJobRepository(db)
	triageRepo := NewTriageRepository(db)
	remRepo := NewRemediationRepository(db)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	input := uniqueInput()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM incidents WHERE tenant_id = $1`, input.TenantID)
	})

	created, _, err := incRepo.Create(ctx, input)
	if err != nil {
		t.Fatalf("Create incident failed: %v", err)
	}
	now := time.Now().UTC()
	job, claimed, err := jobRepo.ClaimNext(ctx, "w-rem", 30*time.Second, now)
	if err != nil || !claimed {
		t.Fatalf("ClaimNext failed: ok=%v err=%v", claimed, err)
	}

	exp := now.Add(1 * time.Hour)
	view, err := triageRepo.SaveTriageAndApproval(ctx, triage.TriageResult{
		TenantID:              created.TenantID,
		IncidentID:            created.ID,
		JobID:                 job.ID,
		ContractVersion:       triage.ContractVersion,
		PromptVersion:         triage.PromptVersion,
		Model:                 "gpt-4o-mini",
		Classification:        triage.ClassDependencyFailure,
		RecommendedSeverity:   incident.SeverityHigh,
		Confidence:            0.9,
		Evidence:              []string{"504 errors"},
		SuspectedRootCause:    "primary route down",
		RecommendedSteps:      []string{"failover"},
		ProposedAction:        triage.ActionRouteFailover,
		RequiresHumanApproval: true,
		Metadata: triage.ProcessingMetadata{
			Model: "gpt-4o-mini", Provider: "openai", PromptVersion: triage.PromptVersion,
			ContractVersion: triage.ContractVersion, Attempts: 1,
			Usage: llmagent.Usage{Available: true},
		},
	}, triage.ApprovalRecord{
		TenantID:       created.TenantID,
		IncidentID:     created.ID,
		Status:         triage.ApprovalPendingReview,
		ProposedAction: triage.ActionRouteFailover,
		ExpiresAt:      &exp,
	})
	if err != nil {
		t.Fatalf("SaveTriageAndApproval failed: %v", err)
	}

	app, err := triageRepo.DecideApproval(ctx, created.TenantID, created.ID, triage.ApprovalApproved, "human-oncall", "ok", now)
	if err != nil {
		t.Fatalf("DecideApproval failed: %v", err)
	}

	claimReq := remediation.ClaimRequest{
		TenantID:      created.TenantID,
		IncidentID:    created.ID,
		TriageID:      view.Triage.ID,
		ApprovalID:    app.ID,
		ApproverID:    app.DecidedBy,
		LeaseDuration: 10 * time.Second,
		Now:           now,
		Action: remediation.ActionRequest{
			ActionType:     remediation.ActionSwitchPaymentRouteToBackup,
			ActionVersion:  remediation.ActionVersion,
			TargetResource: "payment-api",
			Parameters:     map[string]string{"target_route": "backup"},
			ApprovalID:     app.ID,
			IdempotencyKey: "idem-" + app.ID,
			RequestedAt:    now,
		},
	}

	// 1. First claim succeeds
	exec1, already1, err := remRepo.ClaimExecution(ctx, claimReq)
	if err != nil || already1 || exec1.ExecutionToken == "" {
		t.Fatalf("ClaimExecution failed: already=%v err=%v", already1, err)
	}

	// 2. Concurrent claim while lease active -> ErrExecutionConflict
	if _, _, err := remRepo.ClaimExecution(ctx, claimReq); !errors.Is(err, remediation.ErrExecutionConflict) {
		t.Fatalf("concurrent ClaimExecution err = %v, want ErrExecutionConflict", err)
	}

	// 3. Reclaim after lease expiry issues a new execution_token
	claimReq.Now = now.Add(15 * time.Second)
	exec2, already2, err := remRepo.ClaimExecution(ctx, claimReq)
	if err != nil || already2 || exec2.ExecutionToken == exec1.ExecutionToken || exec2.Attempts != 2 {
		t.Fatalf("reclaim failed: exec2=%#v err=%v", exec2, err)
	}

	// 4. Stale execution_token completion rejected with ErrExecutionLeaseLost
	ver := remediation.PostActionVerification{RecommendationValid: true, ApprovalValid: true, ExecutionSucceeded: true, VerificationPassed: true, SafetyPassed: true}
	if _, err := remRepo.CompleteExecution(ctx, exec1.ID, exec1.ExecutionToken, remediation.StatusSucceeded, ver, "", "", claimReq.Now); !errors.Is(err, remediation.ErrExecutionLeaseLost) {
		t.Fatalf("stale CompleteExecution err = %v, want ErrExecutionLeaseLost", err)
	}

	// 5. Active execution_token completes -> succeeded
	done, err := remRepo.CompleteExecution(ctx, exec2.ID, exec2.ExecutionToken, remediation.StatusSucceeded, ver, "", "", claimReq.Now.Add(time.Second))
	if err != nil || done.Status != remediation.StatusSucceeded {
		t.Fatalf("active CompleteExecution failed: done=%#v err=%v", done, err)
	}

	// 6. Duplicate claim after succeeded -> alreadySucceeded=true
	_, already3, err := remRepo.ClaimExecution(ctx, claimReq)
	if err != nil || !already3 {
		t.Fatalf("post-success ClaimExecution already=%v err=%v, want true/nil", already3, err)
	}
}
