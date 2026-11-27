package remediation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"sentinel/internal/metrics"
	"sentinel/internal/triage"
)

const DefaultExecutionLease = 30 * time.Second

// ExecutorConfig configures the controlled remediation execution engine.
type ExecutorConfig struct {
	LeaseDuration time.Duration
	Clock         func() time.Time
	Logger        *slog.Logger
	PromMetrics   *metrics.SentinelMetrics
}

// Executor enforces human approval, allowlist validation, atomic claim fencing,
// simulated execution, and independent post-action verification.
type Executor struct {
	triage triage.Store
	store  ExecutionStore
	env    Environment
	lease  time.Duration
	clock  func() time.Time
	logger *slog.Logger
	prom   *metrics.SentinelMetrics
}

// NewExecutor creates a controlled remediation Executor.
func NewExecutor(triageStore triage.Store, execStore ExecutionStore, env Environment, cfg ExecutorConfig) *Executor {
	lease := cfg.LeaseDuration
	if lease <= 0 {
		lease = DefaultExecutionLease
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Executor{
		triage: triageStore,
		store:  execStore,
		env:    env,
		lease:  lease,
		clock:  clock,
		logger: logger,
		prom:   cfg.PromMetrics,
	}
}

// Execute validates authorization, claims an execution lease, applies the allowlisted
// simulated action outside any DB transaction, and independently verifies the outcome.
func (e *Executor) Execute(ctx context.Context, tenantID, incidentID string, req ActionRequest) (ExecutionRecord, error) {
	if e.prom != nil {
		e.prom.RemediationRequestsTotal.Inc()
	}

	if err := ValidateActionRequest(req); err != nil {
		if e.prom != nil {
			e.prom.RemediationRejectedTotal.Inc("invalid_action")
		}
		return ExecutionRecord{}, err
	}

	// 1. Load persisted triage & approval record scoped to the authenticated tenant.
	view, err := e.triage.GetByIncidentID(ctx, tenantID, incidentID)
	if err != nil {
		if e.prom != nil {
			e.prom.RemediationRejectedTotal.Inc("triage_not_found")
		}
		return ExecutionRecord{}, err
	}

	now := e.clock().UTC()

	// 2. Verify explicit human approval and expiration.
	if err := triage.CanExecuteAction(view.Approval, tenantID, now); err != nil {
		if e.prom != nil {
			e.prom.RemediationRejectedTotal.Inc("approval_invalid")
		}
		return ExecutionRecord{}, err
	}

	// 3. Verify approval reference matches the loaded approval record.
	if req.ApprovalID != view.Approval.ID {
		if e.prom != nil {
			e.prom.RemediationRejectedTotal.Inc("approval_mismatch")
		}
		return ExecutionRecord{}, fmt.Errorf("%w: approval_id mismatch", triage.ErrApprovalRequired)
	}

	// 4. Verify allowlisted action matches the approved recommendation.
	if err := ActionMatchesApprovedRecommendation(req.ActionType, view.Triage.ProposedAction); err != nil {
		if e.prom != nil {
			e.prom.RemediationRejectedTotal.Inc("action_mismatch")
		}
		return ExecutionRecord{}, err
	}

	if e.prom != nil {
		e.prom.RemediationAuthorizedTotal.Inc()
	}

	// 5. Atomically claim execution in PostgreSQL (short transaction, released before simulator execution).
	claimed, alreadySucceeded, err := e.store.ClaimExecution(ctx, ClaimRequest{
		TenantID:      tenantID,
		IncidentID:    incidentID,
		TriageID:      view.Triage.ID,
		ApprovalID:    view.Approval.ID,
		ApproverID:    view.Approval.DecidedBy,
		Action:        req,
		LeaseDuration: e.lease,
		Now:           now,
	})
	if err != nil {
		if e.prom != nil {
			e.prom.RemediationRejectedTotal.Inc("claim_conflict")
		}
		return ExecutionRecord{}, err
	}
	if alreadySucceeded {
		// Idempotent delivery: do not repeat completed side effects.
		return claimed, nil
	}

	// 6. Execute allowlisted simulated action outside any DB transaction.
	execStart := e.clock()
	before := e.env.Snapshot()
	applyErr := e.env.Apply(ctx, req)
	after := e.env.Snapshot()
	duration := e.clock().Sub(execStart)
	if e.prom != nil && duration >= 0 {
		e.prom.RemediationDuration.Observe(duration.Seconds())
	}

	// 7. Independently verify authoritative simulator state.
	ver := VerifyPostAction(before, after, req, view.Approval, applyErr)

	finalStatus := StatusSucceeded
	failCategory := ""
	failMsg := ""
	if applyErr != nil {
		finalStatus = StatusFailed
		failCategory = classifyFailure(applyErr)
		failMsg = applyErr.Error()
		if len(failMsg) > 512 {
			failMsg = failMsg[:512]
		}
		if e.prom != nil {
			e.prom.RemediationFailuresTotal.Inc(failCategory)
		}
	} else if !ver.VerificationPassed {
		finalStatus = StatusVerificationFailed
		failCategory = "post_action_verification_failed"
		failMsg = "simulator state did not satisfy post-action verification checks"
		if e.prom != nil {
			e.prom.RemediationVerificationFailuresTotal.Inc()
		}
	}

	if e.prom != nil {
		e.prom.RemediationExecutionsTotal.Inc(string(req.ActionType), string(finalStatus))
	}

	// 8. Persist outcome fenced by execution_token.
	completed, compErr := e.store.CompleteExecution(ctx, claimed.ID, claimed.ExecutionToken, finalStatus, ver, failCategory, failMsg, e.clock().UTC())
	if compErr != nil {
		return ExecutionRecord{}, compErr
	}

	e.logger.Info("controlled remediation completed",
		slog.String("action_type", string(req.ActionType)),
		slog.String("status", string(finalStatus)),
		slog.Int("attempt", completed.Attempts),
		slog.Bool("verification_passed", ver.VerificationPassed),
	)
	return completed, nil
}

func classifyFailure(err error) string {
	switch {
	case errors.Is(err, ErrPreconditionFailed):
		return "precondition_failed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout_or_canceled"
	default:
		return "execution_error"
	}
}
