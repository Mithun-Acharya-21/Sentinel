package remediation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"sentinel/internal/metrics"
	"sentinel/internal/triage"
)

type memTriageStore struct {
	mu    sync.Mutex
	views map[string]triage.TriageView
}

func newMemTriageStore() *memTriageStore {
	return &memTriageStore{views: make(map[string]triage.TriageView)}
}

func (m *memTriageStore) SaveTriageAndApproval(_ context.Context, res triage.TriageResult, app triage.ApprovalRecord) (triage.TriageView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := triage.TriageView{Triage: res, Approval: app}
	m.views[res.IncidentID] = v
	return v, nil
}

func (m *memTriageStore) GetByIncidentID(_ context.Context, tenantID, incidentID string) (triage.TriageView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.views[incidentID]
	if !ok || v.Triage.TenantID != tenantID {
		return triage.TriageView{}, triage.ErrTriageNotFound
	}
	return v, nil
}

func (m *memTriageStore) DecideApproval(_ context.Context, tenantID, incidentID string, decision triage.ApprovalStatus, decidedBy, reason string, now time.Time) (triage.ApprovalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.views[incidentID]
	if !ok || v.Approval.TenantID != tenantID {
		return triage.ApprovalRecord{}, triage.ErrTriageNotFound
	}
	t := now.UTC()
	v.Approval.Status = decision
	v.Approval.DecidedBy = decidedBy
	v.Approval.DecisionReason = reason
	v.Approval.DecidedAt = &t
	m.views[incidentID] = v
	return v.Approval, nil
}

func (m *memTriageStore) PendingApprovalCount(_ context.Context) (int64, error) { return 0, nil }

type memExecutionStore struct {
	mu      sync.Mutex
	records map[string]ExecutionRecord
	seq     int
}

func newMemExecutionStore() *memExecutionStore {
	return &memExecutionStore{records: make(map[string]ExecutionRecord)}
}

func (m *memExecutionStore) ClaimExecution(_ context.Context, req ClaimRequest) (ExecutionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.records[req.ApprovalID]; ok {
		if existing.TenantID != req.TenantID {
			return ExecutionRecord{}, false, ErrExecutionConflict
		}
		if existing.Status == StatusSucceeded {
			return existing, true, nil
		}
		if existing.Status == StatusExecuting && existing.LeaseExpiresAt != nil && req.Now.Before(*existing.LeaseExpiresAt) {
			return ExecutionRecord{}, false, ErrExecutionConflict
		}
		m.seq++
		existing.Status = StatusExecuting
		existing.Attempts++
		existing.ExecutionToken = string(rune('a'+m.seq)) + "-tok"
		exp := req.Now.Add(req.LeaseDuration)
		existing.LeaseExpiresAt = &exp
		m.records[req.ApprovalID] = existing
		return existing, false, nil
	}

	m.seq++
	exp := req.Now.Add(req.LeaseDuration)
	rec := ExecutionRecord{
		ID:             "exec-1",
		TenantID:       req.TenantID,
		IncidentID:     req.IncidentID,
		TriageID:       req.TriageID,
		ApprovalID:     req.ApprovalID,
		ActionType:     req.Action.ActionType,
		ActionVersion:  req.Action.ActionVersion,
		TargetResource: req.Action.TargetResource,
		Parameters:     req.Action.Parameters,
		IdempotencyKey: req.Action.IdempotencyKey,
		ApproverID:     req.ApproverID,
		Status:         StatusExecuting,
		Attempts:       1,
		ExecutionToken: string(rune('a'+m.seq)) + "-tok",
		LeaseExpiresAt: &exp,
		RequestedAt:    req.Action.RequestedAt,
	}
	m.records[req.ApprovalID] = rec
	return rec, false, nil
}

func (m *memExecutionStore) CompleteExecution(_ context.Context, execID, executionToken string, status ExecutionStatus, verification PostActionVerification, failCategory, failMsg string, now time.Time) (ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, rec := range m.records {
		if rec.ID == execID {
			if rec.Status != StatusExecuting || rec.ExecutionToken != executionToken {
				return ExecutionRecord{}, ErrExecutionLeaseLost
			}
			rec.Status = status
			vp := verification.VerificationPassed
			rec.VerificationPassed = &vp
			rec.VerificationSummary = verification
			rec.FailureCategory = failCategory
			rec.FailureMessage = failMsg
			rec.LeaseExpiresAt = nil
			t := now
			rec.CompletedAt = &t
			m.records[k] = rec
			return rec, nil
		}
	}
	return ExecutionRecord{}, ErrExecutionLeaseLost
}

func (m *memExecutionStore) GetByIncidentID(_ context.Context, tenantID, incidentID string) (ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.records {
		if rec.TenantID == tenantID && rec.IncidentID == incidentID {
			return rec, nil
		}
	}
	return ExecutionRecord{}, ErrExecutionNotFound
}

func seedApprovedTriage(t *testing.T, ts *memTriageStore, tenantID, incidentID string, proposed triage.ProposedAction, status triage.ApprovalStatus, expiresAt time.Time) {
	t.Helper()
	now := expiresAt.Add(-1 * time.Hour)
	decidedBy := ""
	var decidedAt *time.Time
	if status == triage.ApprovalApproved || status == triage.ApprovalRejected {
		decidedBy = "human-oncall"
		decidedAt = &now
	}
	_, _ = ts.SaveTriageAndApproval(context.Background(), triage.TriageResult{
		ID:                    "triage-" + incidentID,
		TenantID:              tenantID,
		IncidentID:            incidentID,
		ProposedAction:        proposed,
		RequiresHumanApproval: true,
	}, triage.ApprovalRecord{
		ID:             "app-" + incidentID,
		TriageID:       "triage-" + incidentID,
		TenantID:       tenantID,
		IncidentID:     incidentID,
		Status:         status,
		ProposedAction: proposed,
		DecidedBy:      decidedBy,
		DecidedAt:      decidedAt,
		ExpiresAt:      &expiresAt,
	})
}

func TestAuthorizationAndValidationBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	past := now.Add(-1 * time.Minute)

	ts := newMemTriageStore()
	es := newMemExecutionStore()
	sim := NewSimulator(func() time.Time { return now })
	prom := metrics.NewSentinelMetrics()
	exec := NewExecutor(ts, es, sim, ExecutorConfig{Clock: func() time.Time { return now }, PromMetrics: prom})

	baseReq := func(appID string) ActionRequest {
		return ActionRequest{
			ActionType:     ActionSwitchPaymentRouteToBackup,
			ActionVersion:  ActionVersion,
			TargetResource: "payment-api",
			Parameters:     map[string]string{"target_route": "backup"},
			ApprovalID:     appID,
			IdempotencyKey: "idem-" + appID,
			RequestedAt:    now,
		}
	}

	// 1. Execution without triage/approval
	if _, err := exec.Execute(context.Background(), "tenant-a", "inc-missing", baseReq("app-missing")); !errors.Is(err, triage.ErrTriageNotFound) {
		t.Fatalf("missing triage err = %v, want ErrTriageNotFound", err)
	}

	// 2. Pending approval rejected
	seedApprovedTriage(t, ts, "tenant-a", "inc-pending", triage.ActionRouteFailover, triage.ApprovalPendingReview, future)
	if _, err := exec.Execute(context.Background(), "tenant-a", "inc-pending", baseReq("app-inc-pending")); !errors.Is(err, triage.ErrApprovalRequired) {
		t.Fatalf("pending approval err = %v, want ErrApprovalRequired", err)
	}

	// 3. Rejected approval rejected
	seedApprovedTriage(t, ts, "tenant-a", "inc-rej", triage.ActionRouteFailover, triage.ApprovalRejected, future)
	if _, err := exec.Execute(context.Background(), "tenant-a", "inc-rej", baseReq("app-inc-rej")); !errors.Is(err, triage.ErrApprovalRequired) {
		t.Fatalf("rejected approval err = %v, want ErrApprovalRequired", err)
	}

	// 4. Expired approval rejected
	seedApprovedTriage(t, ts, "tenant-a", "inc-exp", triage.ActionRouteFailover, triage.ApprovalApproved, past)
	if _, err := exec.Execute(context.Background(), "tenant-a", "inc-exp", baseReq("app-inc-exp")); !errors.Is(err, triage.ErrApprovalExpired) {
		t.Fatalf("expired approval err = %v, want ErrApprovalExpired", err)
	}

	// 5. Cross-tenant execution rejected
	seedApprovedTriage(t, ts, "tenant-a", "inc-ok", triage.ActionRouteFailover, triage.ApprovalApproved, future)
	if _, err := exec.Execute(context.Background(), "tenant-b", "inc-ok", baseReq("app-inc-ok")); !errors.Is(err, triage.ErrTriageNotFound) {
		t.Fatalf("cross-tenant err = %v, want ErrTriageNotFound", err)
	}

	// 6. Invalid action type & unsafe parameters rejected
	badAction := baseReq("app-inc-ok")
	badAction.ActionType = "run_shell"
	if _, err := exec.Execute(context.Background(), "tenant-a", "inc-ok", badAction); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("invalid action err = %v, want ErrInvalidAction", err)
	}
	badParam := baseReq("app-inc-ok")
	badParam.Parameters = map[string]string{"cmd": "rm -rf /"}
	if _, err := exec.Execute(context.Background(), "tenant-a", "inc-ok", badParam); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("unsafe param err = %v, want ErrInvalidAction", err)
	}

	// 7. Action differing from approved recommendation rejected
	mismatch := baseReq("app-inc-ok")
	mismatch.ActionType = ActionRestartSimulatedWorker
	mismatch.Parameters = map[string]string{"mode": "graceful"}
	if _, err := exec.Execute(context.Background(), "tenant-a", "inc-ok", mismatch); !errors.Is(err, ErrActionMismatch) {
		t.Fatalf("action mismatch err = %v, want ErrActionMismatch", err)
	}

	// Verify simulator was NEVER mutated by any rejected attempt
	if snap := sim.Snapshot(); snap.ActivePaymentRoute != "primary" || len(snap.AuditEvents) != 0 {
		t.Fatalf("simulator was mutated despite rejections: %#v", snap)
	}
}

func TestSuccessfulExecutionVerificationAndIdempotentRetry(t *testing.T) {
	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)

	ts := newMemTriageStore()
	es := newMemExecutionStore()
	sim := NewSimulator(func() time.Time { return now })
	prom := metrics.NewSentinelMetrics()
	exec := NewExecutor(ts, es, sim, ExecutorConfig{Clock: func() time.Time { return now }, PromMetrics: prom})

	seedApprovedTriage(t, ts, "tenant-a", "inc-1", triage.ActionRouteFailover, triage.ApprovalApproved, future)

	req := ActionRequest{
		ActionType:     ActionSwitchPaymentRouteToBackup,
		ActionVersion:  ActionVersion,
		TargetResource: "payment-api",
		Parameters:     map[string]string{"target_route": "backup"},
		ApprovalID:     "app-inc-1",
		IdempotencyKey: "idem-inc-1",
		RequestedAt:    now,
	}

	rec, err := exec.Execute(context.Background(), "tenant-a", "inc-1", req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if rec.Status != StatusSucceeded || rec.VerificationPassed == nil || !*rec.VerificationPassed || rec.ApproverID != "human-oncall" {
		t.Fatalf("unexpected execution record: %#v", rec)
	}

	// Duplicate delivery must return succeeded record WITHOUT re-applying to simulator
	rec2, err := exec.Execute(context.Background(), "tenant-a", "inc-1", req)
	if err != nil || rec2.Status != StatusSucceeded {
		t.Fatalf("duplicate Execute failed: rec=%#v err=%v", rec2, err)
	}
	if snap := sim.Snapshot(); len(snap.AuditEvents) != 1 || snap.ActivePaymentRoute != "backup" {
		t.Fatalf("simulator audit events = %d (want 1), route = %q", len(snap.AuditEvents), snap.ActivePaymentRoute)
	}
}

type noopBrokenEnv struct {
	snap Snapshot
}

func (n *noopBrokenEnv) Apply(_ context.Context, _ ActionRequest) error { return nil }
func (n *noopBrokenEnv) Snapshot() Snapshot                             { return n.snap }

func TestPreconditionFailureAndPostActionVerificationFailure(t *testing.T) {
	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)

	t.Run("SimulatorPreconditionFailureRecordedAndRetryable", func(t *testing.T) {
		ts := newMemTriageStore()
		es := newMemExecutionStore()
		sim := NewSimulator(func() time.Time { return now })
		st := sim.Snapshot()
		st.BackupProviderHealth = false // precondition failure!
		sim.SetState(st)

		exec := NewExecutor(ts, es, sim, ExecutorConfig{Clock: func() time.Time { return now }})
		seedApprovedTriage(t, ts, "tenant-a", "inc-pre", triage.ActionRouteFailover, triage.ApprovalApproved, future)

		req := ActionRequest{
			ActionType:     ActionSwitchPaymentRouteToBackup,
			ActionVersion:  ActionVersion,
			TargetResource: "payment-api",
			ApprovalID:     "app-inc-pre",
			IdempotencyKey: "idem-pre",
			RequestedAt:    now,
		}
		rec, err := exec.Execute(context.Background(), "tenant-a", "inc-pre", req)
		if err != nil {
			t.Fatalf("Execute returned unexpected err: %v", err)
		}
		if rec.Status != StatusFailed || rec.FailureCategory != "precondition_failed" {
			t.Fatalf("expected failed/precondition_failed, got %#v", rec)
		}

		// Fix backup health and retry -> succeeds on attempt 2!
		st.BackupProviderHealth = true
		sim.SetState(st)
		rec2, err := exec.Execute(context.Background(), "tenant-a", "inc-pre", req)
		if err != nil || rec2.Status != StatusSucceeded || rec2.Attempts != 2 {
			t.Fatalf("retry after precondition fix failed: rec=%#v err=%v", rec2, err)
		}
	})

	t.Run("PostActionVerificationFailureCaughtIndependently", func(t *testing.T) {
		ts := newMemTriageStore()
		es := newMemExecutionStore()
		brokenEnv := &noopBrokenEnv{snap: Snapshot{ActivePaymentRoute: "primary", BackupProviderHealth: true}}

		exec := NewExecutor(ts, es, brokenEnv, ExecutorConfig{Clock: func() time.Time { return now }})
		seedApprovedTriage(t, ts, "tenant-a", "inc-ver", triage.ActionRouteFailover, triage.ApprovalApproved, future)

		req := ActionRequest{
			ActionType:     ActionSwitchPaymentRouteToBackup,
			ActionVersion:  ActionVersion,
			TargetResource: "payment-api",
			ApprovalID:     "app-inc-ver",
			IdempotencyKey: "idem-ver",
			RequestedAt:    now,
		}
		rec, err := exec.Execute(context.Background(), "tenant-a", "inc-ver", req)
		if err != nil {
			t.Fatalf("Execute returned unexpected err: %v", err)
		}
		if rec.Status != StatusVerificationFailed || rec.VerificationPassed == nil || *rec.VerificationPassed {
			t.Fatalf("expected verification_failed, got %#v", rec)
		}
	})
}

func TestConcurrentExecutionAttemptsPreventDuplicates(t *testing.T) {
	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)

	ts := newMemTriageStore()
	es := newMemExecutionStore()
	sim := NewSimulator(func() time.Time { return now })
	exec := NewExecutor(ts, es, sim, ExecutorConfig{Clock: func() time.Time { return now }})
	seedApprovedTriage(t, ts, "tenant-a", "inc-conc", triage.ActionRouteFailover, triage.ApprovalApproved, future)

	req := ActionRequest{
		ActionType:     ActionSwitchPaymentRouteToBackup,
		ActionVersion:  ActionVersion,
		TargetResource: "payment-api",
		ApprovalID:     "app-inc-conc",
		IdempotencyKey: "idem-conc",
		RequestedAt:    now,
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	var succeeded, conflicts int
	var mu sync.Mutex

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec, err := exec.Execute(context.Background(), "tenant-a", "inc-conc", req)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && rec.Status == StatusSucceeded {
				succeeded++
			} else if errors.Is(err, ErrExecutionConflict) {
				conflicts++
			} else {
				t.Errorf("unexpected result: rec=%#v err=%v", rec, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if succeeded < 1 || (succeeded+conflicts) != 8 {
		t.Fatalf("succeeded=%d conflicts=%d, want >=1 succeeded and total 8", succeeded, conflicts)
	}
	if snap := sim.Snapshot(); len(snap.AuditEvents) != 1 {
		t.Fatalf("simulator AuditEvents = %d, want 1 (no duplicate mutations)", len(snap.AuditEvents))
	}
}
