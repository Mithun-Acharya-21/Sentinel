# ADR-010: Opaque Bearer API-Key Authentication & Credential Storage

## Status
Accepted

## Date
2026-11-26

## Context
Prior to Milestone 2B, Sentinel's incident API (`cmd/sentinel-api`) had no authentication boundary. While Milestone 2A prevented unverified `Authorization` and `X-API-Key` headers from bypassing the rate limiter, callers could still invoke `POST /v1/incidents` and `GET /v1/incidents/{id}` without verified credentials.

Sentinel needed an authentication mechanism that:
1. Establishes a verified caller identity without introducing third-party JWT/OIDC dependencies or external identity infrastructure.
2. Supports immediate credential revocation.
3. Never stores plaintext secrets in PostgreSQL or logs them.
4. Decouples HTTP middleware from the backing credential verifier so an external IdP (OIDC/JWT) can be integrated in the future without rewriting handlers.

## Decision

### 1. Opaque Bearer API Keys vs. JWTs
We chose **opaque Bearer API keys** (`snt_<16-hex-key-id>_<base64url-secret>`) backed by PostgreSQL over stateless JWTs:
- **Instant Revocation:** Setting `revoked_at` in PostgreSQL immediately invalidates a compromised credential on the next request. Stateless JWTs cannot be revoked before expiry without maintaining a distributed revocation list.
- **Zero Signing-Key / Algorithm-Confusion Surface:** No `alg=none`, RS256/HS256 key-confusion, or JWKS rotation complexity.
- **O(1) Format Rejection & Indexed Lookup:** The `snt_` prefix and 16-hex `key_id` allow malformed tokens to be rejected in memory before touching PostgreSQL, and valid-format tokens perform a single indexed lookup on `api_credentials(key_id)`.

### 2. Cryptographic Storage & Constant-Time Verification
- Secrets are generated with 256 bits of entropy from `crypto/rand`.
- PostgreSQL (`migrations/002_create_api_credentials.up.sql`) stores only `key_id` and `secret_hash` (hex-encoded SHA-256 of the random secret). Because the secret has 256 bits of cryptographic randomness (not a low-entropy user password), SHA-256 is preimage-resistant without slow KDF latency on every API request.
- `auth.VerifySecretHash` compares the computed SHA-256 digest against the decoded stored hash using `crypto/subtle.ConstantTimeCompare` before checking `revoked_at`, preventing timing side-channel leaks.

### 3. Verifier Abstraction (`auth.CredentialVerifier`)
`httpapi.AuthenticateMiddleware` depends only on the `auth.CredentialVerifier` interface (`Verify(ctx, token) (Principal, error)`). Replacing or augmenting `StoreVerifier` with an OIDC/JWT verifier in a future milestone requires no changes to `incidents.go` or `ratelimit.go`.

### 4. Controlled Provisioning (`cmd/sentinel-admin`)
No self-registration or unauthenticated key-creation HTTP endpoint is exposed. Operators provision and revoke credentials out-of-band via `cmd/sentinel-admin` (`create-key`, `revoke-key`), which displays the plaintext token **once** at creation time and never logs it.

## Consequences

### Positive
- Immediate, auditable credential revocation in PostgreSQL.
- No plaintext secrets persisted in the database or application logs.
- Clean separation between `CredentialVerifier` and HTTP handlers.

### Trade-offs & Limitations
- Every request bearing a well-formed `snt_` token performs a PostgreSQL lookup on `api_credentials`. Under very high QPS, a short-TTL in-memory verification cache (invalidated on revocation or bounded to seconds) may be needed to reduce database read load.
- Credentials do not currently carry an automatic `expires_at` column; rotation relies on explicit `revoke-key` and `create-key`.
