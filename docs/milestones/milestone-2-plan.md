# Sentinel — Milestone 2 Implementation Plan
## Incoming API Rate Limiting & Client Admission Control

### 1. Executive Summary & Objective

Milestone 2 implements production-grade incoming API rate limiting for Sentinel's HTTP API (`cmd/sentinel-api`). While Milestone 1 secured client-side outbound resilience against upstream LLM provider throttling, Milestone 2 protects Sentinel's core incident-ingestion API against incoming denial-of-service, runaway telemetry loops, and noisy-neighbor resource exhaustion.

The rate limiting engine will be implemented strictly within the Go standard library, maintaining zero third-party dependencies, clean architecture boundaries, deterministic testability, and thread safety.

---

### 2. Phase 1: Audit of Existing Request Flow & Architecture

#### 2.1 Current Server & Routing Architecture
- **Server Entrypoint**: `cmd/sentinel-api/main.go`
  - Instantiates `http.NewServeMux()` with Go 1.22+ method-based route patterns:
    - `GET /health/live` -> `httpapi.LivenessHandler()`
    - `GET /health/ready` -> `httpapi.ReadinessHandler(database)`
    - `/` -> `httpapi.New(repository)`
  - `httpapi.New(repository)` registers:
    - `POST /v1/incidents` -> `api.createIncident`
    - `GET /v1/incidents/{id}` -> `api.getIncident`
  - Server configuration: `http.Server` configured with `ReadHeaderTimeout: 5s`, `ReadTimeout: 10s`, `WriteTimeout: 15s`, `IdleTimeout: 60s`.

#### 2.2 Client Identity & Header Handling
- Currently, Sentinel does not enforce user authentication on the public incident API; incidents specify a `tenant_id` in their JSON body.
- `r.RemoteAddr` contains the immediate socket peer address (`ip:port`).
- Arbitrary upstream headers like `X-Forwarded-For` or `X-Real-IP` are untrusted by default, as spoofing these headers allows malicious clients to bypass IP-based limits or starve arbitrary third parties.

#### 2.3 Error Envelope & Response Protocol
- Consistent JSON error schema in `internal/httpapi/incidents.go`:
  ```json
  {
    "error": {
      "code": "string",
      "message": "string"
    }
  }
  ```
- Function: `writeError(w http.ResponseWriter, status int, code, message string)`.
- Rate limit rejections must conform to:
  - HTTP Status: `429 Too Many Requests`
  - Error code: `"rate_limit_exceeded"`
  - Error message: `"too many requests, please slow down"`
  - Response header: `Retry-After: <seconds>` (RFC 7231 / RFC 6585 integer seconds).

#### 2.4 Critical Invariants & Non-Interference
1. **Health Check Exemption**: `/health/live` and `/health/ready` MUST NEVER be rate limited. Throttling orchestration probes causes false-positive Kubernetes/container restarts.
2. **Idempotency Neutrality**: Rate limiting occurs prior to JSON body parsing and database transactions. If an admission token is granted, subsequent idempotent replays are processed normally. If rejected, no storage locks or connections are consumed.
3. **Process-Local Scope**: Process-local token bucket; no distributed state (Redis/Memcached) is claimed or introduced.

---

### 3. Detailed Component Design

#### 3.1 Token Bucket Algorithm (`TokenBucket`)
- **State per Client**:
  - `capacity float64`: Maximum burst size $B$.
  - `tokens float64`: Current available tokens $\le B$.
  - `refillRate float64`: Refill velocity $R$ in tokens per second.
  - `lastRefill time.Time`: Timestamp of last token replenishment.
  - `lastSeen time.Time`: Timestamp of last client activity (for eviction).
  - `mu sync.Mutex`: Fine-grained per-bucket lock.
- **Refill Logic**:
  $$\Delta t = \text{now}() - \text{lastRefill}$$
  $$\text{tokens} = \min(\text{capacity}, \text{tokens} + \Delta t \times \text{refillRate})$$
  $$\text{lastRefill} = \text{now}()$$
- **Admission Decision (`Allow()`)**:
  - If $\text{tokens} \ge 1.0$:
    - $\text{tokens} \leftarrow \text{tokens} - 1.0$
    - Return `(allowed = true, retryAfter = 0)`
  - If $\text{tokens} < 1.0$:
    - Deficit: $D = 1.0 - \text{tokens}$
    - Wait duration: $T_{\text{wait}} = \lceil D / \text{refillRate} \rceil$ seconds (minimum 1 second)
    - Return `(allowed = false, retryAfter = } T_{\text{wait}} \text{ duration})`
- **Clock Injection**: Accept a `now func() time.Time` to ensure 100% deterministic, non-flaky unit tests.

#### 3.2 Client Identity Extraction (`ClientIdentifier`)
- Strategy:
  1. **API Key / Bearer Token**: If `Authorization: Bearer <token>` or `X-API-Key: <key>` is present, hash the value (SHA-256) or prefix-namespace it: `"key:" + hash(token)`.
  2. **Peer Socket IP**: Fallback to `net.SplitHostPort(r.RemoteAddr)`.
  3. **Trusted Proxies**: Only evaluate `X-Forwarded-For` if `RemoteAddr` matches a configured list/set of trusted proxy IP networks. Untrusted requests use `RemoteAddr` directly.

#### 3.3 State Management & Memory Cleanup (`RateLimiter`)
- **Registry**: `sync.RWMutex` guarding `map[string]*tokenBucket`.
- **Bucket Eviction Policy**:
  - To prevent unbounded memory growth from short-lived or scanner IP addresses, buckets inactive for longer than a configurable TTL (e.g., 10 minutes) are purged.
  - Active cleanup via background ticker (`Stop()` method provided for clean server shutdown).
  - Thread-safe two-phase eviction: inspect with read lock or write lock, remove expired keys safely.

#### 3.4 Telemetry & Observability (`LimiterMetrics`)
- Track in-memory execution telemetry using `sync/atomic`:
  - `AllowedRequests uint64`
  - `RejectedRequests uint64`
- Dynamically report:
  - `ActiveBuckets() int`
  - `RejectionRate() float64`

#### 3.5 HTTP Middleware Integration
- Function: `RateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler`
- Exempts exact paths: `/health/live`, `/health/ready`.
- On rejection: sets `Retry-After: <seconds>`, `Content-Type: application/json`, status `429`, writes error JSON.

---

### 4. Implementation Steps & Milestones

1. **Step 1**: Implement `internal/httpapi/ratelimit.go` containing `TokenBucket`, `RateLimiter`, `RateLimitConfig`, `RateLimitMiddleware`, and metrics.
2. **Step 2**: Add comprehensive tests in `internal/httpapi/ratelimit_test.go`.
3. **Step 3**: Update `internal/config/config.go` with optional rate limit environment variables (`SENTINEL_RATE_LIMIT_RPS`, `SENTINEL_RATE_LIMIT_BURST`, `SENTINEL_RATE_LIMIT_ENABLED`).
4. **Step 4**: Wire rate limiting middleware into `cmd/sentinel-api/main.go`.
5. **Step 5**: Write architectural decision record `docs/engineering_decisions/ADR-009-incoming-rate-limiting.md`.
6. **Step 6**: Produce final report `docs/milestones/milestone-2-report.md`.
