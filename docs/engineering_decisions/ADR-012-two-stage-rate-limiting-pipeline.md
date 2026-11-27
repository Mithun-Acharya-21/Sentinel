# ADR-012: Two-Stage Rate Limiting & Pre-Auth Flood Protection

## Status
Accepted (supersedes the single-limiter ordering in ADR-011)

## Date
2026-11-27

## Context
In Milestone 2B (`ADR-011`), `AuthenticateMiddleware` executed *before* `RateLimitMiddleware` so that authenticated requests could populate `WithVerifiedPrincipal` and share a single principal bucket. However, an adversarial audit (Milestone 2C) identified a critical flaw in that ordering:

Any unauthenticated caller sending syntactically valid but nonexistent `Authorization: Bearer snt_<16hex>_<secret>` tokens triggered `CredentialRepository.GetByKeyID` (a PostgreSQL `SELECT` query) on **every request before** `RateLimitMiddleware` could throttle the peer IP. An attacker could saturate the PostgreSQL connection pool (`MaxOpenConns = 10`) from a single IP address.

## Decision
We replaced the single post-auth `RateLimitMiddleware` with two independent rate-limiting stages in `NewProtectedHandlerWithLimiters`:

1. **Stage 1 — `PreAuthIPRateLimitMiddleware` (before `AuthenticateMiddleware`):**
   - Keys strictly by trusted peer IP (`ExtractPeerIPKey`), ignoring `Authorization`, `X-API-Key`, `X-Real-IP`, and untrusted `X-Forwarded-For` headers.
   - Configurable independently via `SENTINEL_PREAUTH_RATE_LIMIT_RPS` and `SENTINEL_PREAUTH_RATE_LIMIT_BURST` (defaulting to `SENTINEL_RATE_LIMIT_*`).
   - Drops excessive traffic with HTTP `429` + `Retry-After` **before** any credential database lookup occurs.

2. **Stage 2 — `AuthenticatedPrincipalRateLimitMiddleware` (after `AuthenticateMiddleware`, before `RequireAuthMiddleware`):**
   - Keys strictly by verified principal scoped to its tenant (`principal:<sha256(tenant_id + ":" + principal_id)>`).
   - Unauthenticated requests pass through without allocating principal buckets so `RequireAuthMiddleware` returns `401` or `503`.
   - Enforces per-principal quotas even if a caller rotates IPs or unverified headers, and isolates tenants that share a principal name.

## Consequences

### Positive
- Eliminates unauthenticated credential-verification floods against PostgreSQL.
- Preserves per-principal quota isolation for authenticated tenants.
- Separates Stage 1 IP metrics/buckets from Stage 2 principal metrics/buckets.

### Trade-offs & Limitations
- Authenticated clients behind a shared corporate NAT IP share the Stage 1 IP burst/rate limit; operators should size `SENTINEL_PREAUTH_RATE_LIMIT_*` higher than individual tenant quotas when multiple tenants share a NAT gateway.
- Both stages remain process-local in-memory token buckets.
