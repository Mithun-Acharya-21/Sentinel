package triage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sentinel/internal/evaluation/llmagent"
	"sentinel/internal/incident"
	"sentinel/internal/metrics"
	"sentinel/internal/worker"
)

type stubIncidentRepo struct {
	inc incident.Incident
	err error
}

func (s *stubIncidentRepo) Create(_ context.Context, _ incident.CreateInput) (incident.Incident, bool, error) {
	return incident.Incident{}, false, nil
}

func (s *stubIncidentRepo) Get(_ context.Context, tenantID, id string) (incident.Incident, error) {
	if s.err != nil {
		return incident.Incident{}, s.err
	}
	if s.inc.TenantID != tenantID || s.inc.ID != id {
		return incident.Incident{}, incident.ErrNotFound
	}
	return s.inc, nil
}

type memTriageStore struct {
	mu    sync.Mutex
	views map[string]TriageView
	err   error
}

func newMemTriageStore() *memTriageStore {
	return &memTriageStore{views: make(map[string]TriageView)}
}

func (m *memTriageStore) SaveTriageAndApproval(_ context.Context, result TriageResult, approval ApprovalRecord) (TriageView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return TriageView{}, m.err
	}
	if existing, ok := m.views[result.IncidentID]; ok {
		return existing, nil
	}
	result.ID = "triage-1"
	approval.ID = "approval-1"
	approval.TriageID = result.ID
	view := TriageView{Triage: result, Approval: approval}
	m.views[result.IncidentID] = view
	return view, nil
}

func (m *memTriageStore) GetByIncidentID(_ context.Context, tenantID, incidentID string) (TriageView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return TriageView{}, m.err
	}
	v, ok := m.views[incidentID]
	if !ok || v.Triage.TenantID != tenantID {
		return TriageView{}, ErrTriageNotFound
	}
	return v, nil
}

func (m *memTriageStore) DecideApproval(_ context.Context, tenantID, incidentID string, decision ApprovalStatus, decidedBy, reason string, now time.Time) (ApprovalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.views[incidentID]
	if !ok || v.Approval.TenantID != tenantID {
		return ApprovalRecord{}, ErrTriageNotFound
	}
	if v.Approval.Status != ApprovalPendingReview {
		return ApprovalRecord{}, ErrApprovalAlreadyDecided
	}
	if v.Approval.ExpiresAt != nil && !now.UTC().Before(v.Approval.ExpiresAt.UTC()) {
		v.Approval.Status = ApprovalExpired
		m.views[incidentID] = v
		return ApprovalRecord{}, ErrApprovalExpired
	}
	t := now.UTC()
	v.Approval.Status = decision
	v.Approval.DecidedBy = decidedBy
	v.Approval.DecisionReason = reason
	v.Approval.DecidedAt = &t
	v.Approval.UpdatedAt = t
	m.views[incidentID] = v
	return v.Approval, nil
}

func (m *memTriageStore) PendingApprovalCount(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, v := range m.views {
		if v.Approval.Status == ApprovalPendingReview {
			n++
		}
	}
	return n, nil
}

type fakeLLMClient struct {
	fn    func(ctx context.Context, req llmagent.CompletionRequest) (llmagent.CompletionResponse, error)
	calls atomic.Int32
}

func (f *fakeLLMClient) Complete(ctx context.Context, req llmagent.CompletionRequest) (llmagent.CompletionResponse, error) {
	f.calls.Add(1)
	return f.fn(ctx, req)
}

func sampleIncident() incident.Incident {
	return incident.Incident{
		ID:            "4b1dd0c8-72aa-4a57-96ac-c5b46a5582ea",
		TenantID:      "tenant-a",
		Source:        "synthetic",
		SourceEventID: "evt-1",
		Service:       "payment-api",
		Title:         "Primary payment gateway timeout",
		Description:   "504 errors on /charge for 5 minutes",
		Severity:      incident.SeverityHigh,
		OccurredAt:    time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC),
	}
}

func validTriageJSON() string {
	return `{
		"classification": "dependency_failure",
		"recommended_severity": "high",
		"confidence": 0.91,
		"evidence": ["504 errors on /charge for 5 minutes", "service=payment-api"],
		"suspected_root_cause": "Upstream payment gateway primary route is unresponsive",
		"recommended_steps": ["Verify backup payment route health", "Request human approval for route failover"],
		"proposed_action": "route_failover",
		"requires_human_approval": false
	}`
}

func TestTriageProcessorValidOutputIdempotencyAndApprovalBoundary(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	inc := sampleIncident()
	incRepo := &stubIncidentRepo{inc: inc}
	triageStore := newMemTriageStore()
	prom := metrics.NewSentinelMetrics()

	llm := &fakeLLMClient{fn: func(_ context.Context, req llmagent.CompletionRequest) (llmagent.CompletionResponse, error) {
		if len(req.Tools) != 0 {
			t.Fatalf("LLM request must have zero tools, got %d", len(req.Tools))
		}
		return llmagent.CompletionResponse{
			Content:  validTriageJSON(),
			Attempts: 1,
			Usage:    llmagent.Usage{PromptTokens: 120, CompletionTokens: 80, TotalTokens: 200, Available: true},
		}, nil
	}}

	proc := NewProcessor(incRepo, triageStore, llm, ProcessorConfig{
		Model:       "gpt-4o-mini",
		Clock:       func() time.Time { return now },
		PromMetrics: prom,
	})

	job := worker.Job{ID: "job-1", TenantID: "tenant-a", IncidentID: inc.ID, Attempts: 1, MaxAttempts: 5}
	if err := proc.Process(context.Background(), job); err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	// Verify persisted triage & server-enforced human approval requirement
	view, err := triageStore.GetByIncidentID(context.Background(), "tenant-a", inc.ID)
	if err != nil {
		t.Fatalf("GetByIncidentID failed: %v", err)
	}
	if !view.Triage.RequiresHumanApproval || view.Approval.Status != ApprovalPendingReview {
		t.Fatalf("human approval not enforced: triage=%v approval=%v", view.Triage.RequiresHumanApproval, view.Approval.Status)
	}

	// Unapproved action cannot execute
	if err := CanExecuteAction(view.Approval, "tenant-a", now); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("CanExecuteAction before approval = %v, want ErrApprovalRequired", err)
	}

	// Duplicate job execution must NOT call the LLM again
	if err := proc.Process(context.Background(), job); err != nil {
		t.Fatalf("duplicate Process failed: %v", err)
	}
	if calls := llm.calls.Load(); calls != 1 {
		t.Fatalf("llm.calls = %d, want 1 (duplicate job should skip LLM)", calls)
	}

	// Cross-tenant read must fail
	if _, err := triageStore.GetByIncidentID(context.Background(), "tenant-b", inc.ID); !errors.Is(err, ErrTriageNotFound) {
		t.Fatalf("cross-tenant GetByIncidentID = %v, want ErrTriageNotFound", err)
	}

	// Approve by human principal -> CanExecuteAction succeeds only for owning tenant
	approved, err := triageStore.DecideApproval(context.Background(), "tenant-a", inc.ID, ApprovalApproved, "human-oncall-1", "verified backup healthy", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("DecideApproval failed: %v", err)
	}
	if err := CanExecuteAction(approved, "tenant-a", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("CanExecuteAction after approval failed: %v", err)
	}
	if err := CanExecuteAction(approved, "tenant-b", now.Add(2*time.Minute)); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("cross-tenant CanExecuteAction = %v, want ErrApprovalRequired", err)
	}
}

func TestTriageRejectsMalformedAndUnsafeModelOutputs(t *testing.T) {
	badOutputs := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"unknown_field", `{"classification":"error_spike","recommended_severity":"high","confidence":0.8,"evidence":["e"],"suspected_root_cause":"r","recommended_steps":["s"],"proposed_action":"none","requires_human_approval":true,"extra":1}`},
		{"invalid_classification", `{"classification":"magic","recommended_severity":"high","confidence":0.8,"evidence":["e"],"suspected_root_cause":"r","recommended_steps":["s"],"proposed_action":"none","requires_human_approval":true}`},
		{"invalid_severity", `{"classification":"error_spike","recommended_severity":"urgent","confidence":0.8,"evidence":["e"],"suspected_root_cause":"r","recommended_steps":["s"],"proposed_action":"none","requires_human_approval":true}`},
		{"confidence_out_of_range", `{"classification":"error_spike","recommended_severity":"high","confidence":1.5,"evidence":["e"],"suspected_root_cause":"r","recommended_steps":["s"],"proposed_action":"none","requires_human_approval":true}`},
		{"missing_evidence", `{"classification":"error_spike","recommended_severity":"high","confidence":0.8,"evidence":[],"suspected_root_cause":"r","recommended_steps":["s"],"proposed_action":"none","requires_human_approval":true}`},
		{"invalid_action", `{"classification":"error_spike","recommended_severity":"high","confidence":0.8,"evidence":["e"],"suspected_root_cause":"r","recommended_steps":["s"],"proposed_action":"rm_rf_cluster","requires_human_approval":true}`},
	}

	for _, tc := range badOutputs {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseAndValidateModelOutput(tc.raw); !errors.Is(err, ErrInvalidModelOutput) {
				t.Fatalf("ParseAndValidateModelOutput(%s) err = %v, want ErrInvalidModelOutput", tc.name, err)
			}
		})
	}

	t.Run("VerifierRejectsShellOrSelfApprovalClaims", func(t *testing.T) {
		inc := sampleIncident()
		out, err := ParseAndValidateModelOutput(`{
			"classification":"error_spike",
			"recommended_severity":"high",
			"confidence":0.8,
			"evidence":["504 spike"],
			"suspected_root_cause":"approved_by_model auto_executed",
			"recommended_steps":["run rm -rf /tmp"],
			"proposed_action":"none",
			"requires_human_approval":true
		}`)
		if err != nil {
			t.Fatalf("unexpected parse err: %v", err)
		}
		rep := VerifyProposal(inc, out, nil, time.Now().UTC())
		if rep.SafetyPassed {
			t.Fatal("VerifyProposal should have failed SafetyPassed on prohibited instructions")
		}
		if rep.ActionExecuted {
			t.Fatal("ActionExecuted must remain false")
		}
	})
}

func TestTriageProcessorProviderFailureAndContextCancellation(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	inc := sampleIncident()
	incRepo := &stubIncidentRepo{inc: inc}
	triageStore := newMemTriageStore()
	prom := metrics.NewSentinelMetrics()

	t.Run("ProviderUnavailableDoesNotPersistTriage", func(t *testing.T) {
		llm := &fakeLLMClient{fn: func(_ context.Context, _ llmagent.CompletionRequest) (llmagent.CompletionResponse, error) {
			return llmagent.CompletionResponse{}, &llmagent.HTTPError{Category: "provider_server_error", StatusCode: 503}
		}}
		proc := NewProcessor(incRepo, triageStore, llm, ProcessorConfig{Model: "gpt-4o-mini", Clock: func() time.Time { return now }, PromMetrics: prom})

		err := proc.Process(context.Background(), worker.Job{ID: "j1", TenantID: "tenant-a", IncidentID: inc.ID})
		if err == nil {
			t.Fatal("expected provider error")
		}
		if _, getErr := triageStore.GetByIncidentID(context.Background(), "tenant-a", inc.ID); !errors.Is(getErr, ErrTriageNotFound) {
			t.Fatalf("triage should not be persisted on provider failure, got %v", getErr)
		}
		if got := prom.TriageFailuresTotal.Value("provider_error"); got != 1 {
			t.Fatalf("TriageFailuresTotal(provider_error) = %v, want 1", got)
		}
	})

	t.Run("UnauthorizedToolCallRejected", func(t *testing.T) {
		llm := &fakeLLMClient{fn: func(_ context.Context, _ llmagent.CompletionRequest) (llmagent.CompletionResponse, error) {
			return llmagent.CompletionResponse{
				ToolCalls: []llmagent.ToolCall{{ID: "tc1", Type: "function", Function: llmagent.FunctionCall{Name: "switch_route"}}},
			}, nil
		}}
		proc := NewProcessor(incRepo, triageStore, llm, ProcessorConfig{Model: "gpt-4o-mini", Clock: func() time.Time { return now }, PromMetrics: prom})

		err := proc.Process(context.Background(), worker.Job{ID: "j2", TenantID: "tenant-a", IncidentID: inc.ID})
		if !errors.Is(err, ErrInvalidModelOutput) {
			t.Fatalf("err = %v, want ErrInvalidModelOutput", err)
		}
	})

	t.Run("ContextCanceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		llm := &fakeLLMClient{fn: func(ctx context.Context, _ llmagent.CompletionRequest) (llmagent.CompletionResponse, error) {
			return llmagent.CompletionResponse{}, ctx.Err()
		}}
		proc := NewProcessor(incRepo, triageStore, llm, ProcessorConfig{Model: "gpt-4o-mini", Clock: func() time.Time { return now }})
		if err := proc.Process(ctx, worker.Job{ID: "j3", TenantID: "tenant-a", IncidentID: inc.ID}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
