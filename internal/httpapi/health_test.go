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
