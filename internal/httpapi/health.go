package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const readinessTimeout = time.Second

type databasePinger interface {
	PingContext(context.Context) error
}

type healthResponse struct {
	Status string `json:"status"`
}

// LivenessHandler reports that the HTTP process is serving requests.
func LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, "ok")
	})
}

// ReadinessHandler reports ready only while the database can be reached.
func ReadinessHandler(database databasePinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
