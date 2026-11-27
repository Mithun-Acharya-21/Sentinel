# Sentinel — Milestone 2 Engineering Report
## Incoming API Rate Limiting & Client Admission Control

### 1. Milestone Overview & Objectives

Milestone 2 hardens Sentinel's incoming HTTP API (`cmd/sentinel-api`) against Denial-of-Service (DoS), traffic bursts, and noisy-neighbor resource exhaustion. Milestone 1 established outbound resilience when calling upstream LLM providers (retry classification, exponential backoff, jitter, RFC 7231 `Retry-After` parsing). Milestone 2 mirrors this discipline on the inbound boundary, providing safe client admission control without introducing external dependencies or destabilizing existing incident ingestion logic.

---

### 2. Architectural Implementation

#### 2.1 File Map & Component Responsibilities
| File Path | Component | Responsibility |
|---|---|---|
| `internal/httpapi/ratelimit.go` | Token Bucket & Limiter Manager | Per-client token buckets, thread-safe refill calculations, client identity resolution, state eviction, in-memory telemetry, and HTTP middleware. |
| `internal/httpapi/ratelimit_test.go` | Unit & Integration Test Suite | Deterministic tests for burst capacity, clock-advanced refills, `Retry-After` header precision, client isolation, concurrent safety, state eviction, and idempotency protection. |
| `internal/config/config.go` | Configuration Loader | Environment variable parsing for `SENTINEL_RATE_LIMIT_ENABLED`, `SENTINEL_RATE_LIMIT_RPS`, `SENTINEL_RATE_LIMIT_BURST`, and `SENTINEL_RATE_LIMIT_TRUSTED_PROXIES`. |
| `internal/config/config_test.go` | Config Unit Tests | Validates defaults, positive overrides, and rejection of invalid numeric values. |
| `cmd/sentinel-api/main.go` | Application Entrypoint | Initializes the limiter, applies `RateLimitMiddleware` to the incident handler, and registers clean shutdown hooks. |
| `docs/engineering_decisions/ADR-009-incoming-rate-limiting.md` | Architectural Decision Record | Documents rationale, trade-offs, security model, and rejection of alternative algorithms. |

---

### 3. Detailed Technical Design & Mechanics

#### 3.1 Token Bucket Algorithm & Refill Mechanics
Each active client is represented by an independent `tokenBucket` instance:
```go
type tokenBucket struct {
    mu         sync.Mutex
    tokens     float64
    capacity   float64
    refillRate float64
    lastRefill time.Time
    lastSeen   time.Time
}
```

- **Initial State**: Upon arrival of the first request at time $t_0$, a new bucket is allocated with $\text{capacity} - 1.0$ tokens, immediately admitting the first request.
- **Refill Calculation**:
  $$\Delta t = t_{\text{now}} - t_{\text{lastRefill}}$$
  $$\text{tokens} = \min(\text{capacity}, \text{tokens} + \Delta t \times \text{refillRate})$$
- **Admission Rule**:
  - If $\text{tokens} \ge 1.0$: decrement $\text{tokens} \leftarrow \text{tokens} - 1.0$, return `(allowed = true, wait = 0)`.
  - If $\text{tokens} < 1.0$: calculate required wait duration:
    $$\text{deficit} = \max(0.0, 1.0 - \text{tokens})$$
    $$\text{waitSec} = \max\left(1, \left\lceil \frac{\text{deficit}}{\text{refillRate}} \right\rceil\right)$$
    Return `(allowed = false, wait = waitSec * time.Second)`.

#### 3.2 Client Identity Resolution & Anti-Spoofing Architecture
In an incident ingestion system, clients may be automated services, CLI tools, or external webhooks. Sentinel implements a secure three-tier identification hierarchy:
1. **Bearer Token Authentication**: If `Authorization: Bearer <token>` is present, Sentinel extracts the token, computes its SHA-256 digest, and uses `auth:<hex_prefix>` as the client key.
2. **API Key Authentication**: If `X-API-Key: <key>` is present, Sentinel hashes it as `apikey:<hex_prefix>`.
3. **Peer IP Address Fallback**:
   - Sentinel extracts the socket peer address via `net.SplitHostPort(r.RemoteAddr)`.
   - **Anti-Spoofing Rule**: Arbitrary `X-Forwarded-For` and `X-Real-IP` headers are **strictly ignored** unless the immediate peer IP matches a configured trusted proxy (`SENTINEL_RATE_LIMIT_TRUSTED_PROXIES`).
   - When communicating through trusted reverse proxies (e.g., AWS ALB, Cloudflare, Kubernetes Ingress), Sentinel traverses `X-Forwarded-For` from right to left to locate the first untrusted client IP.

#### 3.3 Memory Eviction & Bounded Resource Management
To prevent unbounded memory growth from scanner traffic or transient IP addresses:
- Every bucket access updates `lastSeen = now`.
- A background ticker periodically runs `Cleanup(now)`.
- Buckets with `now.Sub(lastSeen) > BucketTTL` (default 10 minutes) are removed from the internal map under `mu.Lock()`.
- Server shutdown calls `limiter.Stop()`, safely terminating the cleanup goroutine without leaking resources.

#### 3.4 Invariants & Non-Interference Proofs
1. **Health Check Immunity**:
   - `/health/live` and `/health/ready` are mounted directly on the root mux in `main.go`.
   - In addition, `RateLimitMiddleware` explicitly checks `if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready"` and bypasses throttling.
   - Verified via `TestHealthEndpointsExemptFromRateLimiting`: Orchestration probes succeed even when client quota is completely exhausted.
2. **Idempotency Neutrality**:
   - Rate limiting occurs before JSON deserialization and database calls.
   - If admitted, idempotent incident creation succeeds normally (returning HTTP 200 or 201).
   - If throttled (HTTP 429), the repository is never touched, preventing database connection starvation.
   - Verified via `TestRateLimiterWithIncidentIdempotency`.

---

### 4. Verification & Test Suite Results

The complete test suite passed with zero errors, zero vet warnings, and strict code formatting compliance:

```
=== RUN   TestLivenessDoesNotDependOnDatabase
--- PASS: TestLivenessDoesNotDependOnDatabase (0.00s)
=== RUN   TestReadinessReturnsDatabaseStatusWithoutLeakingError
--- PASS: TestReadinessReturnsDatabaseStatusWithoutLeakingError (0.00s)
=== RUN   TestCreateIncidentReturnsCreatedRecordAndUsesRequestContext
--- PASS: TestCreateIncidentReturnsCreatedRecordAndUsesRequestContext (0.00s)
=== RUN   TestCreateIncidentReturnsReplay
--- PASS: TestCreateIncidentReturnsReplay (0.00s)
=== RUN   TestCreateIncidentRejectsMalformedAndInvalidRequests
--- PASS: TestCreateIncidentRejectsMalformedAndInvalidRequests (0.00s)
=== RUN   TestCreateIncidentRejectsOversizedBody
--- PASS: TestCreateIncidentRejectsOversizedBody (0.00s)
=== RUN   TestCreateIncidentMapsRepositoryErrors
--- PASS: TestCreateIncidentMapsRepositoryErrors (0.00s)
=== RUN   TestGetIncidentReturnsRecord
--- PASS: TestGetIncidentReturnsRecord (0.00s)
=== RUN   TestGetIncidentMapsNotFoundUnavailableAndInvalidID
--- PASS: TestGetIncidentMapsNotFoundUnavailableAndInvalidID (0.00s)
=== RUN   TestTokenBucketInitialBurstAndRejection
--- PASS: TestTokenBucketInitialBurstAndRejection (0.00s)
=== RUN   TestTokenBucketRefillOverTime
--- PASS: TestTokenBucketRefillOverTime (0.00s)
=== RUN   TestRetryAfterCalculation
--- PASS: TestRetryAfterCalculation (0.00s)
=== RUN   TestClientIdentityResolution
    --- PASS: TestClientIdentityResolution/BearerTokenTakesPrecedence (0.00s)
    --- PASS: TestClientIdentityResolution/APIKeyFallback (0.00s)
    --- PASS: TestClientIdentityResolution/PeerIPFromUntrustedRemoteAddrIgnoresSpoofedXFF (0.00s)
    --- PASS: TestClientIdentityResolution/TrustedProxyParsesXFF (0.00s)
    --- PASS: TestClientIdentityResolution/TrustedCIDRParsesXFF (0.00s)
--- PASS: TestClientIdentityResolution (0.00s)
=== RUN   TestIndependentClientBuckets
--- PASS: TestIndependentClientBuckets (0.00s)
=== RUN   TestBucketExpirationAndCleanup
--- PASS: TestBucketExpirationAndCleanup (0.00s)
=== RUN   TestConcurrentBucketAccess
--- PASS: TestConcurrentBucketAccess (0.00s)
=== RUN   TestRateLimitMiddlewareIntegration
--- PASS: TestRateLimitMiddlewareIntegration (0.00s)
=== RUN   TestHealthEndpointsExemptFromRateLimiting
--- PASS: TestHealthEndpointsExemptFromRateLimiting (0.00s)
=== RUN   TestRateLimiterWithIncidentIdempotency
--- PASS: TestRateLimiterWithIncidentIdempotency (0.00s)
PASS
ok  	sentinel/internal/httpapi	2.058s
```

All repository packages passed clean execution:
- `sentinel/internal/config`: PASS
- `sentinel/internal/evaluation`: PASS
- `sentinel/internal/evaluation/llmagent`: PASS
- `sentinel/internal/httpapi`: PASS
- `sentinel/internal/storage/postgres`: PASS
- `go vet ./...`: 0 warnings
- `gofmt -l .`: Clean (0 diffs)

---

### 5. Summary & Hand-off

Milestone 2 is complete and verified:
- **Resilient Ingress**: Process-local token bucket protects the HTTP incident API against DoS and quota monopolization.
- **RFC Compliance**: Returns standardized HTTP 429 responses with accurate integer `Retry-After` headers and JSON error envelopes.
- **Secure Identity**: Prevents header spoofing while enabling authenticated tokens and trusted proxy traversal.
- **Clean Boundaries**: Zero third-party dependencies, clean shutdown, and complete non-interference with orchestrator health checks.
