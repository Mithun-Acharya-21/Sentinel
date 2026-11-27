# ADR-006: Bounded Provider Retry Strategy with Full Jitter

## Decision
We implement a bounded, context-aware retry policy using exponential backoff with full jitter (`delay = random(0, min(MaxDelay, InitialDelay * 2^(attempt-1)))`) for transient provider errors (HTTP 429 and HTTP 503) within Sentinel's OpenAI-compatible HTTP client. Non-retryable errors (HTTP 400, 401, 403, 404, context cancellation) terminate immediately without retry.

## Context
In live agent evaluations against cloud LLM providers (e.g. Groq, OpenAI), requests frequently encounter transient rate limits (HTTP 429) and temporary gateway overload (HTTP 503). In Sentinel's Increment 12 evaluation, 3 of 5 live trials were lost to immediate HTTP 429 rate-limit interruptions. Without a client-side backoff strategy, batch evaluations on real endpoints fail to collect statistically valid sample sizes.

However, unconstrained retries introduce severe distributed systems hazards:
1. **Retry storms**: When an upstream provider degrades, clients retrying with fixed intervals synchronize and amplify load, worsening the outage.
2. **Infinite loops**: Retrying unrecoverable errors (e.g. invalid API keys or malformed JSON) burns resources and exceeds test deadlines.
3. **Hidden interruptions**: Silently retrying until success without recording attempts masks provider instability.

## Constraints
- Must remain 100% testable offline with zero network calls and deterministic RNG/sleep injection.
- Must honor the overarching request `context.Context` deadline and abort backoff immediately upon cancellation.
- Must preserve the distinction between an agent reasoning failure and an infrastructure provider interruption.

## Alternatives Considered
1. **No Retries (Fail-Fast)**:
   - *Pros*: Simple, zero delay.
   - *Cons*: **Excessive fragility**. As demonstrated in Increment 12, minor momentary bursts drop evaluations unnecessarily.
2. **Fixed Interval Retry (e.g. wait 1s, retry 3 times)**:
   - *Pros*: Simple calculation.
   - *Cons*: Causes **thundering herd / client synchronization**. If 10 agent instances get rate limited at the same second, they will all retry simultaneously at +1s, +2s, and +3s, creating self-inflicted congestion waves.
3. **Exponential Backoff without Jitter**:
   - *Pros*: Expands the time window.
   - *Cons*: Still clusters requests at identical exponential intervals ($2^n$).
4. **Selected Approach: Exponential Backoff with Full Jitter & Retry-After Precedence**:
   - Computes exponential ceiling: $C = \min(\text{MaxDelay}, \text{InitialDelay} \times 2^{\text{attempt}-1})$.
   - Selects delay uniformly at random from $[0, C)$.
   - Incorporates `Retry-After` header when provided, capped to `MaxDelay`.
   - Halts immediately when `ctx.Done()`.

## Why this approach?
- **Spreads Out Demand**: Full jitter completely decorrelates concurrent retry attempts across the time interval, maximizing the probability that subsequent requests hit an open rate-limit token bucket.
- **Strict Bounding**: Maximum attempts (default 4) and maximum delay (default 10s) prevent unbounded execution.
- **Context Awareness**: `time.NewTimer` select blocks on `ctx.Done()` so timeouts or cancellations interrupt sleeping instantly without leaking goroutines.

## Trade-offs
- **Gains**: Massive increase in live evaluation completion rates; zero thundering herd risk.
- **Costs**: Increases worst-case latency for genuinely exhausted quotas up to the bounded retry budget.

## Failure Scenarios
- If an API key is revoked (HTTP 401): Evaluated as non-retryable; exits immediately on attempt 1 without sleeping.
- If upstream is down for 10 minutes: Exhausts max 4 attempts over ~15 seconds and returns a categorized `HTTPError{Category: "provider_server_error"}`, which the runner marks as `evaluation_completed: false` and preserves diagnostics.

## Evidence
- `internal/evaluation/llmagent/retry.go`
- `internal/evaluation/llmagent/retry_test.go` (`TestRetryPolicy_ComputeDelayFullJitter`, `TestHTTPClient_RetryBehavior`)

## Status
Implemented and verified.
