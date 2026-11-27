# Milestone 1 Report: Resilient Agent Engine & Execution Telemetry

Date: 2026-11-26  
Branch: `portfolio-transformation`  
Author: Staff AI Infrastructure & Reliability Engineer  

---

## 1. Original Reliability Problem & Root Cause

### Problem
In live repeated evaluations against cloud LLM endpoints (e.g. Groq `openai/gpt-oss-20b` in Increment 12), **60% of evaluation runs (3 out of 5) were aborted** due to transient HTTP 429 (Too Many Requests) rate-limit interruptions.

### Root Cause
1. **Zero Retries**: `HTTPClient.Complete` made a single HTTP POST request. Any non-2xx status code immediately terminated execution with an `*HTTPError`.
2. **Missing Jittered Backoff**: The agent had no mechanism to pause, back off, or honor provider `Retry-After` headers during burst traffic.
3. **Observability Blind Spots**: The evaluation trace captured iteration counts and error strings, but recorded zero token consumption data, zero request attempt counters, and zero step-level latency measurements.

---

## 2. Implementation Summary

We implemented a bounded, resilient agent engine and execution telemetry pipeline within `internal/evaluation/llmagent` and `internal/evaluation`:

1. **Bounded Exponential Backoff with Full Jitter (`internal/evaluation/llmagent/retry.go`)**:
   - Implemented `ComputeDelay(attempt, retryAfter)` using the full-jitter formula:
     $$\text{factor} = \min(\text{MaxDelay}, \text{InitialDelay} \times 2^{\text{attempt}-1})$$
     $$\text{delay} = \text{RNG}() \times \text{factor}$$
   - Configurable `RetryPolicy` with safe defaults: 4 max attempts (1 initial + 3 retries), 500ms initial delay, 10s max delay ceiling.
   - Overflow-checked exponential factor calculations ($2^{\le 30}$).
   - Strict context-aware sleep using `time.NewTimer` and channel select, halting immediately on `ctx.Done()`.

2. **Retry Classification & `Retry-After` Parser**:
   - **Retryable**: HTTP 429 (`StatusTooManyRequests`), HTTP 503 (`StatusServiceUnavailable`).
   - **Non-retryable**: HTTP 400, 401, 403, 404, context cancellation, and client validation errors.
   - **`Retry-After` Parsing**: Parses delta-seconds (`"30"`) and RFC1123/RFC850 HTTP-date strings, clamped to `MaxDelay`.

3. **Token Usage Accounting**:
   - Parses OpenAI-compatible `usage` object (`prompt_tokens`, `completion_tokens`, `total_tokens`).
   - Explicit `Available: bool` flag. When a provider omits usage data, Sentinel records `usage_available: false` and **never fabricates or estimates token values**.

4. **Latency & Attempt Diagnostics in Traces**:
   - Extended `AgentTraceEntry` in `internal/evaluation/task.go` with backward-compatible `omitempty` fields:
     - `attempts`: Number of attempts taken for that completion request.
     - `prompt_tokens`, `completion_tokens`, `total_tokens`, `usage_available`.
     - `request_latency_ms`: Duration of the successful/final HTTP roundtrip.
     - `total_latency_ms`: Total elapsed duration of the operation including all backoff sleeps.

---

## 3. Files Modified and Created

| File | Status | Description |
|---|---|---|
| `internal/evaluation/task.go` | Modified | Extended `AgentTraceEntry` with token usage and latency fields (`omitempty`). |
| `internal/evaluation/llmagent/agent.go` | Modified | Enriched `Agent.Run` trace generation to record token usage, attempts, and request/total latency. |
| `internal/evaluation/llmagent/openai_client.go` | Modified | Integrated bounded retry loop, `RoundTripper` abstraction, and OpenAI `usage` parsing. |
| `internal/evaluation/llmagent/retry.go` | Created | Bounded exponential backoff, full jitter, `Retry-After` parsing, and retryability classification. |
| `internal/evaluation/llmagent/retry_test.go` | Created | Comprehensive deterministic unit tests for retries, backoff, jitter, cancellation, and token telemetry. |
| `internal/evaluation/llmagent/agent_test.go` | Modified | Updated mock tests to verify injected transport compatibility. |
| `docs/engineering_decisions/ADR-006-bounded-provider-retry-strategy.md` | Created | Architectural decision record for bounded retries and full jitter. |
| `docs/engineering_decisions/ADR-007-retry-classification-and-retry-after-handling.md` | Created | Architectural decision record for retry classification and `Retry-After` clamping. |
| `docs/engineering_decisions/ADR-008-token-usage-and-latency-accounting.md` | Created | Architectural decision record for token accounting and latency telemetry. |
| `docs/milestones/milestone-1-plan.md` | Created | Milestone 1 architecture and test inspection plan. |
| `docs/milestones/milestone-1-report.md` | Created | This completion report. |

---

## 4. Verification & Test Results

All verification commands executed cleanly:

```powershell
# 1. Format check
gofmt -l .
# Outcome: 0 unformatted files

# 2. Package test suite
go test -count=1 ./...
# Outcome:
# ok sentinel/internal/config 2.306s
# ok sentinel/internal/evaluation 2.362s
# ok sentinel/internal/evaluation/llmagent 6.535s
# ok sentinel/internal/httpapi 1.985s
# ok sentinel/internal/storage/postgres 2.357s
# 100% PASS

# 3. Static linter
go vet ./...
# Outcome: 0 warnings, clean

# 4. Evaluation smoke test
go run ./cmd/sentinel-eval -runs-per-agent 1 -out -
# Outcome: PASS (Generates valid JSON with task v1.1.0 and preserved baseline semantics)
```

### Deterministic Unit Test Coverage in `retry_test.go`:
- `TestRetryPolicy_ComputeDelayFullJitter`: Verified exponential factor growth and random jitter ceiling.
- `TestParseRetryAfter`: Verified delta-seconds, RFC1123 date parsing, negative duration handling, and invalid string fallback.
- `TestHTTPClient_RetryBehavior`: Table-driven tests covering:
  - HTTP 429 followed by 200 success (attempt count = 2).
  - HTTP 503 followed by 200 success (attempt count = 2).
  - HTTP 429 exceeding max attempts (halts at 4 attempts, records `rate_limit`).
  - HTTP 503 exceeding max attempts (halts at 4 attempts, records `provider_server_error`).
  - HTTP 400 Bad Request (aborts at attempt 1 without retry).
  - HTTP 401 Unauthorized (aborts at attempt 1 without retry).
  - HTTP 403 Forbidden (aborts at attempt 1 without retry).
  - HTTP 404 Not Found (aborts at attempt 1 without retry).
- `TestHTTPClient_ContextCancellationDuringSleep`: Proved context cancellation immediately aborts backoff without executing subsequent HTTP attempts.
- `TestAgent_TraceTelemetryAndUsageAccounting`: Proved token counts (`prompt_tokens: 120`, `completion_tokens: 35`, `total_tokens: 155`) and latency fields propagate into trace entries.
- `TestAgent_MissingUsageMetadataHandledCleanly`: Proved omitted usage from providers sets `usage_available: false` with zero fabricated tokens.

---

## 5. Remaining Limitations

1. **In-Memory Rate Limiting Only**: Client-side retries protect against external provider bursts, but Sentinel's HTTP API (`cmd/sentinel-api`) does not yet have incoming token-bucket rate limiting (scheduled for Milestone 2).
2. **Provider Scope**: Currently tested against OpenAI-compatible REST schemas. Providers with proprietary non-standard streaming or custom gRPC interfaces require separate adapter implementations.
3. **Trace Export**: Traces are serialized to JSON report artifacts; OpenTelemetry collector span export is planned for Phase 4.

---

## 6. Interview Talking Points & Deep-Dive Defenses

### Q1: Why did you choose full jitter instead of fixed exponential backoff?
> *"Fixed exponential backoff ($2^n$) expands the retry interval but keeps all competing clients synchronized. If 50 agent instances hit a rate-limit burst at the same second, fixed backoff causes all 50 clients to retry simultaneously at $t+1\text{s}$, $t+2\text{s}$, and $t+4\text{s}$, creating self-inflicted thundering herds. Full jitter chooses a uniform random value between $0$ and the exponential ceiling. This completely decorrelates client retry timestamps, maximizing the chance that requests hit newly refilled rate-limit token buckets."*

### Q2: Why should HTTP 401 and 400 never be retried?
> *"Retries are strictly reserved for transient, self-healing conditions—specifically rate limits (429) and temporary gateway overload (503). An HTTP 401 Unauthorized means the API credential is missing or invalid; an HTTP 400 Bad Request means the request JSON schema is rejected. Neither error can possibly succeed on a retry without code or credential changes. Retrying them burns network bandwidth, exhausts request deadlines, and can trigger security lockouts."*

### Q3: How do you prevent retries from exceeding the overall request deadline?
> *"In Go, `http.NewRequestWithContext(ctx, ...)` and our backoff sleeper both listen to the parent `ctx.Done()` channel. If the parent request deadline is 25 seconds and attempt 2 is sleeping with an 8-second backoff when the deadline expires, the timer channel select wakes immediately, aborts the sleep, and returns without executing attempt 3."*

### Q4: How do you handle cases where an LLM provider omits token usage?
> *"We follow strict scientific honesty: we never estimate tokens by dividing characters by 4. In `internal/evaluation/llmagent`, we parse the provider's `usage` object. If present, we populate `prompt_tokens` and set `usage_available: true`. If omitted, we set `usage_available: false` and leave token counts as zero. Fabricating metrics in an evaluation system destroys benchmark credibility."*
