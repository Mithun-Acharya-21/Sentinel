# Sentinel — Milestone 2C: Authentication Pipeline Hardening & Security Audit

**Date:** 2026-11-27  
**Branch:** `portfolio-transformation`

---

## 1. Initial Findings & Security Vulnerabilities Discovered

An independent source audit of Milestone 2B (`internal/httpapi/`, `internal/auth/`, `internal/storage/postgres/`, `cmd/sentinel-api/`) revealed two security/isolation issues:

1. **[HIGH] Unauthenticated Credential-Verification Flood Against PostgreSQL:**
   In Milestone 2B's `NewProtectedHandler`, `AuthenticateMiddleware` ran **before** `RateLimitMiddleware`. Any unauthenticated attacker sending syntactically valid but nonexistent `Authorization: Bearer snt_<16hex>_<secret>` tokens triggered `CredentialRepository.GetByKeyID` (a PostgreSQL `SELECT` query) on **every single request** before IP rate limiting could drop the traffic. From a single IP, an attacker could exhaust the PostgreSQL connection pool (`SetMaxOpenConns(10)`).
2. **[LOW/MEDIUM] Cross-Tenant Principal Bucket Key Collision:**
   `WithVerifiedPrincipal(ctx, principal.PrincipalID)` hashed only `principal.PrincipalID`. If two distinct tenants (`tenant-a` and `tenant-b`) both had a principal named `svc-ingest`, they would share a single Stage 2 rate-limit bucket.

All other Milestone 2B controls (constant-time SHA-256 secret comparison before revocation check, `snt_` token parsing, tenant-scoped `Repository.Get(ctx, tenantID, id)`, body/query tenant-substitution `403` rejection, and `/health/*` exemption) were verified intact.

---

## 2. Before-and-After Middleware Architecture

### Before (Milestone 2B)
```text
RequestMetadata -> AuthenticateMiddleware (DB lookup!) -> RateLimitMiddleware -> RequireAuth -> RequireScope -> Handler
```

### After (Milestone 2C)
```text
1. RequestMetadataMiddleware
2. PreAuthIPRateLimitMiddleware          (Stage 1: Peer IP only; drops floods before DB)
3. AuthenticateMiddleware                (Bearer verification -> auth.Principal on ctx)
4. AuthenticatedPrincipalRateLimitMiddleware (Stage 2: Tenant-scoped principal bucket)
5. RequireAuthMiddleware                 (401 / 503 for unauthenticated requests)
6. RequireScopeMiddleware                (403 if lacking incidents:read / incidents:write)
7. Incident Handlers                     (Tenant derived strictly from auth.Principal)
```
Health endpoints (`/health/live`, `/health/ready`) remain mounted directly on the root `ServeMux` outside the protected chain.

---

## 3. Two-Stage Rate-Limiting Design

- **Stage 1 (`PreAuthIPRateLimitMiddleware`):**
  - Uses `ExtractPeerIPKey(r)` (strictly peer IP from `RemoteAddr`, or `X-Forwarded-For` only when peer matches `SENTINEL_RATE_LIMIT_TRUSTED_PROXIES`). Never reads `Authorization`, `X-API-Key`, `X-Real-IP`, or context principals.
  - Configurable via `SENTINEL_PREAUTH_RATE_LIMIT_RPS` and `SENTINEL_PREAUTH_RATE_LIMIT_BURST` (defaults to `SENTINEL_RATE_LIMIT_RPS` / `SENTINEL_RATE_LIMIT_BURST`).
  - Bounded by `MaxBuckets = 10000` with `BucketTTL = 10m` and `CleanupInterval = 1m`.
- **Stage 2 (`AuthenticatedPrincipalRateLimitMiddleware`):**
  - Keys verified principals by `principal:<sha256(tenant_id + ":" + principal_id)>`.
  - Unauthenticated requests pass through without allocating Stage 2 buckets so `RequireAuthMiddleware` returns `401` or `503`.

---

## 4. Adversarial Test Cases Added

Added in `internal/httpapi/security_2c_test.go`:
- `TestPreAuthFloodProtectionStopsCredentialVerification`: Uses a `countingVerifier` to prove that once Stage 1 IP burst is exhausted, rotating nonexistent `snt_` tokens, malformed keys, missing `Authorization` headers, and spoofed `X-Forwarded-For` headers return `429` **with zero additional `Verify` calls**. Also verifies trusted-proxy `XFF` resolution.
- `TestAuthenticatedPrincipalRateLimitingTwoStage`: Proves Stage 2 enforces per-principal quotas across IP/header mutations and isolates `tenant-a:svc-shared` from `tenant-b:svc-shared`.
- `TestVerifierOutageReturns503AndFailsClosed`: Proves verifier/DB outages return `503 auth_unavailable` without invoking incident handlers.
- `TestConcurrencyTwoStageLimitersAndCredentialRevocation`: Concurrent workers hitting both limiters, `Cleanup()`, `Verify()`, and `Revoke()` simultaneously.

---

## 5. Verification Results

Executed after final code edits:
- `go build ./cmd/sentinel-api`: **PASS**
- `go build ./cmd/sentinel-admin`: **PASS**
- `gofmt -l .`: **PASS** (clean)
- `go test -count=1 ./...`: **PASS** (`internal/auth`, `internal/config`, `internal/evaluation`, `internal/evaluation/llmagent`, `internal/httpapi`, `internal/storage/postgres` unit tests)
- `go vet ./...`: **PASS**
- `git diff --check`: **PASS**
- **Environmental Blockers:**
  - `go test -race`: Blocked on this Windows host (`cgo: C compiler "gcc" not found`).
  - Live PostgreSQL integration tests (`SENTINEL_TEST_DATABASE_URL`): Skipped (PostgreSQL/Docker not running in this host sandbox).

---

## 6. Remaining Limitations & Production Deployment Considerations

1. **Process-Local Buckets:** Stage 1 and Stage 2 limiters are in-memory per process; multi-replica deployments scale effective limits by replica count unless fronted by an edge/distributed limiter.
2. **NAT Sizing:** Multiple authenticated tenants behind one NAT IP share Stage 1 IP burst/rate; configure `SENTINEL_PREAUTH_RATE_LIMIT_*` higher than individual tenant quotas in shared-NAT environments.
3. **Credential Expiration & Caching (Future Milestone):** Credentials currently have no `expires_at` TTL and perform one indexed `SELECT` per valid-format `snt_` request. A future milestone (Milestone 2D) should add `expires_at` support and evaluate a short-TTL verification cache with revocation invalidation.
