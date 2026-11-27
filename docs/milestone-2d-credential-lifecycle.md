# Sentinel — Milestone 2D: Credential Lifecycle, Revocation Consistency & Database Reliability

**Date:** 2026-11-27  
**Branch:** `portfolio-transformation`

---

## 1. Initial Findings (Phase A Audit)

1. **Non-Idempotent Revocation:** `postgres.CredentialRepository.Revoke` executed `UPDATE api_credentials SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL` and returned `auth.ErrCredentialNotFound` on `RowsAffected() == 0`, causing repeated revocation calls on an already-revoked key to fail as "not found".
2. **Missing Credential Expiration:** `api_credentials` had no `expires_at` column, and `StoreVerifier` had no injectable clock for deterministic expiration testing.
3. **Duplicate Key-ID Mapping:** `CredentialRepository.Create` mapped PostgreSQL unique-constraint (`23505`) errors to `ErrStoreUnavailable` instead of `ErrDuplicateKeyID`.
4. **Missing Lifecycle Observability:** No counters or lookup latency metrics existed for auth successes, rejections, revoked/expired rejections, or DB lookup errors.
5. **Missing CI Race & PostgreSQL Workflow:** No `.github/workflows/ci.yml` existed to run `CGO_ENABLED=1 go test -race -count=1 ./...` and PostgreSQL integration tests on Ubuntu.

---

## 2. Credential Expiration Model

- **Schema:** `migrations/003_add_credential_expiration.up.sql` adds `expires_at TIMESTAMPTZ NULL` and a partial index `idx_api_credentials_expires_at` (`WHERE expires_at IS NOT NULL`).
- **Backward Compatibility:** Existing credentials receive `NULL` (`expires_at`), meaning no automatic expiration.
- **Clock & Evaluation Policy:** All timestamps are normalized to UTC. `StoreVerifier.Verify` checks `VerifySecretHash` in constant time **first**, then `stored.RevokedAt != nil` (`ErrRevokedCredentials`), then `stored.ExpiresAt != nil && !v.clock().UTC().Before(stored.ExpiresAt.UTC())` (`ErrExpiredCredentials`).
- **CLI Support:** `cmd/sentinel-admin create-key` supports optional `-expires-in <duration>` and `-expires-at <RFC3339>` flags while keeping existing invocations backward compatible.

---

## 3. Revocation Consistency Guarantees

- `postgres.CredentialRepository.Revoke` now uses an atomic CTE (`WITH target AS (...), updated AS (...) SELECT EXISTS(...)`) to distinguish:
  - Unknown `key_id` $\rightarrow$ `auth.ErrCredentialNotFound`.
  - Active `key_id` $\rightarrow$ sets `revoked_at = now()`, returns `nil`.
  - Already-revoked `key_id` $\rightarrow$ idempotent `nil`.
  - Database/context failure $\rightarrow$ `auth.ErrStoreUnavailable` (never reported as successful).
- `StoreVerifier.Verify` maps `ErrCredentialNotFound` $\rightarrow$ `ErrInvalidCredentials` (`401`), and all store/context/timeout errors $\rightarrow$ `ErrVerifierUnavailable` (`503 auth_unavailable`).

---

## 4. Migration Strategy

- `migrations/003_add_credential_expiration.up.sql` / `.down.sql` follow the `scripts/migrate-up.ps1` `<version>_<name>.up.sql` convention without altering `001` or `002`.
- Rollback (`003_add_credential_expiration.down.sql`) drops `idx_api_credentials_expires_at` and `expires_at`.

---

## 5. Cache Evaluation & Decision

See [ADR-013](engineering_decisions/ADR-013-credential-expiration-and-verification-cache-evaluation.md).  
Because `cmd/sentinel-admin` revokes credentials out-of-band as a separate OS process and multi-replica deployments lack a shared invalidation bus, **Design A (No Cache)** is retained to preserve `0ms` immediate revocation semantics.

---

## 6. PostgreSQL Integration-Test Architecture

Expanded `internal/storage/postgres/`:
- `credentials_integration_test.go`: Creation, verification, invalid secret, duplicate `key_id` (`ErrDuplicateKeyID`), revocation, idempotent repeated revocation, expiration, and concurrent `Verify` + `Revoke`.
- `incidents_integration_test.go`: Tenant-scoped create/get, cross-tenant `ErrNotFound`, idempotent replay, conflict, and concurrent creates.
- `failures_integration_test.go`: Canceled context, deadline exceeded, closed DB pool, pool exhaustion (`SetMaxOpenConns(1)`), and transaction rollback verification.
- All integration tests require `SENTINEL_TEST_DATABASE_URL`, generate isolated timestamped `tenant_id`s, and clean up their own rows via `t.Cleanup`.

---

## 7. CI Race-Detector Configuration

Created `.github/workflows/ci.yml` on `ubuntu-latest` with two jobs:
1. `unit-and-race`: Checks `gofmt -l .`, `git diff --check`, `go vet ./...`, builds `cmd/sentinel-api`, `cmd/sentinel-admin`, `cmd/sentinel-eval`, runs `go test -count=1 ./...`, and runs `CGO_ENABLED=1 go test -race -count=1 ./...`.
2. `postgres-integration`: Starts `postgres:16-alpine` with `pg_isready` health check, applies `migrations/*.up.sql`, and runs `CGO_ENABLED=1 go test -v -race -count=1 ./internal/storage/postgres/...` against `SENTINEL_TEST_DATABASE_URL`.

---

## 8. Observability Changes

Added `auth.AuthMetrics` (`StoreVerifier.Metrics()`) using bounded-cardinality `sync/atomic` counters:
- `auth_successes`, `auth_rejections`, `revoked_rejections`, `expired_rejections`
- `db_lookup_count`, `db_lookup_errors`, `avg_db_lookup_ms`
- `revocation_successes`, `revocation_failures`

No plaintext secrets, secret hashes, raw tokens, or unbounded tenant labels are exposed.

---

## 9. Remaining Limitations

1. Local Windows host lacks `gcc` (blocks `-race` locally) and Docker/PostgreSQL (integration tests skipped locally; executed by CI).
2. `AuthMetrics` is in-memory per process; Prometheus/OpenTelemetry export can be wired in a future observability milestone if needed.
