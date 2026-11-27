# Sentinel — Milestone 2A Rate Limiter Security Review & Hardening

**Review Date:** 2026-11-26  
**Branch:** `portfolio-transformation`  
**Scope:** `internal/httpapi/ratelimit.go`, `internal/httpapi/ratelimit_test.go`, `internal/config/config.go`, `internal/config/config_test.go`, `cmd/sentinel-api/main.go`

---

## 1. Threat Model

Sentinel's HTTP API (`cmd/sentinel-api`) exposes incident ingestion (`POST /v1/incidents`) and retrieval (`GET /v1/incidents/{id}`) backed by PostgreSQL. Because the current API does not enforce authentication middleware on these endpoints, an unauthenticated or malicious caller can attempt to:
1. **Bypass per-client rate limits** by rotating unverified HTTP headers (`Authorization`, `X-API-Key`, `X-Forwarded-For`, `X-Real-IP`).
2. **Exhaust server memory (OOM DoS)** by forcing unbounded allocation of per-client token-bucket entries.
3. **Spoof client identity** across IPv4/IPv6 representations or malformed forwarding chains behind trusted reverse proxies.
4. **Disrupt availability** via invalid configuration values (`NaN`, `Inf`), clock anomalies, or arithmetic overflow in `Retry-After` headers.

---

## 2. Attack Scenarios & Actual Vulnerabilities Found

### Finding 1 — [HIGH] Complete Rate-Limit Bypass & Memory Exhaustion via Unverified Auth Headers
- **Location:** `internal/httpapi/ratelimit.go` (`ExtractClientKey`)
- **Vulnerability:** Prior to this audit, `ExtractClientKey` hashed raw `Authorization: Bearer <token>` and `X-API-Key: <key>` headers before falling back to peer IP. Because Sentinel has no authentication middleware validating those credentials, an attacker from a single IP could send arbitrary rotating values (`Bearer fake-1`, `Bearer fake-2`, `X-API-Key: fake-N`), receiving a fresh `BurstCapacity` bucket on every request and allocating unbounded map entries.
- **Fix Implemented:** Removed unverified `Authorization` and `X-API-Key` header extraction completely. Added `WithVerifiedPrincipal(ctx, principal)` so only verified authentication middleware can supply a principal identity (still hashed with SHA-256). Unauthenticated requests always resolve to the trusted peer IP.

### Finding 2 — [HIGH] Unbounded Bucket Growth & Inactive Background Cleanup in `sentinel-api`
- **Location:** `cmd/sentinel-api/main.go` and `internal/httpapi/ratelimit.go`
- **Vulnerability:**
  1. `cmd/sentinel-api/main.go` instantiated `httpapi.NewLimiter` without setting `CleanupInterval`, leaving it at `0`. Because `NewLimiter` only started `runCleanup` when `CleanupInterval > 0`, **background bucket eviction never ran in production**.
  2. `Limiter` had no `MaxBuckets` cap, allowing distributed botnets or IPv6 `/64` scanners to grow `l.buckets` without bound.
- **Fix Implemented:**
  1. Wired `CleanupInterval: httpapi.DefaultCleanupInterval`, `BucketTTL: httpapi.DefaultBucketTTL`, and `MaxBuckets: httpapi.DefaultMaxBuckets` into `cmd/sentinel-api/main.go`.
  2. Added `MaxBuckets` (default `10000`) to `RateLimitConfig`. When `len(buckets) >= MaxBuckets` on a new key, `Allow` opportunistically evicts expired buckets (`evictExpiredLocked`); if the table remains full of active clients, new unallocated keys fail closed with HTTP `429` rather than evicting active clients or growing memory unboundedly.

### Finding 3 — [MEDIUM] Trusted Proxy `X-Forwarded-For` Traversal & IPv6 Edge Cases
- **Location:** `internal/httpapi/ratelimit.go` (`ExtractClientKey`)
- **Vulnerability:**
  1. `r.Header.Get("X-Forwarded-For")` only inspected the first `X-Forwarded-For` header line if a proxy appended a separate header line.
  2. The right-to-left scan continued past malformed entries (`candIP == nil`) instead of halting, potentially allowing an attacker-injected IP further left to be selected if a malformed token appeared on the right.
  3. Bracketed IPv6 addresses without ports (e.g., `[2001:db8::1]`) failed `net.SplitHostPort` and `net.ParseIP`.
- **Fix Implemented:** Switched to `r.Header.Values("X-Forwarded-For")`, broke out of the right-to-left scan immediately on empty/malformed entries (falling back to the verified peer IP), stripped brackets on portless IPv6 `RemoteAddr`, and normalized empty `RemoteAddr` to `ip:unknown`.

### Finding 4 — [MEDIUM] `NaN` / `Inf` Float Injection in Rate Limit Configuration
- **Location:** `internal/config/config.go` (`LoadFrom`) and `internal/httpapi/ratelimit.go` (`NewLimiter`)
- **Vulnerability:** `strconv.ParseFloat("NaN", 64)` and `strconv.ParseFloat("Inf", 64)` succeed without error, and `NaN <= 0` evaluates to `false` in IEEE 754 arithmetic. Passing `SENTINEL_RATE_LIMIT_RPS=NaN` would poison token refill math (`b.tokens >= 1.0` always `false`), locking out all clients.
- **Fix Implemented:** Explicitly rejected `math.IsNaN` and `math.IsInf` in both `config.LoadFrom` and `httpapi.NewLimiter`.

### Finding 5 — [LOW] Backward Clock `lastSeen` Regression & `Retry-After` Duration Overflow
- **Location:** `internal/httpapi/ratelimit.go` (`tokenBucket.allow`)
- **Vulnerability:**
  1. If the system clock stepped backward, `b.lastSeen = now` moved `lastSeen` backward, risking premature TTL eviction.
  2. Extremely small `refillRate` values could cause `time.Duration(waitSec) * time.Second` to overflow `int64` nanoseconds into a negative duration.
- **Fix Implemented:** Guarded `if now.After(b.lastSeen) { b.lastSeen = now }` and capped `waitSec` at `maxRetryAfterSeconds` (`math.MaxInt64 / int64(time.Second)`).

---

## 3. Adversarial Regression Tests Added

Added deterministic tests in `internal/httpapi/ratelimit_test.go` and `internal/config/config_test.go`:
- `TestAdversarialHeaderRotationBypassPrevented`: Verifies rotating fake `Authorization: Bearer ...` and `X-API-Key: ...` headers from the same IP shares 1 bucket and returns HTTP `429`.
- `TestAdversarialProxyAndForwardingEdgeCases`: Tests spoofed `XFF`/`X-Real-IP` from untrusted peers, multi-proxy + multi-header `XFF` chains, malformed `XFF` fallback, IPv6 canonicalization, trusted IPv6 proxies, and empty `RemoteAddr`.
- `TestBucketExhaustionBoundsMemoryAndPreservesActiveClients`: Verifies `MaxBuckets` fail-closed saturation, active-client fairness, and opportunistic stale-bucket eviction.
- `TestConcurrentCleanupAndRequests`: Exercises concurrent `Allow` and `Cleanup` calls across goroutines.
- `TestClockBackwardAndForwardJumps`: Verifies backward clock steps preserve tokens/`lastSeen`, exact refill boundaries admit, and large forward jumps cap at `BurstCapacity`.
- `TestRetryAfterFractionalRoundingAndOverflowGuard`: Verifies fractional wait rounding up (`Ceil`) and `time.Duration` overflow protection.
- `TestRateLimiterBackgroundCleanupAndStopIdempotency`: Verifies background ticker eviction and idempotent `Stop()`.
- `TestLoadFromRateLimitSettings/invalid_RPS_rejected`: Verifies `NaN`, `Inf`, and `-Inf` rejection in config.

---

## 4. Remaining Limitations & Multi-Instance Trade-Offs

- **Process-Local State:** Token buckets live strictly in process memory (`Limiter.buckets`).
  - **Multiple Instances:** If $N$ independent `sentinel-api` replicas run behind a round-robin load balancer without sticky sessions or an edge gateway, an individual client's effective sustained limit scales up to $N \times \text{RequestsPerSec}$ and burst scales up to $N \times \text{BurstCapacity}$.
  - **Process Restarts:** Restarting a process clears all in-memory buckets and metrics counters, resetting client quotas to full `BurstCapacity`.
  - **IPv6 /64 Rotation:** An attacker controlling a large IPv6 prefix (e.g., `/64`) can still rotate source IPs; `MaxBuckets` bounds Sentinel's memory footprint under such a flood, though distributed/edge enforcement or authenticated principals are required for strict per-tenant quotas across IPv6 ranges.

---

## 5. Verification Results

Executed after final edits:
- `gofmt -l .`: Exit code `0` (no unformatted files).
- `go test -count=1 ./...`: Exit code `0` — all packages passed (`internal/config`, `internal/evaluation`, `internal/evaluation/llmagent`, `internal/httpapi`, `internal/storage/postgres`).
- `go vet ./...`: Exit code `0` (no diagnostics).
- `git diff --check`: Exit code `0` (no whitespace/conflict marker errors).
- `go test -race -count=1 ./internal/httpapi`: **Blocked by host environment** (`go: -race requires cgo`, and with `CGO_ENABLED=1`: `cgo: C compiler "gcc" not found: exec: "gcc": executable file not found in %PATH%`). Race detector execution must be run in a Linux/MSYS2 environment with `gcc` installed before production sign-off.
