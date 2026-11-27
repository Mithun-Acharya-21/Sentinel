package llmagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sentinel/internal/evaluation"
)

func TestRetryPolicy_ComputeDelayFullJitter(t *testing.T) {
	policy := RetryPolicy{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     1000 * time.Millisecond,
		RNG:          func() float64 { return 0.5 }, // deterministic 50% jitter
	}

	// Attempt 1: factor = 1 -> 100ms * 0.5 = 50ms
	d1 := policy.ComputeDelay(1, 0)
	if d1 != 50*time.Millisecond {
		t.Fatalf("attempt 1: expected 50ms, got %v", d1)
	}

	// Attempt 2: factor = 2 -> 200ms * 0.5 = 100ms
	d2 := policy.ComputeDelay(2, 0)
	if d2 != 100*time.Millisecond {
		t.Fatalf("attempt 2: expected 100ms, got %v", d2)
	}

	// Attempt 5: factor = 16 -> 1600ms capped to MaxDelay 1000ms * 0.5 = 500ms
	d5 := policy.ComputeDelay(5, 0)
	if d5 != 500*time.Millisecond {
		t.Fatalf("attempt 5: expected 500ms, got %v", d5)
	}

	// Server Retry-After takes precedence if greater than calculated jitter
	dRetryAfter := policy.ComputeDelay(1, 400*time.Millisecond)
	if dRetryAfter != 400*time.Millisecond {
		t.Fatalf("expected 400ms from Retry-After, got %v", dRetryAfter)
	}

	// Server Retry-After is capped by MaxDelay
	dOverMax := policy.ComputeDelay(1, 5000*time.Millisecond)
	if dOverMax != 1000*time.Millisecond {
		t.Fatalf("expected Retry-After capped to MaxDelay 1000ms, got %v", dOverMax)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	// Delta-seconds
	if d := ParseRetryAfter("30", now); d != 30*time.Second {
		t.Fatalf("expected 30s, got %v", d)
	}
	if d := ParseRetryAfter("0", now); d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
	if d := ParseRetryAfter("-5", now); d != 0 {
		t.Fatalf("expected 0 for negative delta, got %v", d)
	}

	// HTTP-date (RFC1123)
	httpDate := now.Add(45 * time.Second).Format(http.TimeFormat)
	if d := ParseRetryAfter(httpDate, now); d != 45*time.Second {
		t.Fatalf("expected 45s for HTTP-date, got %v", d)
	}

	// Invalid header returns 0
	if d := ParseRetryAfter("invalid-date-string", now); d != 0 {
		t.Fatalf("expected 0 for invalid header, got %v", d)
	}
}

type mockTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (m mockTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return m.roundTrip(r)
}

func TestHTTPClient_RetryBehavior(t *testing.T) {
	tests := []struct {
		name             string
		responses        []int
		retryAfterHeader string
		wantAttempts     int
		wantSuccess      bool
		wantCategory     string
	}{
		{
			name:         "HTTP 429 followed by 200 success",
			responses:    []int{http.StatusTooManyRequests, http.StatusOK},
			wantAttempts: 2,
			wantSuccess:  true,
		},
		{
			name:         "HTTP 503 followed by 200 success",
			responses:    []int{http.StatusServiceUnavailable, http.StatusOK},
			wantAttempts: 2,
			wantSuccess:  true,
		},
		{
			name:         "HTTP 429 exceeding max attempts (4 attempts)",
			responses:    []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests},
			wantAttempts: 4,
			wantSuccess:  false,
			wantCategory: "rate_limit",
		},
		{
			name:         "HTTP 503 exceeding max attempts (4 attempts)",
			responses:    []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
			wantAttempts: 4,
			wantSuccess:  false,
			wantCategory: "provider_server_error",
		},
		{
			name:         "HTTP 400 Bad Request is not retried",
			responses:    []int{http.StatusBadRequest},
			wantAttempts: 1,
			wantSuccess:  false,
			wantCategory: "api_error",
		},
		{
			name:         "HTTP 401 Unauthorized is not retried",
			responses:    []int{http.StatusUnauthorized},
			wantAttempts: 1,
			wantSuccess:  false,
			wantCategory: "authentication",
		},
		{
			name:         "HTTP 403 Forbidden is not retried",
			responses:    []int{http.StatusForbidden},
			wantAttempts: 1,
			wantSuccess:  false,
			wantCategory: "authentication",
		},
		{
			name:         "HTTP 404 Not Found is not retried",
			responses:    []int{http.StatusNotFound},
			wantAttempts: 1,
			wantSuccess:  false,
			wantCategory: "api_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var callCount int32
			transport := mockTransport{
				roundTrip: func(r *http.Request) (*http.Response, error) {
					idx := int(atomic.AddInt32(&callCount, 1)) - 1
					statusCode := http.StatusOK
					if idx < len(tc.responses) {
						statusCode = tc.responses[idx]
					}
					header := make(http.Header)
					if tc.retryAfterHeader != "" && statusCode == http.StatusTooManyRequests {
						header.Set("Retry-After", tc.retryAfterHeader)
					}
					var body io.ReadCloser
					if statusCode == http.StatusOK {
						body = io.NopCloser(strings.NewReader(`{
							"choices": [{"finish_reason": "stop", "message": {"role": "assistant", "content": "success"}}],
							"usage": {"prompt_tokens": 15, "completion_tokens": 5, "total_tokens": 20}
						}`))
					} else {
						body = io.NopCloser(strings.NewReader(`{"error": "synthetic"}`))
					}
					return &http.Response{
						StatusCode: statusCode,
						Header:     header,
						Body:       body,
						Request:    r,
					}, nil
				},
			}

			sleepCalls := 0
			policy := RetryPolicy{
				MaxAttempts:  4,
				InitialDelay: 10 * time.Millisecond,
				MaxDelay:     100 * time.Millisecond,
				RNG:          func() float64 { return 0.5 },
				Sleeper: func(ctx context.Context, d time.Duration) error {
					sleepCalls++
					return nil // instant mock sleep
				},
			}

			client, err := NewHTTPClientWithTransport("test-key", "https://mock.invalid/v1/chat/completions", 5*time.Second, policy, transport)
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}

			resp, err := client.Complete(context.Background(), CompletionRequest{
				Model:    "test-model",
				Messages: []Message{{Role: "user", Content: "hello"}},
			})

			if tc.wantSuccess {
				if err != nil {
					t.Fatalf("expected success, got error: %v (raw: %#v)", err, err)
				}
				if resp.Attempts != tc.wantAttempts {
					t.Errorf("attempts = %d; want %d", resp.Attempts, tc.wantAttempts)
				}
				if !resp.Usage.Available || resp.Usage.TotalTokens != 20 {
					t.Errorf("expected 20 tokens, got %+v", resp.Usage)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error, got success: %+v", resp)
				}
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) {
					t.Fatalf("expected *HTTPError, got %T: %v", err, err)
				}
				if httpErr.Category != tc.wantCategory {
					t.Errorf("error category = %q; want %q", httpErr.Category, tc.wantCategory)
				}
				if resp.Attempts != tc.wantAttempts {
					t.Errorf("attempts = %d; want %d", resp.Attempts, tc.wantAttempts)
				}
			}
		})
	}
}

func TestHTTPClient_ContextCancellationDuringSleep(t *testing.T) {
	transport := mockTransport{
		roundTrip: func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error": "rate limited"}`)),
				Request:    r,
			}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	policy := RetryPolicy{
		MaxAttempts:  4,
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     500 * time.Millisecond,
		Sleeper: func(sleepCtx context.Context, d time.Duration) error {
			cancel() // cancel during backoff
			return sleepCtx.Err()
		},
	}

	client, err := NewHTTPClientWithTransport("test-key", "https://mock.invalid/v1/chat/completions", 5*time.Second, policy, transport)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Complete(ctx, CompletionRequest{Model: "test-model"})
	if err == nil {
		t.Fatal("expected context timeout/cancellation error, got nil")
	}
	if resp.Attempts != 1 {
		t.Errorf("expected exactly 1 attempt before cancellation interrupted backoff, got %d", resp.Attempts)
	}
}

func TestAgent_TraceTelemetryAndUsageAccounting(t *testing.T) {
	fake := &fakeClient{
		responses: []CompletionResponse{
			{
				Content:      "investigated and recovered",
				FinishReason: "stop",
				Attempts:     2,
				Usage: Usage{
					PromptTokens:     120,
					CompletionTokens: 35,
					TotalTokens:      155,
					Available:        true,
				},
				RequestLatency: 150 * time.Millisecond,
				TotalLatency:   650 * time.Millisecond,
			},
		},
	}

	agent, err := New(Config{Model: "telemetry-model"}, fake)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), evaluation.NewPaymentEnvironment())
	if err != nil {
		t.Fatalf("unexpected agent error: %v", err)
	}
	if resp != "investigated and recovered" {
		t.Errorf("unexpected agent response: %q", resp)
	}

	details := agent.EvaluationDetails()
	if len(details.Trace) == 0 {
		t.Fatal("expected trace entries, got none")
	}

	finalEntry := details.Trace[0]
	if finalEntry.Event != "final_response" {
		t.Fatalf("expected final_response event, got %q", finalEntry.Event)
	}
	if finalEntry.Attempts != 2 {
		t.Errorf("trace attempts = %d; want 2", finalEntry.Attempts)
	}
	if finalEntry.UsageAvailable == nil || !*finalEntry.UsageAvailable {
		t.Errorf("expected UsageAvailable = true")
	}
	if finalEntry.PromptTokens != 120 || finalEntry.CompletionTokens != 35 || finalEntry.TotalTokens != 155 {
		t.Errorf("unexpected token usage: prompt=%d completion=%d total=%d",
			finalEntry.PromptTokens, finalEntry.CompletionTokens, finalEntry.TotalTokens)
	}
	if finalEntry.RequestLatencyMs != 150 || finalEntry.TotalLatencyMs != 650 {
		t.Errorf("unexpected latency: req=%dms total=%dms",
			finalEntry.RequestLatencyMs, finalEntry.TotalLatencyMs)
	}
}

func TestAgent_MissingUsageMetadataHandledCleanly(t *testing.T) {
	fake := &fakeClient{
		responses: []CompletionResponse{
			{
				Content:      "no usage metadata from provider",
				FinishReason: "stop",
				Attempts:     1,
				Usage:        Usage{Available: false}, // provider returned no usage
			},
		},
	}

	agent, err := New(Config{Model: "no-usage-model"}, fake)
	if err != nil {
		t.Fatal(err)
	}

	_, err = agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), evaluation.NewPaymentEnvironment())
	if err != nil {
		t.Fatal(err)
	}

	details := agent.EvaluationDetails()
	finalEntry := details.Trace[0]
	if finalEntry.UsageAvailable == nil || *finalEntry.UsageAvailable {
		t.Errorf("expected UsageAvailable explicitly false when provider omits usage")
	}
	if finalEntry.PromptTokens != 0 || finalEntry.TotalTokens != 0 {
		t.Errorf("expected 0 token usage when unavailable, got prompt=%d total=%d",
			finalEntry.PromptTokens, finalEntry.TotalTokens)
	}
}

func TestHTTPClient_RequestReplaySafety(t *testing.T) {
	var recordedBodies []string
	var recordedMethods []string
	var recordedURLs []string
	var recordedAuthHeaders []string
	var recordedContentTypes []string

	callCount := 0
	transport := mockTransport{
		roundTrip: func(r *http.Request) (*http.Response, error) {
			callCount++
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			recordedBodies = append(recordedBodies, string(bodyBytes))
			recordedMethods = append(recordedMethods, r.Method)
			recordedURLs = append(recordedURLs, r.URL.String())
			recordedAuthHeaders = append(recordedAuthHeaders, r.Header.Get("Authorization"))
			recordedContentTypes = append(recordedContentTypes, r.Header.Get("Content-Type"))

			if callCount == 1 {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
					Request:    r,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{
					"choices": [{"finish_reason": "stop", "message": {"role": "assistant", "content": "replay success"}}],
					"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
				}`)),
				Request: r,
			}, nil
		},
	}

	policy := RetryPolicy{
		MaxAttempts:  3,
		InitialDelay: time.Millisecond,
		MaxDelay:     10 * time.Millisecond,
		RNG:          func() float64 { return 0.5 },
		Sleeper:      func(ctx context.Context, d time.Duration) error { return nil },
	}

	client, err := NewHTTPClientWithTransport("test-secret-key", "https://mock.invalid/v1/chat/completions", 5*time.Second, policy, transport)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Complete(context.Background(), CompletionRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "critical action payload"}},
	})
	if err != nil {
		t.Fatalf("expected success on second attempt: %v", err)
	}
	if resp.Attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", resp.Attempts)
	}

	// Verify request replay integrity
	if len(recordedBodies) != 2 {
		t.Fatalf("expected 2 recorded requests, got %d", len(recordedBodies))
	}
	for i := 0; i < 2; i++ {
		if recordedMethods[i] != http.MethodPost {
			t.Errorf("attempt %d: method = %q; want POST", i+1, recordedMethods[i])
		}
		if recordedURLs[i] != "https://mock.invalid/v1/chat/completions" {
			t.Errorf("attempt %d: URL = %q; want https://mock.invalid/v1/chat/completions", i+1, recordedURLs[i])
		}
		if recordedAuthHeaders[i] != "Bearer test-secret-key" {
			t.Errorf("attempt %d: auth header = %q; want Bearer test-secret-key", i+1, recordedAuthHeaders[i])
		}
		if recordedContentTypes[i] != "application/json" {
			t.Errorf("attempt %d: content-type = %q; want application/json", i+1, recordedContentTypes[i])
		}
		if !strings.Contains(recordedBodies[i], "critical action payload") {
			t.Errorf("attempt %d: body did not preserve payload: %s", i+1, recordedBodies[i])
		}
	}
	if recordedBodies[0] != recordedBodies[1] {
		t.Errorf("request bodies differ across retries!\n1: %s\n2: %s", recordedBodies[0], recordedBodies[1])
	}
}

func TestAgent_MultiTurnTokenAggregation(t *testing.T) {
	fake := &fakeClient{
		responses: []CompletionResponse{
			{
				FinishReason: "tool_calls",
				ToolCalls: []ToolCall{
					{ID: "call_1", Type: "function", Function: FunctionCall{Name: "read_incident_alert", Arguments: "{}"}},
				},
				Attempts: 1,
				Usage:    Usage{PromptTokens: 50, CompletionTokens: 20, TotalTokens: 70, Available: true},
			},
			{
				FinishReason: "stop",
				Content:      "Resolved payment incident",
				Attempts:     2,
				Usage:        Usage{PromptTokens: 100, CompletionTokens: 30, TotalTokens: 130, Available: true},
			},
		},
	}

	agent, err := New(Config{Model: "multi-turn-model"}, fake)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), evaluation.NewPaymentEnvironment())
	if err != nil {
		t.Fatalf("unexpected agent error: %v", err)
	}
	if resp != "Resolved payment incident" {
		t.Errorf("unexpected response: %q", resp)
	}

	details := agent.EvaluationDetails()
	totalPrompt, totalComp, totalAll := 0, 0, 0
	for _, entry := range details.Trace {
		if entry.UsageAvailable != nil && *entry.UsageAvailable {
			totalPrompt += entry.PromptTokens
			totalComp += entry.CompletionTokens
			totalAll += entry.TotalTokens
		}
	}
	if totalPrompt != 150 || totalComp != 50 || totalAll != 200 {
		t.Errorf("aggregated tokens across 2 turns = prompt:%d comp:%d total:%d; want 150/50/200",
			totalPrompt, totalComp, totalAll)
	}
}

func TestHTTPClient_CancellationBeforeFirstAttempt(t *testing.T) {
	callMade := false
	transport := mockTransport{
		roundTrip: func(r *http.Request) (*http.Response, error) {
			callMade = true
			return nil, errors.New("should not be called")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	client, err := NewHTTPClientWithTransport("test-key", "https://mock.invalid/v1/chat/completions", time.Second, DefaultRetryPolicy(), transport)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Complete(ctx, CompletionRequest{Model: "test-model"})
	if err == nil {
		t.Fatal("expected error on canceled context, got nil")
	}
	if callMade {
		t.Fatal("no HTTP attempt should be made when context is already canceled")
	}
}

func TestAttemptDiagnostic_SerializationExplicitTrueAndFalse(t *testing.T) {
	diagTrue := AttemptDiagnostic{
		Attempt:    1,
		StatusCode: 429,
		Retryable:  boolPtr(true),
	}
	bytesTrue, err := json.Marshal(diagTrue)
	if err != nil {
		t.Fatalf("marshal diagTrue: %v", err)
	}
	strTrue := string(bytesTrue)
	if !strings.Contains(strTrue, `"retryable":true`) {
		t.Errorf("expected explicit retryable:true in json, got: %s", strTrue)
	}

	diagFalse := AttemptDiagnostic{
		Attempt:    1,
		StatusCode: 400,
		Retryable:  boolPtr(false),
	}
	bytesFalse, err := json.Marshal(diagFalse)
	if err != nil {
		t.Fatalf("marshal diagFalse: %v", err)
	}
	strFalse := string(bytesFalse)
	if !strings.Contains(strFalse, `"retryable":false`) {
		t.Errorf("expected explicit retryable:false in json, got: %s", strFalse)
	}

	// Test historical compatibility where retryable was absent (nil)
	diagNil := AttemptDiagnostic{
		Attempt:    1,
		StatusCode: 200,
		Retryable:  nil,
	}
	bytesNil, err := json.Marshal(diagNil)
	if err != nil {
		t.Fatalf("marshal diagNil: %v", err)
	}
	strNil := string(bytesNil)
	if strings.Contains(strNil, `"retryable"`) {
		t.Errorf("expected retryable omitted when nil, got: %s", strNil)
	}

	// Unmarshal historical json without retryable field
	var unmarshaled AttemptDiagnostic
	err = json.Unmarshal([]byte(`{"attempt":1,"status_code":200,"duration_ms":50}`), &unmarshaled)
	if err != nil {
		t.Fatalf("unmarshal historical diagnostic: %v", err)
	}
	if unmarshaled.Retryable != nil {
		t.Errorf("expected unmarshaled.Retryable to be nil for historical json, got %v", *unmarshaled.Retryable)
	}
}

func TestAgentTraceEntry_SerializationExplicitTrueAndFalse(t *testing.T) {
	entryTrue := evaluation.AgentTraceEntry{
		Event:          "final_response",
		UsageAvailable: boolPtr(true),
	}
	bTrue, err := json.Marshal(entryTrue)
	if err != nil {
		t.Fatalf("marshal entryTrue: %v", err)
	}
	if !strings.Contains(string(bTrue), `"usage_available":true`) {
		t.Errorf("expected usage_available:true in json, got: %s", string(bTrue))
	}

	entryFalse := evaluation.AgentTraceEntry{
		Event:          "final_response",
		UsageAvailable: boolPtr(false),
	}
	bFalse, err := json.Marshal(entryFalse)
	if err != nil {
		t.Fatalf("marshal entryFalse: %v", err)
	}
	if !strings.Contains(string(bFalse), `"usage_available":false`) {
		t.Errorf("expected explicit usage_available:false in json, got: %s", string(bFalse))
	}

	// Historical report where usage_available is absent
	var unmarshaled evaluation.AgentTraceEntry
	err = json.Unmarshal([]byte(`{"event":"final_response","iteration":1}`), &unmarshaled)
	if err != nil {
		t.Fatalf("unmarshal historical trace: %v", err)
	}
	if unmarshaled.UsageAvailable != nil {
		t.Errorf("expected nil UsageAvailable for historical trace entry, got %v", *unmarshaled.UsageAvailable)
	}
}
