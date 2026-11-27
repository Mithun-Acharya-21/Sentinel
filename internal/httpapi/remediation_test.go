package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/metrics"
	"sentinel/internal/remediation"
	"sentinel/internal/triage"
)

type memHTTPExecStore struct {
	mu      sync.Mutex
	records map[string]remediation.ExecutionRecord
}

func newMemHTTPExecStore() *memHTTPExecStore {
	return &memHTTPExecStore{records: make(map[string]remediation.ExecutionRecord)}
}

func (m *memHTTPExecStore) ClaimExecution(_ context.Context, req remediation.ClaimRequest) (remediation.ExecutionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ex, ok := m.records[req.ApprovalID]; ok {
		if ex.Status == remediation.StatusSucceeded {
			return ex, true, nil
		}
		if ex.Status == remediation.StatusExecuting {
			return remediation.ExecutionRecord{}, false, remediation.ErrExecutionConflict
		}
	}
	rec := remediation.ExecutionRecord{
		ID:             "exec-http-1",
		TenantID:       req.TenantID,
		IncidentID:     req.IncidentID,
		TriageID:       req.TriageID,
		ApprovalID:     req.ApprovalID,
		ActionType:     req.Action.ActionType,
		ActionVersion:  req.Action.ActionVersion,
		TargetResource: req.Action.TargetResource,
		IdempotencyKey: req.Action.IdempotencyKey,
		ApproverID:     req.ApproverID,
		Status:         remediation.StatusExecuting,
		Attempts:       1,
		ExecutionToken: "tok-http-1",
		RequestedAt:    req.Action.RequestedAt,
	}
	m.records[req.ApprovalID] = rec
	return rec, false, nil
}

func (m *memHTTPExecStore) CompleteExecution(_ context.Context, execID, executionToken string, status remediation.ExecutionStatus, verification remediation.PostActionVerification, failCategory, failMsg string, now time.Time) (remediation.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, rec := range m.records {
		if rec.ID == execID && rec.ExecutionToken == executionToken {
			rec.Status = status
			vp := verification.VerificationPassed
			rec.VerificationPassed = &vp
			rec.VerificationSummary = verification
			rec.FailureCategory = failCategory
			rec.FailureMessage = failMsg
			rec.CompletedAt = &now
			m.records[k] = rec
			return rec, nil
		}
	}
	return remediation.ExecutionRecord{}, remediation.ErrExecutionLeaseLost
}

func (m *memHTTPExecStore) GetByIncidentID(_ context.Context, tenantID, incidentID string) (remediation.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.records {
		if rec.TenantID == tenantID && rec.IncidentID == incidentID {
			return rec, nil
		}
	}
	return remediation.ExecutionRecord{}, remediation.ErrExecutionNotFound
}

func TestHTTPRemediationAuthorizationAndExecution(t *testing.T) {
	prom := metrics.NewSentinelMetrics()
	store := &stubCredentialStore{creds: make(map[string]auth.StoredCredential)}
	verifier := auth.NewStoreVerifier(store)
	repo := &fakeRepository{created: sampleIncident(), got: sampleIncident()}
	triageStore := newMemHTTPTriageStore()
	execStore := newMemHTTPExecStore()
	sim := remediation.NewSimulator(nil)
	executor := remediation.NewExecutor(triageStore, execStore, sim, remediation.ExecutorConfig{PromMetrics: prom})

	now := time.Now().UTC()
	future := now.Add(24 * time.Hour)
	_, _ = triageStore.SaveTriageAndApproval(context.Background(), triage.TriageResult{
		ID:                    "triage-1",
		TenantID:              "tenant-a",
		IncidentID:            testID,
		ProposedAction:        triage.ActionRouteFailover,
		RequiresHumanApproval: true,
	}, triage.ApprovalRecord{
		ID:             "app-1",
		TriageID:       "triage-1",
		TenantID:       "tenant-a",
		IncidentID:     testID,
		Status:         triage.ApprovalPendingReview,
		ProposedAction: triage.ActionRouteFailover,
		ExpiresAt:      &future,
	})

	preAuthLimiter, _ := NewLimiter(RateLimitConfig{Enabled: true, RequestsPerSec: 100, BurstCapacity: 100})
	principalLimiter, _ := NewLimiter(RateLimitConfig{Enabled: true, RequestsPerSec: 100, BurstCapacity: 100})
	handler := NewCompleteProtectedHandler(repo, triageStore, execStore, executor, verifier, preAuthLimiter, principalLimiter, prom)

	keyApprover := provisionTestKey(t, store, "tenant-a", "human-approver", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsApprove})
	keyExecutorA := provisionTestKey(t, store, "tenant-a", "svc-executor-a", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsExecute})
	keyExecutorB := provisionTestKey(t, store, "tenant-b", "svc-executor-b", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsExecute})

	body := `{"action_type":"switch_payment_route_to_backup","action_version":"1.0.0","target_resource":"payment-api","parameters":{"target_route":"backup"},"approval_id":"app-1","idempotency_key":"idem-1","requested_at":"2026-11-27T16:00:00Z"}`

	// 1. Missing incidents:execute scope -> 403 forbidden
	reqNoScope := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/remediate", strings.NewReader(body))
	reqNoScope.Header.Set("Authorization", "Bearer "+keyApprover.PlaintextToken)
	recNoScope := httptest.NewRecorder()
	handler.ServeHTTP(recNoScope, reqNoScope)
	assertError(t, recNoScope, http.StatusForbidden, "forbidden")

	// 2. Pending approval -> 403 approval_required
	reqPending := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/remediate", strings.NewReader(body))
	reqPending.Header.Set("Authorization", "Bearer "+keyExecutorA.PlaintextToken)
	recPending := httptest.NewRecorder()
	handler.ServeHTTP(recPending, reqPending)
	assertError(t, recPending, http.StatusForbidden, "approval_required")

	// 3. Human approves
	reqApp := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/triage/approve", strings.NewReader(`{"reason":"ok"}`))
	reqApp.Header.Set("Authorization", "Bearer "+keyApprover.PlaintextToken)
	recApp := httptest.NewRecorder()
	handler.ServeHTTP(recApp, reqApp)
	if recApp.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200", recApp.Code)
	}

	// 4. Cross-tenant executor -> 404 not_found
	reqCross := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/remediate", strings.NewReader(body))
	reqCross.Header.Set("Authorization", "Bearer "+keyExecutorB.PlaintextToken)
	recCross := httptest.NewRecorder()
	handler.ServeHTTP(recCross, reqCross)
	assertError(t, recCross, http.StatusNotFound, "not_found")

	// 5. Owning tenant executor -> 200 succeeded & verified
	reqExec := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/remediate", strings.NewReader(body))
	reqExec.Header.Set("Authorization", "Bearer "+keyExecutorA.PlaintextToken)
	recExec := httptest.NewRecorder()
	handler.ServeHTTP(recExec, reqExec)
	if recExec.Code != http.StatusOK {
		t.Fatalf("remediate status = %d, want 200; body=%s", recExec.Code, recExec.Body)
	}
	var rec remediation.ExecutionRecord
	if err := json.Unmarshal(recExec.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != remediation.StatusSucceeded || rec.VerificationPassed == nil || !*rec.VerificationPassed {
		t.Fatalf("unexpected execution record: %#v", rec)
	}

	// 6. GET /v1/incidents/{id}/remediation returns audit record
	reqGet := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID+"/remediation", nil)
	reqGet.Header.Set("Authorization", "Bearer "+keyExecutorA.PlaintextToken)
	recGet := httptest.NewRecorder()
	handler.ServeHTTP(recGet, reqGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("get remediation status = %d, want 200", recGet.Code)
	}
}
