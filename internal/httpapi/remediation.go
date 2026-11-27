package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"sentinel/internal/auth"
	"sentinel/internal/incident"
	"sentinel/internal/metrics"
	"sentinel/internal/remediation"
	"sentinel/internal/triage"
)

// NewWithRemediation registers incident, triage/approval, and controlled remediation routes.
func NewWithRemediation(repository incident.Repository, triageStore triage.Store, execStore remediation.ExecutionStore, executor *remediation.Executor, prom *metrics.SentinelMetrics) http.Handler {
	api := &API{
		repository: repository,
		triage:     triageStore,
		execStore:  execStore,
		executor:   executor,
		prom:       prom,
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/incidents", RequireScopeMiddleware(auth.ScopeIncidentsWrite)(http.HandlerFunc(api.createIncident)))
	mux.Handle("GET /v1/incidents/{id}", RequireScopeMiddleware(auth.ScopeIncidentsRead)(http.HandlerFunc(api.getIncident)))
	mux.Handle("GET /v1/incidents/{id}/triage", RequireScopeMiddleware(auth.ScopeIncidentsRead)(http.HandlerFunc(api.getTriage)))
	mux.Handle("POST /v1/incidents/{id}/triage/approve", RequireScopeMiddleware(auth.ScopeIncidentsApprove)(http.HandlerFunc(api.approveTriage)))
	mux.Handle("POST /v1/incidents/{id}/triage/reject", RequireScopeMiddleware(auth.ScopeIncidentsApprove)(http.HandlerFunc(api.rejectTriage)))
	mux.Handle("POST /v1/incidents/{id}/remediate", RequireScopeMiddleware(auth.ScopeIncidentsExecute)(http.HandlerFunc(api.executeRemediation)))
	mux.Handle("GET /v1/incidents/{id}/remediation", RequireScopeMiddleware(auth.ScopeIncidentsRead)(http.HandlerFunc(api.getRemediation)))
	return mux
}

// NewCompleteProtectedHandler wraps all incident, triage, approval, and controlled remediation
// routes with the full security middleware chain and Prometheus observability.
func NewCompleteProtectedHandler(repository incident.Repository, triageStore triage.Store, execStore remediation.ExecutionStore, executor *remediation.Executor, verifier auth.CredentialVerifier, preAuthIPLimiter, principalLimiter *Limiter, prom *metrics.SentinelMetrics) http.Handler {
	handler := NewWithRemediation(repository, triageStore, execStore, executor, prom)
	handler = RequireAuthMiddlewareWithMetrics(prom)(handler)
	handler = AuthenticatedPrincipalRateLimitMiddleware(principalLimiter)(handler)
	handler = AuthenticateMiddleware(verifier)(handler)
	handler = PreAuthIPRateLimitMiddleware(preAuthIPLimiter)(handler)
	handler = MetricsMiddleware(prom)(handler)
	handler = RequestMetadataMiddleware()(handler)
	return handler
}

func (api *API) executeRemediation(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sentinel"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer credentials are required")
		return
	}
	if qTenant := strings.TrimSpace(r.URL.Query().Get("tenant_id")); qTenant != "" && qTenant != principal.TenantID {
		writeError(w, http.StatusForbidden, "forbidden", "tenant_id does not match authenticated tenant")
		return
	}
	if api.executor == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation_unavailable", "remediation executor is not configured")
		return
	}

	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must be a UUID")
		return
	}

	var req remediation.ActionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a valid remediation action JSON object")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return
	}

	rec, err := api.executor.Execute(r.Context(), principal.TenantID, id, req)
	if err != nil {
		switch {
		case errors.Is(err, remediation.ErrInvalidAction):
			writeError(w, http.StatusBadRequest, "invalid_action", err.Error())
		case errors.Is(err, remediation.ErrActionMismatch):
			writeError(w, http.StatusForbidden, "action_mismatch", err.Error())
		case errors.Is(err, triage.ErrTriageNotFound):
			writeError(w, http.StatusNotFound, "not_found", "triage or approval was not found")
		case errors.Is(err, triage.ErrApprovalRequired):
			writeError(w, http.StatusForbidden, "approval_required", "explicit human approval is required before remediation")
		case errors.Is(err, triage.ErrApprovalExpired):
			writeError(w, http.StatusConflict, "approval_expired", "triage approval has expired")
		case errors.Is(err, remediation.ErrExecutionConflict), errors.Is(err, remediation.ErrExecutionLeaseLost):
			writeError(w, http.StatusConflict, "execution_conflict", "remediation execution is already in progress or lease was lost")
		case errors.Is(err, remediation.ErrRemediationStoreError), errors.Is(err, triage.ErrTriageStoreUnavailable):
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "remediation storage is temporarily unavailable")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "remediation could not be executed")
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (api *API) getRemediation(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sentinel"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer credentials are required")
		return
	}
	if qTenant := strings.TrimSpace(r.URL.Query().Get("tenant_id")); qTenant != "" && qTenant != principal.TenantID {
		writeError(w, http.StatusForbidden, "forbidden", "tenant_id does not match authenticated tenant")
		return
	}
	if api.execStore == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation_unavailable", "remediation storage is not configured")
		return
	}

	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must be a UUID")
		return
	}

	rec, err := api.execStore.GetByIncidentID(r.Context(), principal.TenantID, id)
	if err != nil {
		switch {
		case errors.Is(err, remediation.ErrExecutionNotFound):
			writeError(w, http.StatusNotFound, "not_found", "remediation execution was not found")
		case errors.Is(err, remediation.ErrRemediationStoreError):
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "remediation storage is temporarily unavailable")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "remediation execution could not be retrieved")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}
