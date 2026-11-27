# Sentinel — Milestone 2B: Authentication, Trusted Identity & Tenant Authorization

**Date:** 2026-11-26  
**Branch:** `portfolio-transformation`

---

## 1. Architecture Before & After

### Before Milestone 2B
- `cmd/sentinel-api` exposed `POST /v1/incidents` and `GET /v1/incidents/{id}` behind `RateLimitMiddleware` with no authentication.
- `POST /v1/incidents` trusted `tenant_id` directly from the client's JSON payload.
- `GET /v1/incidents/{id}` called `postgres.Repository.Get(ctx, id)` (`WHERE id = $1::uuid`) with **no** `tenant_id` predicate, allowing cross-tenant reads by UUID.
- `WithVerifiedPrincipal` existed in `internal/httpapi/ratelimit.go` but had no authentication middleware populating it.

### After Milestone 2B
- **Authentication Layer (`internal/auth`, `internal/storage/postgres/credentials.go`):** Opaque Bearer API keys (`snt_<key_id>_<secret>`) verified against SHA-256 hashes in PostgreSQL (`api_credentials` table via `migrations/002_create_api_credentials.up.sql`) using `crypto/subtle.ConstantTimeCompare`.
- **Trusted Principal & Rate-Limit Integration (`internal/httpapi/auth.go`):** `AuthenticateMiddleware` verifies credentials and populates both `auth.Principal` and `httpapi.WithVerifiedPrincipal` on `context.Context`.
- **Tenant Isolation (`internal/incident/repository.go`, `internal/storage/postgres/incidents.go`, `internal/httpapi/incidents.go`):**
  - `POST /v1/incidents` derives `TenantID` exclusively from `principal.TenantID` and rejects mismatched `tenant_id` in body or query parameters with `403 Forbidden`.
  - `GET /v1/incidents/{id}` requires `principal.TenantID` in `Repository.Get(ctx, tenantID, id)` and executes `WHERE id = $1::uuid AND tenant_id = $2`.
- **Controlled Provisioning (`cmd/sentinel-admin/main.go`):** Out-of-band CLI for `create-key` and `revoke-key`; no self-registration HTTP endpoint.

---

## 2. Threat Model & Threats Addressed

| Threat | Mitigation |
|---|---|
| Unauthenticated API access | `RequireAuthMiddleware` + fail-closed handler principal check (`401 Unauthorized`). |
| Tenant ID substitution (body / query) | `createIncident` and `getIncident` reject mismatched `tenant_id` with `403 Forbidden` and derive `TenantID` from `auth.Principal`. |
| Cross-tenant IDOR on `GET /v1/incidents/{id}` | `Repository.Get(ctx, tenantID, id)` enforces `AND tenant_id = $2` in SQL, returning `404 Not Found` on mismatch. |
| Plaintext secret exposure in DB / logs | Only SHA-256 hash (`secret_hash`) is persisted; `PlaintextToken` has `json:"-"` and is printed once by `sentinel-admin`. |
| Timing side-channel on secret comparison | `auth.VerifySecretHash` uses `crypto/subtle.ConstantTimeCompare` before checking `revoked_at`. |
| Unauthenticated brute-force / DoS | `RateLimitMiddleware` runs after `AuthenticateMiddleware` resolution and throttles unauthenticated peers by `ip:<peer_ip>`, while malformed tokens fail in `ParseToken` with zero DB queries. |
| Duplicate rate-limit buckets per client | Authenticated requests carry `WithVerifiedPrincipal`, creating exactly one `principal:<hash>` bucket regardless of client IP. |
| Missing authorization scopes | `RequireScopeMiddleware` enforces `incidents:write` on `POST` and `incidents:read` on `GET` (`403 Forbidden`). |

---

## 3. SQL Tenant Isolation Review

- `postgres.Repository.Create`: Uses `input.TenantID` (authoritatively set to `principal.TenantID` by `createIncident`) in `INSERT INTO incidents (...)` and `ON CONFLICT (tenant_id, source, source_event_id)` as well as `findByEventKey(ctx, input.TenantID, ...)`.
- `postgres.Repository.Get`: Updated to `SELECT ... FROM incidents WHERE id = $1::uuid AND tenant_id = $2` and rejects empty `tenantID` before querying.
- `postgres.CredentialRepository`: `GetByKeyID`, `Create`, and `Revoke` use parameterized SQL with `api_credentials`.

---

## 4. Verification Summary

- `gofmt -l .`: Exit code `0` (clean).
- `go test -count=1 ./...`: Exit code `0` (`internal/auth`, `internal/config`, `internal/evaluation`, `internal/evaluation/llmagent`, `internal/httpapi`, `internal/storage/postgres` unit tests all pass).
- `go vet ./...`: Exit code `0`.
- `git diff --check`: Exit code `0`.
- **PostgreSQL Integration Tests:** `internal/storage/postgres` integration tests (`TestRepositoryCreateReplayConflictAndGet`, `TestCredentialRepositoryCreateVerifyAndRevoke`) skipped because `SENTINEL_TEST_DATABASE_URL` / Docker PostgreSQL is not running in this host sandbox.
- **Race Detector:** `go test -race` is blocked on this Windows host because `gcc` is not installed (`cgo: C compiler "gcc" not found`).

---

## 5. Remaining Limitations & Future IdP Integration

1. **No Automatic Credential Expiration:** `api_credentials` supports explicit `revoked_at` revocation; automatic `expires_at` TTLs are not yet implemented.
2. **Synchronous DB Lookup per Authenticated Request:** Each valid-format `snt_` token performs one indexed `SELECT` on `api_credentials`. A bounded-TTL verification cache can be added inside `StoreVerifier` if high QPS requires it.
3. **Future External IdP (OIDC / JWT):** `AuthenticateMiddleware` depends only on `auth.CredentialVerifier`, allowing a JWT/OIDC verifier to be swapped in without modifying handlers or rate limiting.
