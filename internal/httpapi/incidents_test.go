package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/incident"
)

const testID = "4b1dd0c8-72aa-4a57-96ac-c5b46a5582ea"

func testPrincipal(tenantID string) auth.Principal {
	return auth.Principal{
		PrincipalID:  "svc-test",
		TenantID:     tenantID,
		CredentialID: "cred-test-uuid",
		Scopes:       []string{auth.ScopeIncidentsRead, auth.ScopeIncidentsWrite},
		AuthMethod:   auth.AuthMethodBearerAPIKey,
	}
}

type fakeRepository struct {
	mu           sync.RWMutex
	created      incident.Incident
	createReplay bool
	createErr    error
	got          incident.Incident
	getErr       error
	createCalls  int
	getCalls     int
	lastTenantID string
	lastInput    incident.CreateInput
	lastContext  context.Context
}

func (f *fakeRepository) Create(ctx context.Context, input incident.CreateInput) (incident.Incident, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastInput = input
	f.lastContext = ctx
	return f.created, f.createReplay, f.createErr
}

func (f *fakeRepository) Get(ctx context.Context, tenantID, _ string) (incident.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	f.lastTenantID = tenantID
	f.lastContext = ctx
	return f.got, f.getErr
}

func (f *fakeRepository) CreateCalls() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.createCalls
}

func (f *fakeRepository) GetCalls() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.getCalls
}

func (f *fakeRepository) LastTenantID() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastTenantID
}

func (f *fakeRepository) LastInput() incident.CreateInput {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastInput
}

func (f *fakeRepository) LastContext() context.Context {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastContext
}

func (f *fakeRepository) SetCreateReplay(replay bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createReplay = replay
}

func (f *fakeRepository) SetGetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func TestCreateIncidentReturnsCreatedRecordAndUsesRequestContext(t *testing.T) {
	want := sampleIncident()
	repo := &fakeRepository{created: want}
	handler := New(repo)
	ctx := context.WithValue(context.Background(), struct{}{}, "request-context")
	ctx = auth.WithPrincipal(ctx, testPrincipal("tenant-demo"))
	req := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(sampleJSON()))
	req = req.WithContext(ctx)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body)
	}
	if response.Header().Get("Location") != "/v1/incidents/"+testID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	if repo.LastContext().Value(struct{}{}) != "request-context" {
		t.Fatal("request context was not passed to repository")
	}
	var body createResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Incident.ID != want.ID || body.IdempotentReplay {
		t.Fatalf("response = %#v", body)
	}
	if repo.LastInput().TenantID != "tenant-demo" {
		t.Fatalf("repository input tenant_id = %q", repo.LastInput().TenantID)
	}
}

func TestCreateIncidentReturnsReplay(t *testing.T) {
	repo := &fakeRepository{created: sampleIncident(), createReplay: true}
	response := serve(t, New(repo), http.MethodPost, "/v1/incidents", sampleJSON())
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var body createResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.IdempotentReplay || response.Header().Get("Location") != "" {
		t.Fatalf("unexpected replay response: %#v, Location=%q", body, response.Header().Get("Location"))
	}
}

func TestCreateIncidentRejectsMalformedAndInvalidRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"tenant_id":`},
		{name: "unknown field", body: strings.TrimSuffix(sampleJSON(), "}") + `,"unexpected":true}`},
		{name: "trailing JSON", body: sampleJSON() + `{}`},
		{name: "missing field", body: strings.Replace(sampleJSON(), `"service":"payment-api",`, "", 1)},
		{name: "invalid severity", body: strings.Replace(sampleJSON(), `"severity":"high"`, `"severity":"urgent"`, 1)},
		{name: "invalid timestamp", body: strings.Replace(sampleJSON(), `"occurred_at":"2026-10-12T10:15:00Z"`, `"occurred_at":"yesterday"`, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepository{created: sampleIncident()}
			response := serve(t, New(repo), http.MethodPost, "/v1/incidents", tt.body)
			assertError(t, response, http.StatusBadRequest, "invalid_request")
			if repo.CreateCalls() != 0 {
				t.Fatal("invalid request reached repository")
			}
		})
	}
}

func TestCreateIncidentRejectsOversizedBody(t *testing.T) {
	repo := &fakeRepository{}
	response := serve(t, New(repo), http.MethodPost, "/v1/incidents", strings.Repeat("x", maxRequestBody+1))
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	if repo.CreateCalls() != 0 {
		t.Fatal("oversized request reached repository")
	}
}

func TestCreateIncidentMapsRepositoryErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "key reused", err: incident.ErrIdempotencyConflict, status: http.StatusConflict, code: "idempotency_key_reused"},
		{name: "storage unavailable", err: incident.ErrUnavailable, status: http.StatusServiceUnavailable, code: "storage_unavailable"},
		{name: "unexpected", err: errors.New("secret database detail"), status: http.StatusInternalServerError, code: "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepository{createErr: tt.err}
			response := serve(t, New(repo), http.MethodPost, "/v1/incidents", sampleJSON())
			assertError(t, response, tt.status, tt.code)
			if strings.Contains(response.Body.String(), "secret database detail") {
				t.Fatal("internal error leaked repository detail")
			}
		})
	}
}

func TestGetIncidentReturnsRecord(t *testing.T) {
	want := sampleIncident()
	repo := &fakeRepository{got: want}
	response := serve(t, New(repo), http.MethodGet, "/v1/incidents/"+testID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var got incident.Incident
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID {
		t.Fatalf("id = %q, want %q", got.ID, want.ID)
	}
}

func TestGetIncidentMapsNotFoundUnavailableAndInvalidID(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		err    error
		status int
		code   string
	}{
		{name: "not found", path: "/v1/incidents/" + testID, err: incident.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
		{name: "storage unavailable", path: "/v1/incidents/" + testID, err: incident.ErrUnavailable, status: http.StatusServiceUnavailable, code: "storage_unavailable"},
		{name: "bad UUID", path: "/v1/incidents/not-a-uuid", status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepository{getErr: tt.err}
			response := serve(t, New(repo), http.MethodGet, tt.path, "")
			assertError(t, response, tt.status, tt.code)
			if tt.name == "bad UUID" && repo.GetCalls() != 0 {
				t.Fatal("invalid ID reached repository")
			}
		})
	}
}

func serve(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.WithPrincipal(request.Context(), testPrincipal("tenant-demo")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body)
	}
	var body errorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("error body = %#v", body.Error)
	}
}

func sampleIncident() incident.Incident {
	return incident.Incident{
		ID: testID, TenantID: "tenant-demo", Source: "synthetic", SourceEventID: "evt-payment-500-001",
		Service: "payment-api", Title: "Payment API returns HTTP 500",
		Description: "Elevated 500 responses began after deployment deploy-42.",
		Severity:    incident.SeverityHigh, OccurredAt: time.Date(2026, 10, 12, 10, 15, 0, 0, time.UTC),
		CreatedAt: time.Date(2026, 10, 12, 10, 16, 0, 0, time.UTC),
	}
}

func sampleJSON() string {
	return `{"tenant_id":"tenant-demo","source":"synthetic","source_event_id":"evt-payment-500-001","service":"payment-api","title":"Payment API returns HTTP 500","description":"Elevated 500 responses began after deployment deploy-42.","severity":"high","occurred_at":"2026-10-12T10:15:00Z"}`
}
