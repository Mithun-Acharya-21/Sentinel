package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sentinel/internal/evaluation/llmagent"
	"sentinel/internal/incident"
	"sentinel/internal/remediation"
	"sentinel/internal/triage"
	"sentinel/internal/worker"
)

type stubIncRepo struct {
	inc incident.Incident
}

func (s *stubIncRepo) Create(_ context.Context, _ incident.CreateInput) (incident.Incident, bool, error) {
	return incident.Incident{}, false, nil
}

func (s *stubIncRepo) Get(_ context.Context, tenantID, id string) (incident.Incident, error) {
	if s.inc.TenantID != tenantID || s.inc.ID != id {
		return incident.Incident{}, incident.ErrNotFound
	}
	return s.inc, nil
}

type stubTriageStore struct {
	saved *triage.TriageView
}

func (s *stubTriageStore) SaveTriageAndApproval(_ context.Context, res triage.TriageResult, app triage.ApprovalRecord) (triage.TriageView, error) {
	res.ID = "triage-mock-1"
	app.ID = "app-mock-1"
	app.TriageID = res.ID
	v := triage.TriageView{Triage: res, Approval: app}
	s.saved = &v
	return v, nil
}

func (s *stubTriageStore) GetByIncidentID(_ context.Context, _, _ string) (triage.TriageView, error) {
	if s.saved == nil {
		return triage.TriageView{}, triage.ErrTriageNotFound
	}
	return *s.saved, nil
}

func (s *stubTriageStore) DecideApproval(_ context.Context, _, _ string, _ triage.ApprovalStatus, _, _ string, _ time.Time) (triage.ApprovalRecord, error) {
	return triage.ApprovalRecord{}, nil
}

func (s *stubTriageStore) PendingApprovalCount(_ context.Context) (int64, error) { return 0, nil }

type inProcessTransport struct {
	handler http.Handler
}

func (t inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func TestMockLLMThroughRealHTTPClientAndTriageProcessor(t *testing.T) {
	client, err := llmagent.NewHTTPClientWithTransport(
		"demo-local-key",
		"http://127.0.0.1:18081/v1/chat/completions",
		5*time.Second,
		llmagent.DefaultRetryPolicy(),
		inProcessTransport{handler: newMux()},
	)
	if err != nil {
		t.Fatalf("NewHTTPClientWithTransport failed: %v", err)
	}

	inc := incident.Incident{
		ID:            "4b1dd0c8-72aa-4a57-96ac-c5b46a5582ea",
		TenantID:      "tenant-demo",
		Source:        "demo-cli",
		SourceEventID: "evt-demo-1",
		Service:       "payment-api",
		Title:         "504 timeouts",
		Description:   "Primary route returning 504",
		Severity:      incident.SeverityHigh,
		OccurredAt:    time.Now().UTC(),
	}
	incRepo := &stubIncRepo{inc: inc}
	triageStore := &stubTriageStore{}
	sim := remediation.NewSimulator(nil)

	proc := triage.NewProcessor(incRepo, triageStore, client, triage.ProcessorConfig{
		Model: "mock-triage-model",
	})

	job := worker.Job{
		ID:         "job-demo-1",
		TenantID:   "tenant-demo",
		IncidentID: inc.ID,
		Attempts:   1,
	}
	if err := proc.Process(context.Background(), job); err != nil {
		t.Fatalf("Processor.Process against mock LLM failed: %v", err)
	}

	if triageStore.saved == nil {
		t.Fatal("triage result was not persisted")
	}
	if triageStore.saved.Triage.ProposedAction != triage.ActionRouteFailover {
		t.Fatalf("proposed_action = %q, want route_failover", triageStore.saved.Triage.ProposedAction)
	}
	if triageStore.saved.Approval.Status != triage.ApprovalPendingReview {
		t.Fatalf("approval status = %q, want pending_review", triageStore.saved.Approval.Status)
	}
	if snap := sim.Snapshot(); snap.ActivePaymentRoute != "primary" || len(snap.AuditEvents) != 0 {
		t.Fatalf("mock LLM triage mutated simulator unexpectedly: %#v", snap)
	}
}
