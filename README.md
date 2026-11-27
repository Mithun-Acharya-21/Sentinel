# Sentinel — Evidence-Driven AI Incident Response & Evaluation Platform

Sentinel is a Go and PostgreSQL backend platform for **durable asynchronous incident intake, LLM-assisted triage, explicit human approval, controlled remediation execution, and independent post-action verification**.

It separates incident acceptance from slow AI/remediation work, enforces strict tenant isolation and two-stage rate limiting, and prevents LLMs from executing infrastructure changes autonomously.

---

## 1. Problem Statement

Production incident-response systems face three coupled engineering problems:
1. **Unreliable Intake & Duplicate Events:** Alerting webhooks retry under network failures; intake must be atomic and idempotent.
2. **Unsafe AI Automation:** Letting an LLM invoke infrastructure tools directly risks unverified or destructive mutations, prompt injection, and tenant-boundary escapes.
3. **Silent Worker Failures:** In-memory job queues lose work on restarts, and naive background workers suffer from duplicate execution, stale-worker overwrites, and unbounded retries.

Sentinel addresses these with a PostgreSQL-backed durable queue (`FOR UPDATE SKIP LOCKED` + `lease_token` fencing), zero-tool structured LLM triage, an explicit human approval state machine, and an allowlisted simulated remediation engine verified independently against system state.

---

## 2. Key Engineering Features

- **Tenant-Scoped API-Key Authentication:** Opaque `snt_<key_id>_<secret>` Bearer keys, SHA-256 secret hashing, `crypto/subtle.ConstantTimeCompare`, revocation, UTC expiration (`expires_at`), and out-of-band CLI provisioning (`cmd/sentinel-admin`).
- **Two-Stage Token-Bucket Rate Limiting:** Pre-auth peer IP limiter (drops floods before DB lookups) + post-auth tenant-scoped principal limiter, with trusted-proxy `X-Forwarded-For` traversal and bounded `MaxBuckets`.
- **Atomic Intake & Durable Worker Queue:** Single-transaction `incidents` + `incident_jobs` insertion, `FOR UPDATE SKIP LOCKED` claiming, `lease_token` fencing, exponential backoff with jitter, `dead_letter` transitions, panic recovery, and graceful shutdown.
- **Zero-Tool LLM Triage (`internal/triage`):** Reuses `llmagent.Client` (`HTTPClient` with bounded retries and `Retry-After` handling), enforces strict JSON schema validation, and separates evidence from inference.
- **Human Approval & Controlled Remediation (`internal/remediation`):** Separation of duties (`incidents:write` vs `incidents:approve` vs `incidents:execute`), allowlisted actions only, atomic `execution_token` claims, deterministic `Simulator`, and independent `VerifyPostAction`.
- **Prometheus-Compatible Observability:** Zero-dependency `GET /metrics` (`0.0.4`), `/health/live`, `/health/ready` (with draining state), and `sql.DB.Stats()` pool telemetry.

---

## 3. Architecture & Diagrams

### Overall System Architecture
```mermaid
flowchart LR
    Client[HTTP Client] --> MW[Middleware Chain<br/>RequestID → Metrics → PreAuth IP Limit → Auth → Principal Limit → Scope]
    MW --> API[httpapi Handlers]
    API -->|Atomic TX| PG[(PostgreSQL<br/>incidents, incident_jobs,<br/>api_credentials, triage, remediation)]
    Workers[worker.Engine Pool<br/>SKIP LOCKED + lease_token] -->|Claim / Complete| PG
    Workers --> Triage[triage.Processor<br/>Zero-Tool LLM Request]
    Triage --> LLM[llmagent.Client<br/>OpenAI-compatible / Mock]
    API -->|incidents:execute| Exec[remediation.Executor]
    Exec --> Sim[remediation.Simulator<br/>Process-Local State]
    Exec --> Verifier[VerifyPostAction]
    Verifier -->|Inspects Snapshot| Sim
```

### Incident Processing Lifecycle
```mermaid
sequenceDiagram
    participant C as Client
    participant A as sentinel-api
    participant DB as PostgreSQL
    participant W as worker.Engine
    participant L as LLM Provider
    participant H as Human Approver

    C->>A: POST /v1/incidents (Bearer key)
    A->>DB: TX: INSERT incidents + INSERT incident_jobs (pending)
    A-->>C: 201 Created
    W->>DB: ClaimNext (FOR UPDATE SKIP LOCKED + lease_token)
    W->>L: Complete(Tools=nil, JSON schema)
    L-->>W: Structured Triage JSON
    W->>DB: TX: SaveTriageAndApproval (pending_review) + Complete(lease_token)
    H->>A: POST /v1/incidents/{id}/triage/approve
    A->>DB: UPDATE incident_triage_approvals (approved)
    C->>A: POST /v1/incidents/{id}/remediate
    A->>DB: ClaimExecution (execution_token)
    A->>A: Simulator.Apply + VerifyPostAction
    A->>DB: CompleteExecution (succeeded / failed / verification_failed)
```

### Worker Job State Machine
```mermaid
stateDiagram-v2
    [*] --> pending : Atomic Incident Intake
    pending --> processing : ClaimNext (lease_token)
    failed --> processing : ClaimNext (available_at <= now)
    processing --> completed : Complete (lease_token match)
    processing --> failed : Retryable Error & attempts < max
    processing --> dead_letter : NonRetryableError or max attempts
    processing --> processing : ReclaimExpired (new lease_token)
    processing --> pending : Graceful Shutdown Release
```

### Triage & Human Approval Workflow
```mermaid
stateDiagram-v2
    [*] --> pending_review : Triage Saved
    pending_review --> approved : Human Approves (incidents:approve)
    pending_review --> rejected : Human Rejects (incidents:approve)
    pending_review --> expired : now >= expires_at
    approved --> [*] : Eligible for Remediation
    rejected --> [*] : Execution Blocked (403)
    expired --> [*] : Execution Blocked (409)
```

### Remediation Execution State Machine
```mermaid
stateDiagram-v2
    [*] --> executing : ClaimExecution (execution_token)
    executing --> succeeded : Apply OK & VerifyPostAction OK
    executing --> failed : Apply Error (e.g. precondition_failed)
    executing --> verification_failed : Apply OK & VerifyPostAction Failed
    failed --> executing : Retry ClaimExecution
    verification_failed --> executing : Retry ClaimExecution
    executing --> executing : Expired Lease Reclaim
    succeeded --> [*] : Idempotent Replay Returns Existing
```

---

## 4. Technology Stack

- **Language:** Go `1.25.0` (standard library `net/http`, `database/sql`, `log/slog`, `crypto/*`, `sync/*`)
- **Database:** PostgreSQL 16 (`github.com/jackc/pgx/v5/stdlib` + versioned SQL migrations)
- **CI:** GitHub Actions (`ubuntu-latest`, `postgres:16-alpine`, `CGO_ENABLED=1 go test -race -count=1 ./...`)

---

## 5. Security & Reliability Architecture

- **Authentication & Tenant Isolation:** Bearer API keys (`snt_<16-hex>_<secret>`) are hashed with SHA-256 and verified via `subtle.ConstantTimeCompare`. `tenant_id` is derived exclusively from the verified `auth.Principal`; mismatched body or query `tenant_id` values return `403`, and cross-tenant `GET` requests return `404`.
- **Two-Stage Rate Limiting:** Stage 1 (`PreAuthIPRateLimitMiddleware`) throttles unauthenticated floods by trusted peer IP before PostgreSQL is queried; Stage 2 (`AuthenticatedPrincipalRateLimitMiddleware`) enforces tenant-scoped principal quotas.
- **At-Least-Once Processing & Fencing:** Workers release DB connections before calling the LLM or simulator. Every claim issues a UUID `lease_token` / `execution_token` so stale workers receive `ErrLeaseLost` / `ErrExecutionLeaseLost` instead of overwriting newer results.
- **Independent Verification:** `triage.VerifyProposal` and `remediation.VerifyPostAction` separate recommendation, approval, execution, verification, and safety outcomes.

---

## 6. Local Installation, Migrations & Demo

### Prerequisites
- Go `1.25+`
- Docker Desktop (for local PostgreSQL)

### Run End-to-End Deterministic Demo (No Paid LLM Key Required)
```powershell
./scripts/demo-e2e.ps1
```
This script starts PostgreSQL, applies migrations `001`–`006`, runs `cmd/sentinel-mock-llm` and `cmd/sentinel-api`, provisions a scoped API key with `sentinel-admin`, submits an incident, retrieves LLM triage, approves it, executes simulated remediation, inspects verification, and prints `/metrics`.

### Manual Service Startup
```powershell
docker compose up -d db
.\scripts\migrate-up.ps1
Copy-Item .env.example .env
$env:SENTINEL_DATABASE_URL = "postgres://sentinel:sentinel-local-only@127.0.0.1:5435/sentinel?sslmode=disable"
go run ./cmd/sentinel-admin create-key -tenant tenant-demo -principal human-demo -scopes "incidents:read,incidents:write,incidents:approve,incidents:execute"
go run ./cmd/sentinel-api
```

---

## 7. Testing & Verification

```powershell
gofmt -l .
go vet ./...
go test -count=1 ./...
```
PostgreSQL integration tests run when `SENTINEL_TEST_DATABASE_URL` is set (automatically executed with `-race` in `.github/workflows/ci.yml`):
```powershell
$env:SENTINEL_TEST_DATABASE_URL = "postgres://sentinel:sentinel-local-only@127.0.0.1:5435/sentinel?sslmode=disable"
go test -v -count=1 ./internal/storage/postgres/...
```

---

## 8. Documentation Index

- [API Reference](docs/api-reference.md)
- [Engineering Evidence Matrix](docs/engineering-evidence.md)
- [Observability Guide](docs/observability.md) & [Operations Runbook](docs/operations-runbook.md)
- [Remediation Architecture](docs/remediation-architecture.md), [Security](docs/remediation-security.md), & [Runbook](docs/remediation-runbook.md)
- [Architecture Decision Records](docs/engineering_decisions/) (`ADR-001` through `ADR-017`)

---

## 9. Known Limitations & Roadmap

- **Known Limitations:** Rate-limiter buckets and `remediation.Simulator` state are process-local; at-least-once delivery has a narrow crash window between external/simulated side effects and DB completion commits (mitigated by idempotency keys); `dead_letter` job replay currently uses SQL per the runbook.
- **Future Roadmap:** Distributed rate-limit/cache invalidation, automatic `lease_expires_at` heartbeats for long-running jobs, and dead-letter replay CLI commands.
