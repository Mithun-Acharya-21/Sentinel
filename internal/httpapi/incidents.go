package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"sentinel/internal/incident"
)

const maxRequestBody = 1 << 20

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

type API struct {
	repository incident.Repository
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

// New registers the incident endpoints on a standard-library HTTP mux.
func New(repository incident.Repository) http.Handler {
	api := &API{repository: repository}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/incidents", api.createIncident)
	mux.HandleFunc("GET /v1/incidents/{id}", api.getIncident)
	return mux
}

func (api *API) createIncident(w http.ResponseWriter, r *http.Request) {
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
	if err := input.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input.TenantID = strings.TrimSpace(input.TenantID)
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
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must be a UUID")
		return
	}
	got, err := api.repository.Get(r.Context(), id)
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
