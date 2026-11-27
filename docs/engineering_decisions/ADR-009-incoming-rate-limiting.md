# ADR-009: Incoming API Rate Limiting and Client Admission Control

## Status
Accepted

## Date
2026-11-26

## Context
Sentinel provides an HTTP incident ingestion API (`cmd/sentinel-api`) backed by PostgreSQL. While Milestone 1 established outbound resilience and bounded backoff when Sentinel communicates with upstream LLM providers, Sentinel's own HTTP server remained susceptible to:
1. Denial-of-service (DoS) and resource exhaustion against the PostgreSQL connection pool.
2. Runaway retry loops from automated monitoring, telemetry agents, or misconfigured clients.
3. Noisy-neighbor degradation where one tenant or caller monopolizes server bandwidth.

The rate limiting mechanism needed to satisfy several stringent engineering constraints:
- **Zero External Dependencies**: Must run within the Go standard library without requiring external caching infrastructure (such as Redis or Memcached).
- **Smooth Traffic Shaping**: Must allow short bursts for legitimate sudden traffic while strictly capping sustained ingestion rates.
- **Client Identity Security**: Must prevent IP spoofing via arbitrary HTTP headers like `X-Forwarded-For`.
- **Zero Orchestration Interference**: Must never throttle liveness or readiness probes (`/health/live`, `/health/ready`).
- **Memory Safety**: Must prune inactive client buckets to prevent unbounded memory growth from scanner traffic or ephemeral IP addresses.
- **Deterministic Testability**: Must support injectable virtual clocks to eliminate timing jitter and flakiness in automated test suites.

## Decision

We implemented a process-local, thread-safe **Token Bucket** rate limiter in `internal/httpapi/ratelimit.go` integrated via HTTP middleware.

### 1. Algorithm: Token Bucket vs. Alternatives
We evaluated three classic rate limiting algorithms:
1. **Fixed Window Counter**: Trivial to implement, but suffers from severe traffic spikes at window boundaries (up to $2\times$ the configured limit within a sub-window).
2. **Sliding Window Log**: Stores timestamps of every request. Provides high precision but incurs prohibitive memory overhead ($O(N)$ entries per client) and sorting costs under high throughput.
3. **Token Bucket (Selected)**: Maintains only two scalar state variables (`tokens float64`, `lastRefill time.Time`) per client. Allows smooth traffic shaping with configurable burst capacity $B$ and replenishment velocity $R$ (tokens/second). Ingestion takes $O(1)$ time and consumes bounded memory.

### 2. Client Identity Resolution Hierarchy
To prevent rate-limit bypass and bucket-table exhaustion via forged headers, Sentinel **never** treats unverified `Authorization` or `X-API-Key` headers as client identity:
1. **Verified Principal Context**: If an upstream authentication middleware has verified credentials and attached a principal via `httpapi.WithVerifiedPrincipal(ctx, principal)`, Sentinel computes `sha256(principal)` and namespaces the key as `principal:<hash_prefix>`.
2. **Peer IP Address (Default)**: Defaults to `r.RemoteAddr` (stripped of port and canonicalized via `net.ParseIP`).
   - **Trusted Proxy Traversal**: Sentinel rejects arbitrary `X-Forwarded-For` and `X-Real-IP` headers unless the immediate socket peer matches an explicitly configured list or CIDR block of trusted reverse proxies (`SENTINEL_RATE_LIMIT_TRUSTED_PROXIES`).
   - When the peer is trusted, Sentinel scans all `X-Forwarded-For` header values right-to-left to identify the first untrusted valid upstream client IP, breaking safely back to the peer IP on malformed entries.

### 3. Admission Control & HTTP Protocol Conformance
When a client exceeds their quota:
- HTTP Status Code: `429 Too Many Requests`.
- Standardized Sentinel Error Body:
  ```json
  {
    "error": {
      "code": "rate_limit_exceeded",
      "message": "too many requests, please slow down"
    }
  }
  ```
- Standardized Response Header: `Retry-After: <seconds>` computed as:
  $$\text{deficit} = 1.0 - \text{tokens}$$
  $$\text{waitSec} = \max\left(1, \left\lceil \frac{\text{deficit}}{\text{refillRate}} \right\rceil\right)$$
  This guarantees that once the client waits the prescribed duration, at least one token will have replenished.

### 4. Memory Eviction & Bounded Capacity Policy
- Per-client buckets track `lastSeen time.Time`. A background ticker runs periodically (`DefaultCleanupInterval = 1m`) and removes any bucket inactive for longer than `BucketTTL` (default 10 minutes).
- The bucket table is bounded by `MaxBuckets` (default `10000`). When `len(buckets) >= MaxBuckets` on a new key, the limiter opportunistically evicts expired buckets first; if the table remains full of active clients, new unallocated keys fail closed with HTTP 429 rather than evicting active clients or permitting unbounded memory growth.

### 5. Architectural Boundaries & Health Probe Exemption
- The middleware inspects `r.URL.Path` before key extraction or lock acquisition. Requests to `/health/live` and `/health/ready` immediately proceed to the next handler.
- Rate limiting occurs prior to JSON deserialization and database access, ensuring rejected requests do not consume database transactions, parsing allocations, or idempotency locks.

## Consequences

### Positive
- **Guaranteed Isolation**: Individual clients are isolated into distinct token buckets; exhausting one client's tokens has zero impact on other tenants or health checks.
- **Resource Protection**: The database connection pool and request-parsing CPU are shielded from unconstrained traffic spikes.
- **Observability**: Exposes in-memory atomic metrics for allowed requests, rejected requests, active bucket count, and rejection rate.
- **Zero Third-Party Dependencies**: Retains Go standard library purity without external infrastructure overhead.

### Trade-offs & Limitations
- **Process-Local Scope**: Token buckets are in-memory to the running Go process. In a multi-replica deployment without sticky routing or a shared gateway, total allowed traffic scales linearly with the number of replicas ($N \times \text{limit}$). When horizontal scale-out demands cluster-wide limits, a Redis-backed or distributed token bucket can replace the in-memory storage behind the existing interface.
