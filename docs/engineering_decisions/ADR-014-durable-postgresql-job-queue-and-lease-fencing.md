# ADR-014: Durable PostgreSQL Job Queue, `SKIP LOCKED` & Lease Fencing

## Status
Accepted

## Date
2026-11-27

## Context
Sentinel needed to decouple incident intake (`POST /v1/incidents`) from downstream incident processing so the API returns immediately after durable persistence while background workers execute processing reliably across restarts and worker crashes—without introducing Kafka, Redis, or another message broker.

## Decision

### 1. Atomic Intake + Outbox Job Creation
`postgres.Repository.Create` wraps incident insertion and `incident_jobs` insertion in a single PostgreSQL transaction:
- New incidents atomically insert one `pending` row in `incident_jobs` (`UNIQUE (incident_id)`).
- If job insertion fails, the incident transaction rolls back.
- Idempotent replays (`ON CONFLICT (tenant_id, source, source_event_id) DO NOTHING`) commit without creating duplicate jobs.

### 2. `FOR UPDATE SKIP LOCKED` + Lease Fencing (`lease_token`)
- Workers claim ready jobs (`status IN ('pending', 'failed') AND available_at <= now`) and expired leases (`status = 'processing' AND lease_expires_at <= now`) using `FOR UPDATE SKIP LOCKED` inside short transactions, then release the DB connection **before** running `Processor.Process`.
- Every claim/reclaim generates a fresh UUID `lease_token`.
- All state transitions (`Complete`, `Fail`, `Release`) include `WHERE id = $1::uuid AND status = 'processing' AND lease_token = $2::uuid`. If `RowsAffected() == 0`, the store returns `worker.ErrLeaseLost`, preventing a stale worker from overwriting a newer worker's result.

### 3. At-Least-Once Execution with Idempotent Side-Effect Ledger
Because a worker can crash *after* executing an external side effect but *before* committing `Complete`, Sentinel provides at-least-once job execution paired with `incident_job_effects` (`idempotency_key PRIMARY KEY` + `ON CONFLICT DO NOTHING`) so downstream steps deduplicate repeated executions.

## Consequences
- Zero external broker dependencies; PostgreSQL remains the single source of truth.
- Bounded worker concurrency (`SENTINEL_WORKER_COUNT`, default `2`) preserves connection pool headroom for HTTP intake.
- At-least-once delivery semantics require all custom `Processor` implementations to be idempotent.
