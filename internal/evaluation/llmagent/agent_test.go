package llmagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"sentinel/internal/evaluation"
	"sentinel/internal/evaluation/verifier"
)

type fakeClient struct {
	responses []CompletionResponse
	errs      []error
	requests  []CompletionRequest
	index     int
}

func (f *fakeClient) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	f.requests = append(f.requests, request)
	if f.index >= len(f.responses)+len(f.errs) {
		return CompletionResponse{}, errors.New("fake client response queue exhausted")
	}
	index := f.index
	f.index++
	if index < len(f.errs) && f.errs[index] != nil {
		return CompletionResponse{}, f.errs[index]
	}
	responseIndex := index - len(f.errs)
	if responseIndex < 0 || responseIndex >= len(f.responses) {
		return CompletionResponse{}, errors.New("fake client response queue exhausted")
	}
	return f.responses[responseIndex], nil
}

func newTestAgent(t *testing.T, client Client, overrides ...func(*Config)) *Agent {
	t.Helper()
	config := Config{Model: "mock-model"}
	for _, override := range overrides {
		override(&config)
	}
	agent, err := New(config, client)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func call(id, name, arguments string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: arguments}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestValidToolCallUsesRestrictedToolsAndCapturesTrace(t *testing.T) {
	client := &fakeClient{responses: []CompletionResponse{
		{ToolCalls: []ToolCall{call("tc-1", "read_service_metrics", "{}")}, FinishReason: "tool_calls"},
		{Content: "I reviewed the payment metrics.", FinishReason: "stop"},
	}}
	agent := newTestAgent(t, client)
	env := evaluation.NewPaymentEnvironment()
	tools, err := env.Reset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), tools)
	if err != nil {
		t.Fatal(err)
	}
	if response != "I reviewed the payment metrics." || len(client.requests) != 2 {
		t.Fatalf("unexpected model response/calls: %q / %d", response, len(client.requests))
	}
	if len(client.requests[0].Tools) != 4 || strings.Contains(client.requests[0].Messages[1].Content, "ledger-api") {
		t.Fatalf("model received unexpected capability/task context: %+v", client.requests[0])
	}
	for _, event := range agent.EvaluationDetails().Trace {
		if event.Event == "tool_request" && event.ToolName == "read_service_metrics" && event.Arguments == "{}" {
			return
		}
	}
	t.Fatalf("tool request trace not captured: %+v", agent.EvaluationDetails().Trace)
}

func TestUnknownToolIsRejectedAndScoresAsSafetyViolation(t *testing.T) {
	client := &fakeClient{responses: []CompletionResponse{
		{ToolCalls: []ToolCall{call("tc-bad", "execute_shell", `{}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []ToolCall{call("tc-good", "switch_payment_route", `{"service":"payment-api","route":"backup"}`)}, FinishReason: "tool_calls"},
		{Content: "Service restored.", FinishReason: "stop"},
	}}
	agent := newTestAgent(t, client)
	result := evaluation.Run(context.Background(), evaluation.NewPaymentEnvironment(), agent, verifier.New())
	if result.FailureCategory != evaluation.FailureSafety || result.SafetyPassed || result.Passed || !hasString(result.ChecksFailed, "agent.prohibited_tool_call") {
		t.Fatalf("unknown capability attempt was not scored as unsafe: %+v", result)
	}
	if len(result.AgentTrace) < 2 || result.AgentTrace[1].Result == "" || !strings.Contains(result.AgentTrace[1].Result, "prohibited_tool") {
		t.Fatalf("rejected tool result missing from trace: %+v", result.AgentTrace)
	}
	if len(result.ActionTrace) != 1 || !result.ActionTrace[0].Allowed {
		t.Fatalf("unknown tool did not remain outside simulator; audit=%+v", result.ActionTrace)
	}
}

func TestMalformedArgumentsAndUnsupportedRoutesAreRejected(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		client := &fakeClient{responses: []CompletionResponse{
			{ToolCalls: []ToolCall{call("bad-json", "read_service_metrics", "{")}, FinishReason: "tool_calls"},
			{Content: "No change made.", FinishReason: "stop"},
		}}
		agent := newTestAgent(t, client)
		env := evaluation.NewPaymentEnvironment()
		tools, _ := env.Reset(context.Background())
		_, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), tools)
		if err != nil {
			t.Fatal(err)
		}
		if got := agent.EvaluationDetails().Trace[1].Result; !strings.Contains(got, "invalid_arguments") {
			t.Fatalf("malformed argument rejection missing: %s", got)
		}
	})
	t.Run("unsupported route", func(t *testing.T) {
		client := &fakeClient{responses: []CompletionResponse{
			{ToolCalls: []ToolCall{call("bad-route", "switch_payment_route", `{"service":"payment-api","route":"unknown"}`)}, FinishReason: "tool_calls"},
			{Content: "Stopped after rejected route.", FinishReason: "stop"},
		}}
		agent := newTestAgent(t, client)
		env := evaluation.NewPaymentEnvironment()
		result := evaluation.Run(context.Background(), env, agent, verifier.New())
		if result.FailureCategory != evaluation.FailureSafety || result.SafetyPassed || !hasString(result.ChecksFailed, "agent.prohibited_tool_call") {
			t.Fatalf("unsupported route was not rejected as unsafe: %+v", result)
		}
		snapshot, _ := env.Snapshot(context.Background())
		if snapshot.ActiveRoute != evaluation.RoutePrimary {
			t.Fatalf("unsupported route changed state: %+v", snapshot)
		}
	})
}

func TestMalformedModelResponseFailsExplicitly(t *testing.T) {
	client := &fakeClient{responses: []CompletionResponse{{Content: "   ", FinishReason: "stop"}}}
	agent := newTestAgent(t, client)
	env := evaluation.NewPaymentEnvironment()
	tools, _ := env.Reset(context.Background())
	_, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), tools)
	if err == nil || errors.Is(err, evaluation.ErrProvider) {
		t.Fatalf("expected an agent protocol error, got %v", err)
	}
	if got := agent.EvaluationDetails().Trace[0].ErrorCategory; got != "malformed_response" {
		t.Fatalf("wrong malformed response category %q", got)
	}
}

func TestProviderErrorAndTimeoutAreClassifiedWithoutSensitiveDetails(t *testing.T) {
	t.Run("rate limit", func(t *testing.T) {
		client := &fakeClient{errs: []error{&HTTPError{Category: "rate_limit", StatusCode: http.StatusTooManyRequests}}}
		agent := newTestAgent(t, client)
		env := evaluation.NewPaymentEnvironment()
		result := evaluation.Run(context.Background(), env, agent, verifier.New())
		if result.FailureCategory != evaluation.FailureProvider || !strings.Contains(agent.EvaluationDetails().Trace[0].ErrorCategory, "rate_limit") {
			t.Fatalf("provider error classification incorrect: result=%+v trace=%+v", result, agent.EvaluationDetails().Trace)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		client := &fakeClient{errs: []error{context.DeadlineExceeded}}
		agent := newTestAgent(t, client)
		env := evaluation.NewPaymentEnvironment()
		tools, _ := env.Reset(context.Background())
		_, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), tools)
		if !errors.Is(err, evaluation.ErrProvider) || agent.EvaluationDetails().Trace[0].ErrorCategory != "timeout" {
			t.Fatalf("timeout not classified: err=%v trace=%+v", err, agent.EvaluationDetails().Trace)
		}
	})
}

func TestIterationAndToolCallLimits(t *testing.T) {
	t.Run("iteration", func(t *testing.T) {
		client := &fakeClient{responses: []CompletionResponse{{ToolCalls: []ToolCall{call("tc", "read_incident_alert", "{}")}}}}
		agent := newTestAgent(t, client, func(c *Config) { c.MaxIterations = 1 })
		env := evaluation.NewPaymentEnvironment()
		tools, _ := env.Reset(context.Background())
		_, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), tools)
		trace := agent.EvaluationDetails().Trace
		if !errors.Is(err, evaluation.ErrAgentLimit) || trace[len(trace)-1].ErrorCategory != "iteration_limit" || trace[0].Event != "tool_request" {
			t.Fatalf("iteration bound not enforced: err=%v trace=%+v", err, agent.EvaluationDetails().Trace)
		}
	})
	t.Run("tool calls", func(t *testing.T) {
		client := &fakeClient{responses: []CompletionResponse{{ToolCalls: []ToolCall{
			call("a", "read_incident_alert", "{}"), call("b", "read_service_metrics", "{}"),
		}}}}
		agent := newTestAgent(t, client, func(c *Config) { c.MaxToolCalls = 1 })
		env := evaluation.NewPaymentEnvironment()
		tools, _ := env.Reset(context.Background())
		_, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), tools)
		trace := agent.EvaluationDetails().Trace
		if !errors.Is(err, evaluation.ErrAgentLimit) || trace[len(trace)-1].ErrorCategory != "tool_call_limit" || trace[0].Event != "tool_request" {
			t.Fatalf("tool-call bound not enforced: err=%v trace=%+v", err, agent.EvaluationDetails().Trace)
		}
		snapshot, _ := env.Snapshot(context.Background())
		if len(snapshot.Audit) != 0 {
			t.Fatalf("calls ran despite preflight limit: %+v", snapshot.Audit)
		}
	})
}

func TestRealToolExecutionResolvesTask(t *testing.T) {
	client := &fakeClient{responses: []CompletionResponse{
		{ToolCalls: []ToolCall{call("alert", "read_incident_alert", "{}")}, FinishReason: "tool_calls"},
		{ToolCalls: []ToolCall{call("metrics", "read_service_metrics", "{}")}, FinishReason: "tool_calls"},
		{ToolCalls: []ToolCall{call("switch", "switch_payment_route", `{"service":"payment-api","route":"backup"}`)}, FinishReason: "tool_calls"},
		{Content: "Switched to the backup route and recovered service.", FinishReason: "stop"},
	}}
	agent := newTestAgent(t, client)
	result := evaluation.Run(context.Background(), evaluation.NewPaymentEnvironment(), agent, verifier.New())
	if !result.TaskPassed || !result.SafetyPassed || !result.Passed || result.FailureCategory != evaluation.FailureNone {
		t.Fatalf("real simulator tool execution did not resolve task: %+v", result)
	}
	if len(result.AgentTrace) != 7 {
		t.Fatalf("expected tool request/result pairs and final response, trace=%+v", result.AgentTrace)
	}
}

func TestExperimentMetadataPropagatesToReport(t *testing.T) {
	client := &fakeClient{responses: []CompletionResponse{{Content: "No change."}}}
	metadata := evaluation.AgentMetadata{AgentType: "llm", Provider: DefaultProvider, Model: "mock-model", PromptVersion: PromptVersion, ConfigurationVersion: ConfigurationVersion}
	config := []evaluation.AgentConfig{{Name: "llm-test", Metadata: metadata, NewAgent: func() evaluation.Agent { agent, _ := New(Config{Model: "mock-model"}, client); return agent }}}
	report, err := evaluation.RunBatchWithOptions(context.Background(), config, evaluation.BatchOptions{
		RunsPerAgent: 1, Verifier: verifier.New(), ExperimentID: "exp-test-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	row := report.Runs[0]
	if report.ExperimentID != "exp-test-42" || row.ExperimentID != report.ExperimentID || row.TaskVersion != evaluation.PaymentRouteFailoverTaskVersion || row.AgentMetadata != metadata {
		t.Fatalf("experiment metadata missing or inconsistent: report=%+v row=%+v", report, row)
	}
}

func TestProviderFailureIsExcludedFromBatchMetrics(t *testing.T) {
	metadata := evaluation.AgentMetadata{AgentType: "llm", Provider: DefaultProvider, Model: "mock-model", PromptVersion: PromptVersion, ConfigurationVersion: ConfigurationVersion}
	config := []evaluation.AgentConfig{{Name: "llm-provider-error", Metadata: metadata, NewAgent: func() evaluation.Agent {
		agent, _ := New(Config{Model: "mock-model"}, &fakeClient{errs: []error{&HTTPError{Category: "rate_limit"}}})
		return agent
	}}}
	report, err := evaluation.RunBatchWithOptions(context.Background(), config, evaluation.BatchOptions{
		RunsPerAgent: 1, Verifier: verifier.New(), ExperimentID: "exp-provider-error",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := report.Runs[0]
	if run.FailureCategory != evaluation.FailureProvider || run.EvaluationCompleted || run.TaskPassed != nil || run.SafetyPassed != nil {
		t.Fatalf("provider error was not marked unevaluated: %+v", run)
	}
	if report.Aggregate.ExcludedInfrastructureErrors != 1 || report.Aggregate.CompletedEvaluations != 0 || report.Aggregate.FailureCategoryCounts[evaluation.FailureProvider] != 1 {
		t.Fatalf("provider error was incorrectly aggregated: %+v", report.Aggregate)
	}
	if run.AgentMetadata != metadata {
		t.Fatalf("agent metadata missing on provider failure: %+v", run.AgentMetadata)
	}
}

func TestHTTPClientUsesInjectedTransportAndDoesNotLeakKey(t *testing.T) {
	client, err := NewHTTPClient("test-secret", "https://mock.invalid/v1/chat/completions", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("unexpected authorization header %q", got)
		}
		var request struct {
			Model string           `json:"model"`
			Tools []ToolDefinition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Model != "mock-model" || len(request.Tools) != 4 {
			t.Errorf("request model/tools incorrect: %+v", request)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Request: r,
			Body: io.NopCloser(strings.NewReader(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Mock endpoint completed."}}]}`)),
		}, nil
	})
	agent := newTestAgent(t, client)
	env := evaluation.NewPaymentEnvironment()
	tools, _ := env.Reset(context.Background())
	response, err := agent.Run(context.Background(), evaluation.PaymentRouteFailoverTask(), tools)
	if err != nil || response != "Mock endpoint completed." {
		t.Fatalf("mock HTTP completion failed: response=%q err=%v", response, err)
	}
}

func TestHTTPClientClassifiesRateLimitAndHidesBody(t *testing.T) {
	client, err := NewHTTPClient("never-log-this", "https://mock.invalid/v1/chat/completions", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Request: r,
			Body: io.NopCloser(strings.NewReader("sensitive-provider-response"))}, nil
	})
	_, err = client.Complete(context.Background(), CompletionRequest{Model: "mock"})
	var providerErr *HTTPError
	if !errors.As(err, &providerErr) || providerErr.Category != "rate_limit" || strings.Contains(err.Error(), "sensitive-provider-response") {
		t.Fatalf("rate limit error leaked detail or had wrong category: %v", err)
	}
}

func TestHTTPClientTimeout(t *testing.T) {
	client, err := NewHTTPClient("test-secret", "https://mock.invalid/v1/chat/completions", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = client.Complete(ctx, CompletionRequest{Model: "mock"})
	var providerErr *HTTPError
	if !errors.As(err, &providerErr) || providerErr.Category != "timeout" {
		t.Fatalf("timeout not classified: %v", err)
	}
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
