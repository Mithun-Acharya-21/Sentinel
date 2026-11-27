package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"sentinel/internal/auth"
	"sentinel/internal/incident"
	"sentinel/internal/metrics"
	"sentinel/internal/remediation"
	"sentinel/internal/triage"
)

const maxRequestBody = 1 << 20

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

type API struct {
	repository incident.Repository
	triage     triage.Store
	execStore  remediation.ExecutionStore
	executor   *remediation.Executor
	prom       *metrics.SentinelMetrics
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type createResponse struct {
	Incident         incident.Incident `json:"incident"`
	IdempotentReplay bool              `json:"idempotent_replay"`
}

// New registers the incident endpoints on a standard-library HTTP mux with scope enforcement.
func New(repository incident.Repository) http.Handler {
	api := &API{repository: repository}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/incidents", RequireScopeMiddleware(auth.ScopeIncidentsWrite)(http.HandlerFunc(api.createIncident)))
	mux.Handle("GET /v1/incidents/{id}", RequireScopeMiddleware(auth.ScopeIncidentsRead)(http.HandlerFunc(api.getIncident)))
	return mux
}

// NewProtectedHandler wraps the incident API with the two-stage security middleware chain
// using the provided limiter for both pre-auth IP and post-auth principal stages.
func NewProtectedHandler(repository incident.Repository, verifier auth.CredentialVerifier, limiter *Limiter) http.Handler {
	return NewProtectedHandlerWithLimiters(repository, verifier, limiter, limiter)
}

// NewProtectedHandlerWithLimiters wraps the incident API with the exact security middleware chain.
func NewProtectedHandlerWithLimiters(repository incident.Repository, verifier auth.CredentialVerifier, preAuthIPLimiter, principalLimiter *Limiter) http.Handler {
	return NewProtectedHandlerWithObservability(repository, verifier, preAuthIPLimiter, principalLimiter, nil)
}

// NewProtectedHandlerWithObservability wraps the incident API with the security middleware chain
// and Prometheus-compatible HTTP metrics:
// 1. RequestMetadataMiddleware (request ID)
// 2. MetricsMiddleware (HTTP request count & duration)
// 3. PreAuthIPRateLimitMiddleware (Stage 1: peer IP throttling before credential verification)
// 4. AuthenticateMiddleware (Bearer verification -> verified principal context)
// 5. AuthenticatedPrincipalRateLimitMiddleware (Stage 2: verified principal quota)
// 6. RequireAuthMiddlewareWithMetrics (enforces 401/503 for unauthenticated requests)
// 7. RequireScopeMiddleware + Incident handlers (scope enforcement + tenant-isolated repository ops)
func NewProtectedHandlerWithObservability(repository incident.Repository, verifier auth.CredentialVerifier, preAuthIPLimiter, principalLimiter *Limiter, prom *metrics.SentinelMetrics) http.Handler {
	handler := New(repository)
	handler = RequireAuthMiddlewareWithMetrics(prom)(handler)
	handler = AuthenticatedPrincipalRateLimitMiddleware(principalLimiter)(handler)
	handler = AuthenticateMiddleware(verifier)(handler)
	handler = PreAuthIPRateLimitMiddleware(preAuthIPLimiter)(handler)
	handler = MetricsMiddleware(prom)(handler)
	handler = RequestMetadataMiddleware()(handler)
	return handler
}

func (api *API) createIncident(w http.ResponseWriter, r *http.Request) {
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

	var input incident.CreateInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a valid incident JSON object")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return
	}

	if bodyTenant := strings.TrimSpace(input.TenantID); bodyTenant != "" && bodyTenant != principal.TenantID {
		writeError(w, http.StatusForbidden, "forbidden", "tenant_id does not match authenticated tenant")
		return
	}
	// Derive tenant identity authoritatively from the authenticated principal.
	input.TenantID = principal.TenantID

	if err := input.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input.Source = strings.TrimSpace(input.Source)
	input.SourceEventID = strings.TrimSpace(input.SourceEventID)
	input.Service = strings.TrimSpace(input.Service)
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)

	created, replay, err := api.repository.Create(r.Context(), input)
	if err != nil {
		switch {
		case errors.Is(err, incident.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_key_reused", "source event key was already used with different incident fields")
		case errors.Is(err, incident.ErrUnavailable):
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "incident storage is temporarily unavailable")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "incident could not be created")
		}
		return
	}

	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	} else {
		w.Header().Set("Location", "/v1/incidents/"+created.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(createResponse{Incident: created, IdempotentReplay: replay})
}

func (api *API) getIncident(w http.ResponseWriter, r *http.Request) {
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

	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must be a UUID")
		return
	}
	got, err := api.repository.Get(r.Context(), principal.TenantID, id)
	if err != nil {
		switch {
		case errors.Is(err, incident.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "incident was not found")
		case errors.Is(err, incident.ErrUnavailable):
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "incident storage is temporarily unavailable")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "incident could not be retrieved")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(got)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Code: code, Message: message}})
}
