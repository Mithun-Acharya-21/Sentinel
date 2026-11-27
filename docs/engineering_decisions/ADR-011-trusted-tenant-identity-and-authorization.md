# ADR-011: Trusted Tenant Identity, Middleware Ordering & Scope Authorization

## Status
Accepted

## Date
2026-11-26

## Context
In Sentinel's initial API implementation:
1. `POST /v1/incidents` trusted `tenant_id` directly from the client's JSON request body.
2. `GET /v1/incidents/{id}` called `incident.Repository.Get(ctx, id)` with no `tenant_id` predicate, allowing any caller with a UUID to read any tenant's incidents.
3. `RateLimitMiddleware` needed to key authenticated clients by their verified principal (`WithVerifiedPrincipal`) while still protecting unauthenticated endpoints by peer IP — without creating duplicate buckets for a single authenticated client.

## Decision

### 1. Authoritative Principal & Tenant Derivation
- `auth.Principal` (`PrincipalID`, `TenantID`, `CredentialID`, `Scopes`, `AuthMethod`) is attached to `context.Context` **only** by `AuthenticateMiddleware` after `CredentialVerifier.Verify` succeeds.
- `POST /v1/incidents` and `GET /v1/incidents/{id}` fail closed (`401 Unauthorized`) if no `Principal` exists in context.
- **Tenant Substitution Prevention:** If a client supplies `tenant_id` in the JSON body or `?tenant_id=` query parameter that differs from `principal.TenantID`, the API rejects the request with `403 Forbidden`. Otherwise, `input.TenantID` is overwritten with `principal.TenantID`.
- **Tenant-Scoped Repository:** `incident.Repository.Get` signature was updated to `Get(ctx context.Context, tenantID, id string) (Incident, error)` and `postgres.Repository.Get` enforces `WHERE id = $1::uuid AND tenant_id = $2`. Cross-tenant `GET` requests return `404 Not Found` (`not_found`) to avoid existence-oracle leaks.

### 2. Scope-Based Authorization
Routes enforce explicit scopes via `RequireScopeMiddleware`:
- `POST /v1/incidents` requires `incidents:write`.
- `GET /v1/incidents/{id}` requires `incidents:read`.
Authenticated principals lacking the required scope receive `403 Forbidden` (`{"error":{"code":"forbidden","message":"..."}}`).

### 3. Middleware Execution Order
`httpapi.NewProtectedHandler` applies middleware in the following order:
1. `RequestMetadataMiddleware`: Assigns/sanitizes `X-Request-ID`.
2. `AuthenticateMiddleware`: Verifies `Bearer snt_...` credentials and populates `auth.Principal` + `httpapi.WithVerifiedPrincipal`, recording verification errors on context **without rejecting yet**.
3. `RateLimitMiddleware`: Uses `principal:<hash>` (1 bucket) for authenticated principals, or `ip:<trusted_peer_ip>` for unauthenticated/invalid requests.
4. `RequireAuthMiddleware`: Returns `401 Unauthorized` (or `503 Service Unavailable` if verifier is down) for unauthenticated requests that were not already throttled.
5. `RequireScopeMiddleware` + Handler: Enforces scopes and tenant isolation.

Health endpoints (`/health/live`, `/health/ready`) remain mounted directly on the root mux outside this chain.

## Consequences

### Positive
- Eliminates client-controlled tenant spoofing and cross-tenant IDOR on `GET /v1/incidents/{id}`.
- Each authenticated client consumes exactly **one** rate-limit bucket (`principal:<hash>`), while unauthenticated brute-force traffic is throttled by peer IP before `401` responses.

### Trade-offs & Limitations
- Updating `incident.Repository.Get` is a deliberate internal interface change requiring callers to supply `tenantID`.
- Well-formed invalid `snt_` tokens still trigger a `GetByKeyID` lookup before rate limiting rejects the peer IP; malformed tokens without the `snt_<16hex>_<secret>` structure are rejected in memory with zero DB calls.
