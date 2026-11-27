# ADR-013: Credential Expiration & Verification Cache Evaluation

## Status
Accepted

## Date
2026-11-27

## Context
Milestone 2D evaluated two credential lifecycle questions:
1. How to introduce optional API-key expiration (`expires_at`) without breaking existing credentials or weakening timing-safe verification.
2. Whether to introduce an in-memory bounded-TTL credential verification cache in `StoreVerifier` to reduce PostgreSQL `SELECT` load on `api_credentials`.

## Decision

### 1. Optional UTC Credential Expiration
- Added nullable `expires_at TIMESTAMPTZ NULL` via `migrations/003_add_credential_expiration.up.sql` with a partial index (`WHERE expires_at IS NOT NULL`).
- Existing credentials receive `NULL` (no automatic expiration), maintaining 100% backward compatibility.
- `StoreVerifier.Verify` evaluates `VerifySecretHash` in constant time **before** checking `RevokedAt` or `ExpiresAt` (`!now.UTC().Before(expiresAt.UTC())`), preventing timing side-channels from leaking credential status without a valid secret.

### 2. Verification Cache Evaluation: Design A vs. Design B

#### Design A: No Cache (Selected)
- Every valid-format `snt_<16hex>_<secret>` request executes one indexed `SELECT` on `api_credentials(key_id)` (1 row).
- Pre-auth IP rate limiting (`PreAuthIPRateLimitMiddleware`) already shields PostgreSQL from unauthenticated floods, and `ParseToken` rejects malformed tokens in memory.
- **Revocation Latency:** `0ms` (immediate on `UPDATE ... SET revoked_at = now()` commit under PostgreSQL `READ COMMITTED`).
- **Multi-Replica Consistency:** Inherently consistent across all `sentinel-api` replicas.
- **DB Outage Behavior:** Fails closed (`503 auth_unavailable`) rather than serving stale revoked/expired credentials from memory.

#### Design B: Bounded-TTL In-Memory Cache (Deferred)
- Caching `(key_id -> StoredCredential)` in memory with `MaxEntries = 10000` and `TTL = 5s–30s`.
- **Single-Replica Invalidation:** `Revoke()` can delete the local cache entry on the same process, but `sentinel-admin` runs as a **separate CLI process**, so `sentinel-api` would not receive local invalidation!
- **Multi-Replica Consistency:** Without Redis pub/sub or PostgreSQL `LISTEN/NOTIFY`, other replicas continue accepting revoked credentials for up to the full cache `TTL` (e.g., `5s–30s` revocation delay window).
- **Outage Behavior:** If cache entries outlive a DB outage or stale entries are served, revoked keys could be accepted.

### Verdict
Because Sentinel prioritizes **immediate revocation consistency** (`sentinel-admin` runs out-of-band as a separate process, and multi-replica deployments have no distributed invalidation bus in this milestone), **Design A (No Cache)** is retained. Caching is deferred until a shared invalidation mechanism (`LISTEN/NOTIFY` or Redis pub/sub) is justified by measured DB read saturation.

## Consequences
- Immediate revocation is preserved across single-node and multi-replica deployments.
- `AuthMetrics` tracks `db_lookup_count`, `db_lookup_errors`, and `avg_db_lookup_ms` so operators have empirical evidence if PostgreSQL lookup latency ever warrants Design B.
