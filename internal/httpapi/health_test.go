package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakePinger struct {
	err error
}

func (p fakePinger) PingContext(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return errors.New("readiness ping did not have a deadline")
	}
	return p.err
}

func TestLivenessDoesNotDependOnDatabase(t *testing.T) {
	response := httptest.NewRecorder()
	LivenessHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if response.Code != http.StatusOK || response.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("liveness response = %d %q", response.Code, response.Body.String())
	}
}

func TestReadinessReturnsDatabaseStatusWithoutLeakingError(t *testing.T) {
	tests := []struct {
		name    string
		pingErr error
		status  int
		body    string
	}{
		{name: "ready", status: http.StatusOK, body: "{\"status\":\"ready\"}\n"},
		{name: "not ready", pingErr: errors.New("password=secret database unavailable"), status: http.StatusServiceUnavailable, body: "{\"status\":\"not_ready\"}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			ReadinessHandler(fakePinger{err: tt.pingErr}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
			if response.Code != tt.status || response.Body.String() != tt.body {
				t.Fatalf("readiness response = %d %q", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "secret") {
				t.Fatal("readiness response leaked database error")
			}
		})
	}
}

type mutablePinger struct {
	err error
}

func (p *mutablePinger) PingContext(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return errors.New("missing deadline")
	}
	return p.err
}

func TestReadinessDrainingTimeoutAndRecovery(t *testing.T) {
	pinger := &mutablePinger{err: errors.New("connection refused")}
	state := &ReadinessState{}
	handler := ReadinessHandlerWithState(pinger, state)

	// 1. Unhealthy DB -> 503 not_ready (liveness still 200)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec1.Code != http.StatusServiceUnavailable || rec1.Body.String() != "{\"status\":\"not_ready\"}\n" {
		t.Fatalf("unhealthy readiness = %d %q", rec1.Code, rec1.Body.String())
	}
	recLive := httptest.NewRecorder()
	LivenessHandler().ServeHTTP(recLive, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if recLive.Code != http.StatusOK {
		t.Fatalf("liveness during DB outage = %d, want 200", recLive.Code)
	}

	// 2. DB recovers -> 200 ready
	pinger.err = nil
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec2.Code != http.StatusOK || rec2.Body.String() != "{\"status\":\"ready\"}\n" {
		t.Fatalf("recovered readiness = %d %q", rec2.Code, rec2.Body.String())
	}

	// 3. DB timeout -> 503 not_ready
	pinger.err = context.DeadlineExceeded
	recTimeout := httptest.NewRecorder()
	handler.ServeHTTP(recTimeout, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if recTimeout.Code != http.StatusServiceUnavailable {
		t.Fatalf("timeout readiness = %d, want 503", recTimeout.Code)
	}

	// 4. Draining during shutdown -> 503 draining even if DB is healthy
	pinger.err = nil
	state.SetDraining(true)
	recDrain := httptest.NewRecorder()
	handler.ServeHTTP(recDrain, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if recDrain.Code != http.StatusServiceUnavailable || recDrain.Body.String() != "{\"status\":\"draining\"}\n" {
		t.Fatalf("draining readiness = %d %q", recDrain.Code, recDrain.Body.String())
	}
}
