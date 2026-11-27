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
	"sentinel/internal/triage"
)

type memHTTPTriageStore struct {
	mu    sync.Mutex
	views map[string]triage.TriageView
}

func newMemHTTPTriageStore() *memHTTPTriageStore {
	return &memHTTPTriageStore{views: make(map[string]triage.TriageView)}
}

func (m *memHTTPTriageStore) SaveTriageAndApproval(_ context.Context, res triage.TriageResult, app triage.ApprovalRecord) (triage.TriageView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := triage.TriageView{Triage: res, Approval: app}
	m.views[res.IncidentID] = v
	return v, nil
}

func (m *memHTTPTriageStore) GetByIncidentID(_ context.Context, tenantID, incidentID string) (triage.TriageView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.views[incidentID]
	if !ok || v.Triage.TenantID != tenantID {
		return triage.TriageView{}, triage.ErrTriageNotFound
	}
	return v, nil
}

func (m *memHTTPTriageStore) DecideApproval(_ context.Context, tenantID, incidentID string, decision triage.ApprovalStatus, decidedBy, reason string, now time.Time) (triage.ApprovalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.views[incidentID]
	if !ok || v.Approval.TenantID != tenantID {
		return triage.ApprovalRecord{}, triage.ErrTriageNotFound
	}
	if v.Approval.Status != triage.ApprovalPendingReview {
		return triage.ApprovalRecord{}, triage.ErrApprovalAlreadyDecided
	}
	if v.Approval.ExpiresAt != nil && !now.UTC().Before(v.Approval.ExpiresAt.UTC()) {
		v.Approval.Status = triage.ApprovalExpired
		m.views[incidentID] = v
		return triage.ApprovalRecord{}, triage.ErrApprovalExpired
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

func (m *memHTTPTriageStore) PendingApprovalCount(_ context.Context) (int64, error) {
	return 0, nil
}

func TestHTTPTriageAndHumanApprovalWorkflow(t *testing.T) {
	prom := metrics.NewSentinelMetrics()
	store := &stubCredentialStore{creds: make(map[string]auth.StoredCredential)}
	verifier := auth.NewStoreVerifier(store)
	repo := &fakeRepository{created: sampleIncident(), got: sampleIncident()}
	triageStore := newMemHTTPTriageStore()

	now := time.Now().UTC()
	future := now.Add(24 * time.Hour)
	_, _ = triageStore.SaveTriageAndApproval(context.Background(), triage.TriageResult{
		ID:                    "triage-1",
		TenantID:              "tenant-a",
		IncidentID:            testID,
		ContractVersion:       triage.ContractVersion,
		PromptVersion:         triage.PromptVersion,
		Model:                 "gpt-4o-mini",
		Classification:        triage.ClassDependencyFailure,
		RecommendedSeverity:   "high",
		Confidence:            0.9,
		Evidence:              []string{"504 errors"},
		SuspectedRootCause:    "primary route down",
		RecommendedSteps:      []string{"verify backup"},
		ProposedAction:        triage.ActionRouteFailover,
		RequiresHumanApproval: true,
		CreatedAt:             now,
	}, triage.ApprovalRecord{
		ID:             "app-1",
		TriageID:       "triage-1",
		TenantID:       "tenant-a",
		IncidentID:     testID,
		Status:         triage.ApprovalPendingReview,
		ProposedAction: triage.ActionRouteFailover,
		ExpiresAt:      &future,
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	preAuthLimiter, _ := NewLimiter(RateLimitConfig{Enabled: true, RequestsPerSec: 100, BurstCapacity: 100})
	principalLimiter, _ := NewLimiter(RateLimitConfig{Enabled: true, RequestsPerSec: 100, BurstCapacity: 100})
	handler := NewFullProtectedHandler(repo, triageStore, verifier, preAuthLimiter, principalLimiter, prom)

	keyReadWriteOnly := provisionTestKey(t, store, "tenant-a", "svc-writer", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsWrite})
	keyApproverA := provisionTestKey(t, store, "tenant-a", "human-oncall-a", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsApprove})
	keyApproverB := provisionTestKey(t, store, "tenant-b", "human-oncall-b", []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsApprove})

	t.Run("GetTriageSuccessAndCrossTenant404", func(t *testing.T) {
		reqA := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID+"/triage", nil)
		reqA.Header.Set("Authorization", "Bearer "+keyReadWriteOnly.PlaintextToken)
		recA := httptest.NewRecorder()
		handler.ServeHTTP(recA, reqA)
		if recA.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", recA.Code, recA.Body)
		}

		reqB := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+testID+"/triage", nil)
		reqB.Header.Set("Authorization", "Bearer "+keyApproverB.PlaintextToken)
		recB := httptest.NewRecorder()
		handler.ServeHTTP(recB, reqB)
		assertError(t, recB, http.StatusNotFound, "not_found")
	})

	t.Run("UnauthorizedApprovalMissingScopeReturns403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/triage/approve", strings.NewReader(`{"reason":"lgtm"}`))
		req.Header.Set("Authorization", "Bearer "+keyReadWriteOnly.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusForbidden, "forbidden")
	})

	t.Run("QueryParamTenantSubstitutionReturns403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/triage/approve?tenant_id=tenant-b", strings.NewReader(`{"reason":"lgtm"}`))
		req.Header.Set("Authorization", "Bearer "+keyApproverA.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusForbidden, "forbidden")
	})

	t.Run("CrossTenantApprovalReturns404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/triage/approve", strings.NewReader(`{"reason":"steal"}`))
		req.Header.Set("Authorization", "Bearer "+keyApproverB.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusNotFound, "not_found")
	})

	t.Run("ApproveSucceedsAndDuplicateTransitionReturns409", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/triage/approve", strings.NewReader(`{"reason":"verified backup healthy"}`))
		req.Header.Set("Authorization", "Bearer "+keyApproverA.PlaintextToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("approve status = %d, want 200; body=%s", rec.Code, rec.Body)
		}
		var app triage.ApprovalRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &app); err != nil {
			t.Fatal(err)
		}
		if app.Status != triage.ApprovalApproved || app.DecidedBy != "human-oncall-a" {
			t.Fatalf("unexpected approval record: %#v", app)
		}
		if got := prom.ApprovalDecisionsTotal.Value("approved"); got != 1 {
			t.Fatalf("ApprovalDecisionsTotal(approved) = %v, want 1", got)
		}

		// Duplicate approve or reject after decided -> 409 conflict
		reqDup := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+testID+"/triage/reject", strings.NewReader(`{"reason":"changed mind"}`))
		reqDup.Header.Set("Authorization", "Bearer "+keyApproverA.PlaintextToken)
		recDup := httptest.NewRecorder()
		handler.ServeHTTP(recDup, reqDup)
		assertError(t, recDup, http.StatusConflict, "approval_already_decided")
	})
}
