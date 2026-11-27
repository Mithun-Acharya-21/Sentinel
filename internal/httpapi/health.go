package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

const readinessTimeout = time.Second

type databasePinger interface {
	PingContext(context.Context) error
}

type healthResponse struct {
	Status string `json:"status"`
}

// ReadinessState tracks whether the server is draining during graceful shutdown.
type ReadinessState struct {
	draining atomic.Bool
}

// SetDraining marks the server as draining (not ready to accept new traffic).
func (s *ReadinessState) SetDraining(draining bool) {
	if s == nil {
		return
	}
	s.draining.Store(draining)
}

// IsDraining reports whether the server is currently draining.
func (s *ReadinessState) IsDraining() bool {
	if s == nil {
		return false
	}
	return s.draining.Load()
}

// LivenessHandler reports that the HTTP process is serving requests.
// It never fails due to external dependency outages such as PostgreSQL.
func LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, "ok")
	})
}

// ReadinessHandler reports ready only while the database can be reached.
func ReadinessHandler(database databasePinger) http.Handler {
	return ReadinessHandlerWithState(database, nil)
}

// ReadinessHandlerWithState reports ready only while not draining and while PostgreSQL is reachable.
func ReadinessHandlerWithState(database databasePinger, state *ReadinessState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state != nil && state.IsDraining() {
			writeHealth(w, http.StatusServiceUnavailable, "draining")
			return
		}
		if database == nil {
			writeHealth(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := database.PingContext(ctx); err != nil {
			// Do not return driver errors: they can contain connection details.
			writeHealth(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		writeHealth(w, http.StatusOK, "ready")
	})
}

func writeHealth(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(healthResponse{Status: message})
}
