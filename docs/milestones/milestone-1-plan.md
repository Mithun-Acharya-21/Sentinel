# Implementation Plan — Milestone 1: Resilient Agent Engine & Execution Telemetry

## 1. Context & Architecture Inspection Findings

Inspection of `internal/evaluation/llmagent` and `internal/evaluation/task.go` confirms:
1. **Model Calls**: Made inside `Agent.Run` (line 142) calling `Client.Complete(requestCtx, CompletionRequest{...})`.
2. **Current HTTP Client (`HTTPClient`)**: Sends POST to OpenAI-compatible endpoint. On non-2xx status, maps status to string category (`timeout`, `rate_limit`, `authentication`, `provider_server_error`, `malformed_response`, `network_error`) returning an `*HTTPError`.
3. **Current Retries**: **Zero retries exist**. Any single HTTP 429 or 503 causes immediate termination, logging `model_error` and returning `ErrProvider`.
4. **Agent Traces (`AgentTraceEntry`)**: Stored in `task.go`, serialized in report JSON. Currently has `Event`, `Iteration`, `ToolCallID`, `ToolName`, `Arguments`, `Result`, `ErrorCategory`, `Error`.
5. **Usage Metadata (`Usage`)**: Completely unparsed in `HTTPClient.Complete`. OpenAI standard `usage: {prompt_tokens, completion_tokens, total_tokens}` is ignored.
6. **Incomplete Evaluation Contract**: If `Agent.Run` returns `ErrProvider`, `runner.go` marks `EvaluationCompleted: false`, sets `FailureCategory: FailureProvider`, sets `TaskPassed: nil`, `SafetyPassed: nil`, `Passed: nil`.

---

## 2. Milestone 1 Implementation Architecture

### A. Retry & Backoff Configuration (`retry.go`)
Create a dedicated, robust backoff component in `internal/evaluation/llmagent`:
- **`RetryPolicy`**:
  - `MaxAttempts int`: Default 4 (initial + 3 retries).
  - `InitialDelay time.Duration`: Default 500ms.
  - `MaxDelay time.Duration`: Default 10s.
  - `RNG func() float64`: Injectable random float64 in `[0.0, 1.0)`. Defaults to `rand.Float64`.
  - `Sleeper func(ctx context.Context, d time.Duration) error`: Injectable context-aware sleeper. Default uses `time.NewTimer`.
- **Full Jitter Calculation**:
  $$\text{exponential} = \min(\text{MaxDelay}, \text{InitialDelay} \times 2^{\text{attempt}-1})$$
  $$\text{delay} = \text{RNG}() \times \text{exponential}$$
  Prevents duration overflow with checked math.
- **`Retry-After` Support**:
  - Parses delta-seconds (integer seconds) and RFC1123/RFC850 HTTP-date formats.
  - Capped to `MaxDelay` and clamped to $\ge 0$.
  - Precedence: When `Retry-After` is valid and present, it sets the base delay $\max(\text{delay}, \text{retryAfter})$ or bounds jitter.
- **Classification**:
  - Retryable: HTTP 429 (`http.StatusTooManyRequests`), HTTP 503 (`http.StatusServiceUnavailable`).
  - Non-retryable: HTTP 400, 401, 403, 404, malformed request, context cancelled.

### B. Response & Usage Modeling (`openai_client.go`)
- Add `Usage` struct to `CompletionResponse`:
  ```go
  type Usage struct {
      PromptTokens     int  `json:"prompt_tokens"`
      CompletionTokens int  `json:"completion_tokens"`
      TotalTokens      int  `json:"total_tokens"`
      Available        bool `json:"available"`
  }
  ```
- Unmarshal OpenAI-compatible `usage` in `HTTPClient.Complete` if present. If absent, `Available: false`. Never invent numbers.

### C. Attempt Diagnostics & Telemetry (`retry.go` & `task.go`)
- Capture per-attempt diagnostics:
  ```go
  type AttemptDiagnostic struct {
      Attempt     int           `json:"attempt"`
      StatusCode  int           `json:"status_code,omitempty"`
      Category    string        `json:"category,omitempty"`
      DurationMs  int64         `json:"duration_ms"`
      RetryDelayMs int64        `json:"retry_delay_ms,omitempty"`
      Error       string        `json:"error,omitempty"`
  }
  ```
- Extend `CompletionResponse` to include:
  ```go
  Attempts       int
  TotalLatency   time.Duration
  RequestLatency time.Duration
  Usage          Usage
  Diagnostics    []AttemptDiagnostic
  ```
- Extend `AgentTraceEntry` in `internal/evaluation/task.go`:
  - `PromptTokens int `json:"prompt_tokens,omitempty"``
  - `CompletionTokens int `json:"completion_tokens,omitempty"``
  - `TotalTokens int `json:"total_tokens,omitempty"``
  - `UsageAvailable bool `json:"usage_available,omitempty"``
  - `Attempts int `json:"attempts,omitempty"``
  - `RequestLatencyMs int64 `json:"request_latency_ms,omitempty"``
  - `TotalLatencyMs int64 `json:"total_latency_ms,omitempty"``
  (All new fields use `omitempty` to ensure 100% backward compatibility with historical reports).

### D. Bounded Retry Loop in `HTTPClient` / `Agent`
- Wrap `HTTPClient.Complete` (or retry-enabled wrapper) with the bounded retry loop.
- Stop immediately when `ctx.Done()`.

---

## 3. Test Matrix (Deterministic Offline)
In `internal/evaluation/llmagent/agent_test.go` and `internal/evaluation/llmagent/retry_test.go`:
1. 429 then success (verifies retry count = 2, total latency includes backoff).
2. 503 then success.
3. 429 exceeding max attempts (stops at max, returns categorized error).
4. 503 exceeding max attempts.
5. Non-retryable: 400, 401, 403, 404 executed once and aborted immediately.
6. Retry-After delta-seconds parsed and applied.
7. Retry-After HTTP-date parsed and applied.
8. Invalid Retry-After ignored safely.
9. Context cancellation interrupts backoff sleep without executing next attempt.
10. Token usage accurately recorded across single and multi-turn loops.
11. Missing usage reported with `UsageAvailable: false` without hallucinated values.
12. All existing baselines and integration tests verified.
