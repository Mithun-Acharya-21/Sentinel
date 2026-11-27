# ADR-002: PostgreSQL Idempotency & Concurrency Arbitration

## Decision
We enforce incident event idempotency at the database storage engine level using a compound uniqueness constraint `UNIQUE (tenant_id, source, source_event_id)` and an atomic `INSERT ... ON CONFLICT DO NOTHING RETURNING` pattern, backed by deep payload equality verification.

## Context
In distributed incident management, monitoring systems, webhook emitters (e.g. PagerDuty, Datadog), and network proxies routinely retransmit requests during transient network timeouts or client restarts. If an incident ingestion endpoint processes duplicate events non-idempotently, it creates phantom alerts, corrupts metrics, and triggers duplicate automated remediation workflows.

## Constraints
- High-concurrency intake: Multiple worker nodes or concurrent webhooks can submit identical event keys simultaneously.
- Zero data loss: Valid new events must never be dropped.
- Tamper/Conflict Detection: A retransmitted request with the same event ID but *differing* payload content must be detected and rejected as a conflict, not blindly swallowed or overwritten.

## Alternatives Considered
1. **Application-Level Check-Then-Insert (`SELECT` then `INSERT`)**:
   - *Pros*: Simple to write.
   - *Cons*: **Critically unsafe under concurrency**. Two concurrent requests with the same key will both execute the `SELECT`, see no row, and both attempt to insert. One will fail with an unhandled unique violation or cause race conditions.
2. **Distributed Cache Lock (Redis Redlock)**:
   - *Pros*: Blocks requests before reaching the database.
   - *Cons*: Introduces external infrastructure dependency, cache invalidation complexity, clock drift failure modes, and potential split-brain inconsistency.
3. **Selected Approach: PostgreSQL Atomic Constraint Arbitration with Payload Comparison**:
   - The database constraint is the single source of truth.
   - Executes `INSERT INTO incidents ... ON CONFLICT (tenant_id, source, source_event_id) DO NOTHING RETURNING ...`.
   - If a row is returned: Insert won (`201 Created`).
   - If no row is returned: Another transaction won or earlier record exists. Sentinel performs a context-aware read and compares all payload fields.
   - Exact match: Clean replay (`200 OK`).
   - Mismatch: Semantic conflict (`409 Conflict`).

## Why this approach?
- **Mathematical Correctness**: PostgreSQL row-level locks and unique B-tree indexes guarantee strict serializability for competing inserts without requiring explicit table locks.
- **Fail-Safe Conflict Detection**: Distinguishes benign network retries from dangerous key reuse or data corruption.
- **Timestamp Normalization**: Reconciles Go runtime nanosecond precision with PostgreSQL microsecond storage precision before comparison to eliminate false-positive conflicts.

## Why not the alternatives?
- Check-then-insert is fundamentally broken in concurrent systems.
- Redis introduces network failure modes without eliminating the need for database uniqueness enforcement.

## Trade-offs
- **Gains**: Rock-solid concurrency safety, zero duplicate rows, zero race windows.
- **Costs**: Replay requests require a secondary `SELECT` to compare payload fields, adding one database roundtrip for duplicates.

## Failure Scenarios
- If two requests arrive at the exact same microsecond: PostgreSQL serializes the index insertion; one transaction inserts and returns the row, the second waits, receives nothing from the insert, reads the committed row, and returns `200 OK`.

## Security Implications
Prevents duplicate injection attacks and ensures audit trails in PostgreSQL cannot be overwritten by malicious payload tampering under an existing event ID.

## Testing Strategy
- Unit tests with fakes: `internal/httpapi/incidents_test.go`
- Concurrent integration tests: `internal/storage/postgres/incidents_integration_test.go` (`TestConcurrentDuplicateInserts` spawns concurrent goroutines competing for the same idempotency key).

## Evidence
- `migrations/001_create_incidents.up.sql`
- `internal/storage/postgres/incidents.go`
- `internal/storage/postgres/incidents_integration_test.go`

## Status
Implemented and verified.
