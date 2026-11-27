package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/incident"
	"sentinel/internal/metrics"
	"sentinel/internal/triage"
)

type approvalDecisionBody struct {
	Reason string `json:"reason"`
}

// NewWithTriage registers incident intake/read routes plus tenant-scoped triage and human approval endpoints.
func NewWithTriage(repository incident.Repository, triageStore triage.Store, prom *metrics.SentinelMetrics) http.Handler {
	api := &API{repository: repository, triage: triageStore, prom: prom}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/incidents", RequireScopeMiddleware(auth.ScopeIncidentsWrite)(http.HandlerFunc(api.createIncident)))
	mux.Handle("GET /v1/incidents/{id}", RequireScopeMiddleware(auth.ScopeIncidentsRead)(http.HandlerFunc(api.getIncident)))
	mux.Handle("GET /v1/incidents/{id}/triage", RequireScopeMiddleware(auth.ScopeIncidentsRead)(http.HandlerFunc(api.getTriage)))
	mux.Handle("POST /v1/incidents/{id}/triage/approve", RequireScopeMiddleware(auth.ScopeIncidentsApprove)(http.HandlerFunc(api.approveTriage)))
	mux.Handle("POST /v1/incidents/{id}/triage/reject", RequireScopeMiddleware(auth.ScopeIncidentsApprove)(http.HandlerFunc(api.rejectTriage)))
	return mux
}

// NewFullProtectedHandler wraps the incident and triage/approval API with the full
// security middleware chain and Prometheus observability.
func NewFullProtectedHandler(repository incident.Repository, triageStore triage.Store, verifier auth.CredentialVerifier, preAuthIPLimiter, principalLimiter *Limiter, prom *metrics.SentinelMetrics) http.Handler {
	handler := NewWithTriage(repository, triageStore, prom)
	handler = RequireAuthMiddlewareWithMetrics(prom)(handler)
	handler = AuthenticatedPrincipalRateLimitMiddleware(principalLimiter)(handler)
	handler = AuthenticateMiddleware(verifier)(handler)
	handler = PreAuthIPRateLimitMiddleware(preAuthIPLimiter)(handler)
	handler = MetricsMiddleware(prom)(handler)
	handler = RequestMetadataMiddleware()(handler)
	return handler
}

func (api *API) getTriage(w http.ResponseWriter, r *http.Request) {
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
	if api.triage == nil {
		writeError(w, http.StatusServiceUnavailable, "triage_unavailable", "triage storage is not configured")
		return
	}

	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must be a UUID")
		return
	}

	view, err := api.triage.GetByIncidentID(r.Context(), principal.TenantID, id)
	if err != nil {
		switch {
		case errors.Is(err, triage.ErrTriageNotFound):
			writeError(w, http.StatusNotFound, "not_found", "triage result was not found")
		case errors.Is(err, triage.ErrTriageStoreUnavailable):
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "triage storage is temporarily unavailable")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "triage result could not be retrieved")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}

func (api *API) approveTriage(w http.ResponseWriter, r *http.Request) {
	api.decideTriage(w, r, triage.ApprovalApproved)
}

func (api *API) rejectTriage(w http.ResponseWriter, r *http.Request) {
	api.decideTriage(w, r, triage.ApprovalRejected)
}

func (api *API) decideTriage(w http.ResponseWriter, r *http.Request, decision triage.ApprovalStatus) {
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
	if api.triage == nil {
		writeError(w, http.StatusServiceUnavailable, "triage_unavailable", "triage storage is not configured")
		return
	}

	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must be a UUID")
		return
	}

	var body approvalDecisionBody
	if r.Body != nil {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a valid JSON object")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain at most one JSON object")
			return
		}
	}

	updated, err := api.triage.DecideApproval(
		r.Context(),
		principal.TenantID,
		id,
		decision,
		principal.PrincipalID,
		body.Reason,
		time.Now().UTC(),
	)
	if err != nil {
		switch {
		case errors.Is(err, triage.ErrTriageNotFound):
			writeError(w, http.StatusNotFound, "not_found", "triage approval was not found")
		case errors.Is(err, triage.ErrApprovalAlreadyDecided):
			writeError(w, http.StatusConflict, "approval_already_decided", "triage approval has already been decided")
		case errors.Is(err, triage.ErrApprovalExpired):
			if api.prom != nil {
				api.prom.ApprovalDecisionsTotal.Inc(string(triage.ApprovalExpired))
			}
			writeError(w, http.StatusConflict, "approval_expired", "triage approval has expired")
		case errors.Is(err, triage.ErrTriageStoreUnavailable):
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "triage storage is temporarily unavailable")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "triage approval could not be updated")
		}
		return
	}

	if api.prom != nil {
		api.prom.ApprovalDecisionsTotal.Inc(string(decision))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}
