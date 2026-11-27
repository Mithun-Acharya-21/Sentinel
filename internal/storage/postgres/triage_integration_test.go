package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"sentinel/internal/evaluation/llmagent"
	"sentinel/internal/incident"
	"sentinel/internal/triage"
)

func TestTriageRepositorySaveGetAndApprovalLifecycle(t *testing.T) {
	db, incRepo := openIntegrationRepository(t)
	jobRepo := NewJobRepository(db)
	triageRepo := NewTriageRepository(db)

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
	job, claimed, err := jobRepo.ClaimNext(ctx, "worker-triage-test", 30*time.Second, now)
	if err != nil || !claimed {
		t.Fatalf("ClaimNext failed: ok=%v err=%v", claimed, err)
	}

	exp := now.Add(1 * time.Hour)
	res := triage.TriageResult{
		TenantID:              created.TenantID,
		IncidentID:            created.ID,
		JobID:                 job.ID,
		ContractVersion:       triage.ContractVersion,
		PromptVersion:         triage.PromptVersion,
		Model:                 "gpt-4o-mini",
		Classification:        triage.ClassDependencyFailure,
		RecommendedSeverity:   incident.SeverityHigh,
		Confidence:            0.9,
		Evidence:              []string{"504 spike"},
		SuspectedRootCause:    "primary route down",
		RecommendedSteps:      []string{"verify backup"},
		ProposedAction:        triage.ActionRouteFailover,
		RequiresHumanApproval: true,
		Metadata: triage.ProcessingMetadata{
			Model:             "gpt-4o-mini",
			Provider:          "openai",
			PromptVersion:     triage.PromptVersion,
			ContractVersion:   triage.ContractVersion,
			Attempts:          1,
			ProviderLatencyMs: 250,
			Usage:             llmagent.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, Available: true},
		},
	}
	app := triage.ApprovalRecord{
		TenantID:       created.TenantID,
		IncidentID:     created.ID,
		Status:         triage.ApprovalPendingReview,
		ProposedAction: triage.ActionRouteFailover,
		ExpiresAt:      &exp,
	}

	view, err := triageRepo.SaveTriageAndApproval(ctx, res, app)
	if err != nil {
		t.Fatalf("SaveTriageAndApproval (apply migrations/005_create_incident_triage.up.sql first): %v", err)
	}
	if view.Approval.Status != triage.ApprovalPendingReview {
		t.Fatalf("approval status = %q, want pending_review", view.Approval.Status)
	}

	// Idempotent duplicate save returns existing view
	view2, err := triageRepo.SaveTriageAndApproval(ctx, res, app)
	if err != nil || view2.Triage.ID != view.Triage.ID {
		t.Fatalf("duplicate SaveTriageAndApproval failed: view2=%#v err=%v", view2, err)
	}

	// Cross-tenant get returns ErrTriageNotFound
	if _, err := triageRepo.GetByIncidentID(ctx, input.TenantID+"-other", created.ID); !errors.Is(err, triage.ErrTriageNotFound) {
		t.Fatalf("cross-tenant GetByIncidentID err = %v, want ErrTriageNotFound", err)
	}

	// Cross-tenant approval returns ErrTriageNotFound
	if _, err := triageRepo.DecideApproval(ctx, input.TenantID+"-other", created.ID, triage.ApprovalApproved, "attacker", "hijack", now); !errors.Is(err, triage.ErrTriageNotFound) {
		t.Fatalf("cross-tenant DecideApproval err = %v, want ErrTriageNotFound", err)
	}

	// Approve by owning tenant
	decided, err := triageRepo.DecideApproval(ctx, created.TenantID, created.ID, triage.ApprovalApproved, "human-oncall", "verified", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("DecideApproval failed: %v", err)
	}
	if decided.Status != triage.ApprovalApproved || decided.DecidedBy != "human-oncall" {
		t.Fatalf("unexpected decided record: %#v", decided)
	}

	// Duplicate decision rejected with ErrApprovalAlreadyDecided
	if _, err := triageRepo.DecideApproval(ctx, created.TenantID, created.ID, triage.ApprovalRejected, "human-oncall", "undo", now.Add(2*time.Minute)); !errors.Is(err, triage.ErrApprovalAlreadyDecided) {
		t.Fatalf("duplicate DecideApproval err = %v, want ErrApprovalAlreadyDecided", err)
	}
}
