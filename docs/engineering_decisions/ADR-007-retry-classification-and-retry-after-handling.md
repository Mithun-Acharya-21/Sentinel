# ADR-007: Retry-After Header Precedence & Error Classification

## Decision
We implement strict HTTP status classification distinguishing transient retryable provider errors from terminal client errors, and parse RFC 7231 `Retry-After` headers (both delta-seconds and HTTP-date formats) with ceiling clamping.

## Context
Standard HTTP servers and API gateways return `Retry-After` headers on HTTP 429 (Too Many Requests) and HTTP 503 (Service Unavailable) to inform clients of their quota refill schedule or expected recovery time. 

Two critical failure modes must be defended against:
1. **Under-sleeping**: If an agent retries earlier than the server's `Retry-After`, the request is guaranteed to fail and will waste retry attempts.
2. **Denial of Service by Header**: A misconfigured, malicious, or buggy upstream server could return `Retry-After: 86400` (24 hours). If a client blindly trusts this value, the agent evaluation would freeze indefinitely and hang the test harness.

## Constraints
- Must handle both delta-seconds (`"30"`) and RFC1123/RFC850 HTTP-date strings (`"Wed, 26 Nov 2026 12:00:00 GMT"`).
- Invalid, negative, or unparseable headers must fail safely to local jitter calculation without erroring.
- The server's requested delay must be clamped by Sentinel's configured `MaxDelay`.

## Alternatives Considered
1. **Ignore Server `Retry-After` Headers**:
   - *Pros*: Simple client logic.
   - *Cons*: High likelihood of burning through all retry attempts while the server's rate-limit window is still closed.
2. **Blindly Sleep for Server `Retry-After`**:
   - *Pros*: Exactly follows server instructions.
   - *Cons*: Unacceptable vulnerability. An upstream server can hang client threads for arbitrary durations.
3. **Selected Approach: Clamped Precedence with Fallback**:
   - If `Retry-After` is valid: $\text{delay} = \min(\text{MaxDelay}, \max(\text{jitterDelay}, \text{retryAfter}))$.
   - If `Retry-After` is missing, unparseable, or $\le 0$: Fall back entirely to local full jitter.

## Error Classification Policy
- **Retryable (HTTP 429, 503)**: Classified as `rate_limit` or `provider_server_error`. Bounded retries executed.
- **Terminal Client Errors (HTTP 400, 401, 403, 404)**: Classified as `api_error` or `authentication`. Retrying an invalid API key or malformed request body will never succeed; retries are forbidden.
- **Context Interruption (`context.Canceled`, `context.DeadlineExceeded`)**: Classified as `context_cancelled` or `timeout`. Immediately halts execution.

## Evidence
- `internal/evaluation/llmagent/retry.go` (`ParseRetryAfter`, `IsRetryable`)
- `internal/evaluation/llmagent/retry_test.go` (`TestParseRetryAfter`)

## Status
Implemented and verified.
