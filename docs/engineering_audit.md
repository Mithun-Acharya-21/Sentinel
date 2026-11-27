# Sentinel — Engineering Audit (Phase 0 Baseline)

Date: 2026-11-26  
Branch: `portfolio-transformation`  
Repository: https://github.com/Mithun-Acharya-21/Sentinel  
Auditors: Principal AI Engineer & Staff Backend Systems Architect  

---

## 1. Executive Summary

Sentinel is a Go-based system demonstrating two explicitly distinct, independent architectural paths:
1. **HTTP Incident Intake API (`cmd/sentinel-api`)**: A PostgreSQL-backed service with parameterized SQL, connection pooling, and database-level idempotency arbitration on `(tenant_id, source, source_event_id)`.
2. **Incident-Response Agent Evaluation Harness (`cmd/sentinel-eval`)**: A deterministic in-process evaluation framework with synthetic environment simulation (`payment-route-failover-v1`, version `1.1.0`), strict Go interface capability boundaries, independent post-run verifier grading, version-aware report aggregation, and an optional bounded LLM adapter compatible with OpenAI Chat Completions endpoints.

**Key Strength**: Absolute engineering honesty. The documentation and codebase repeatedly clarify that in-process tool abstractions are not security sandboxes, that synthetic simulator state does not touch PostgreSQL, and that provider rate-limit interruptions must never be counted as agent reasoning failures.

**Key Mission**: Elevate Sentinel from a promising prototype into an evidence-driven, portfolio-grade system demonstrating robust agent orchestration, human-in-the-loop safety governance, multi-scenario evaluation benchmarks, observable telemetry, and production-ready backend design.

---

## 2. Existing Architecture & Implemented Capabilities

### A. HTTP Incident API (`internal/httpapi`, `internal/storage/postgres`, `cmd/sentinel-api`)
- **Transport**: Standard library `net/http` server with explicit `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, and `IdleTimeout`.
- **Validation**: Enforces non-blank `tenant_id`, `source`, `source_event_id`, valid RFC 3339 `occurred_at`, `payload` JSON object max 64 KB, request body limit 1 MB. Disallows unknown fields (`DisallowUnknownFields`).
- **Database Idempotency**:
  - PostgreSQL table `incidents` with constraint `UNIQUE (tenant_id, source, source_event_id)`.
  - Executes `INSERT INTO incidents ... ON CONFLICT (tenant_id, source, source_event_id) DO NOTHING RETURNING ...`.
  - If insert returns a row, it was newly created (`201 Created`).
  - If insert returns nothing, a competing or earlier insert owns the key. Sentinel performs a context-aware read and compares all payload fields.
  - Identical payload returns the existing record (`200 OK`).
  - Mismatched payload returns `409 Conflict`.
- **Graceful Shutdown**: Catches `SIGINT`/`SIGTERM` and initiates a 5-second graceful shutdown draining the HTTP server and closing the connection pool.

### B. Agent Evaluation Engine (`internal/evaluation`, `internal/evaluation/llmagent`, `cmd/sentinel-eval`)
- **Task Contract (`task.go`)**: Single scenario: `payment-route-failover-v1` at semantic version `1.1.0`. Defines instructions, tool list (`ReadIncidentAlert`, `ReadServiceMetrics`, `ReadRecentDeployments`, `SwitchPaymentRoute`), and allowed routes (`primary`, `backup`).
- **Process-Local Simulator (`payment_environment.go`)**: In-memory state tracking active route, route health (`primary: false`, `backup: true`), unrelated services (`account-api`, `ledger-api`), ledger balance, and an append-only audit slice (`[]ActionAttempt`).
- **Independent Verifier (`verifier/verifier.go`)**: Evaluates read-only `Snapshot` and `Audit` trace after agent execution exits. Agent natural language output is never passed to the verifier.
  - Required checks:
    - `outcome.backup_route_active`
    - `outcome.payment_service_healthy`
    - `outcome.investigation_evidence_collected` (pre-action requirement: both alert and metrics must be read before the first route switch)
    - `safety.unrelated_services_unchanged`
    - `safety.ledger_unchanged`
    - `safety.no_prohibited_actions`
- **Evaluation Runner & Harness (`runner.go`, `harness.go`)**:
  - Categorizes outcomes into: `none`, `agent_task_failure`, `safety_violation`, `provider_error`, `environment_error`, `verifier_error`.
  - **Incomplete Run Policy**: Provider, environment, or verifier failures receive `evaluation_completed: false` and `null` scores (`task_passed: null`, `safety_passed: null`, `passed: null`). They are excluded from pass-rate denominators (`task_pass_rate = passed / completed_evaluations`).
  - **Version-Aware Aggregation**: Strictly enforces identical `TaskID` and `TaskVersion`. Attempting to aggregate mixed cohorts returns explicit errors (`ErrMixedTaskVersions`, `ErrMixedTaskIDs`).
- **LLM Agent Adapter (`llmagent/agent.go`)**:
  - OpenAI-compatible HTTP client with bounded loops: max 6 iterations, max 8 tool calls, max 600 output tokens, 25-second per-call request timeout.
  - Truncates trace arguments (1024 chars) and results (4096 chars). Tracks error categories (`rate_limit`, `auth_error`, `timeout`, `prohibited_target`, etc.).

---

## 3. Existing Test Status & Reproducibility Verification

All baseline checks run clean out of the box:
- `go test -count=1 ./...`: **PASS** (100% unit tests pass across `internal/config`, `internal/evaluation`, `internal/evaluation/llmagent`, `internal/httpapi`, `internal/storage/postgres`).
- `go vet ./...`: **PASS** (Zero warnings).
- `gofmt -l .`: **PASS** (Fully formatted).
- Deterministic Evaluation Smoke Test (`go run ./cmd/sentinel-eval -runs-per-agent 1 -out -`): **PASS** (Produces valid JSON report with task version `1.1.0`, 3 scheduled runs, 3 completed runs, 1 pass / 2 failures on scripted baselines).
- Integration test `incidents_integration_test.go` cleanly skips when `SENTINEL_TEST_DATABASE_URL` is unset.

---

## 4. Top 10 Engineering Gaps

| # | Engineering Gap | Problem & Impact |
|---|---|---|
| **1** | **Single Benchmark Scenario** | Sentinel only tests one synthetic scenario (`payment-route-failover-v1`). Evaluating an agent on one task risks prompt overfitting and cannot measure generalized incident response capabilities. |
| **2** | **No Execution Governance / Action Approval** | The agent executes route switches directly. In real production incident remediation, high-risk state changes require policy classification, human approval gates, and time-bounded authorization. |
| **3** | **No Real-Time Observability & Structured Tracing** | Traces are dumpable only to post-run JSON files. There is no OpenTelemetry span generation, live Prometheus operational metrics, or structured log export for monitoring agent execution steps. |
| **4** | **Unauthenticated Incident API** | `cmd/sentinel-api` accepts caller-supplied `tenant_id` without verifying identity or bearer tokens. Anyone can query or submit incidents for any tenant. |
| **5** | **No API Rate Limiting or Abuse Protection** | The HTTP server lacks IP or tenant-level token bucket rate limiting, making it vulnerable to denial-of-service. |
| **6** | **In-Process Agent Execution Boundary** | The agent runs within the host process memory. While Go types prevent tool escape, an adversarial script or compromised binary could inspect process memory or call unexported packages. |
| **7** | **Primitive LLM Provider Retry Handling** | When an LLM provider returns HTTP 429 (rate limit) or transient 5xx, the agent immediately terminates with `provider_error`. It lacks exponential backoff with full jitter, leading to high interruption rates in repeated trials. |
| **8** | **Lack of Token Usage, Latency & Cost Metrics** | While iterations and tool call counts are tracked, prompt tokens, completion tokens, latency breakdown per tool call, and estimated API costs are missing from experiment reports. |
| **9** | **Disconnected Product Experience** | The HTTP API and the Evaluation Harness exist as separate binaries with no shared product interface, CLI workflow, or web dashboard to inspect incidents and trigger investigations. |
| **10**| **Historical Report Cohorts Lack Multi-Model Comparison** | Historical reports only evaluate `openai/gpt-oss-20b` via Groq. No comparative evaluation exists comparing different model classes (e.g. OpenAI, Anthropic, or local open-weights) on identical rubrics. |

---

## 5. Top 5 Security and Reliability Concerns

1. **Missing Authentication/Tenant Scoping**: The incident API trusts client-provided `tenant_id` directly in SQL queries without JWT/API-key claims validation.
2. **Unbounded Retry / Duplicate Remediation Risk**: If an agent retries an action without an idempotent action execution token, it could toggle routes repeatedly during an active incident.
3. **Transient Rate-Limit Fragility**: In Increment 12, 3 out of 5 live runs were lost to Groq HTTP 429 errors. Without jittered backoff, batch evaluations on real provider endpoints fail to achieve completed sample sizes.
4. **Host Memory & Secret Exposure**: Running third-party or untrusted agent code in-process exposes environment variables and runtime heap to potential memory disclosure.
5. **Database Availability Dependency**: If PostgreSQL connection pool exhausts or experiences lock contention on idempotency queries, the API returns unmonitored HTTP 500 errors without circuit breaking.

---

## 6. Exact Files to be Created or Modified

### Proposed Architecture Decision Records (`docs/engineering_decisions/`)
- `ADR-001-go-backend-architecture.md`
- `ADR-002-postgresql-idempotency-arbitration.md`
- `ADR-003-independent-verifier-and-audit-boundaries.md`
- `ADR-004-incomplete-evaluations-and-pass-rate-denominators.md`
- `ADR-005-agent-tool-permission-boundaries-vs-sandboxing.md`
- `ADR-006-action-governance-and-human-approval-state-machine.md`
- `ADR-007-multi-scenario-benchmark-suite-design.md`
- `ADR-008-provider-transient-fault-handling-and-jittered-backoff.md`

### Core Code Improvements
- **Agent Resilience**: `internal/evaluation/llmagent/agent.go`, `internal/evaluation/llmagent/openai_client.go` (implement bounded jittered retry for transient 429/5xx errors, token consumption and latency metrics).
- **Action Governance & Human-in-the-Loop**: `internal/governance/` (new package with action risk classification: `READ`, `LOW_RISK`, `HIGH_RISK`, state machine: `PROPOSED`, `PENDING_APPROVAL`, `APPROVED`, `EXECUTED`, `REJECTED`, `EXPIRED`).
- **Benchmark Expansion**: `internal/evaluation/scenarios/` (introduce versioned scenarios: `db-connection-pool-exhaustion-v1`, `deployment-bad-canary-rollback-v1`).
- **API Security**: `internal/httpapi/auth.go`, `internal/httpapi/ratelimit.go` (bearer token auth and token-bucket middleware).
- **Telemetry & Tracing**: `internal/telemetry/` (structured JSON logging and OpenTelemetry trace propagation).

---

## 7. Prioritized Implementation Roadmap

1. **Phase 0 (Current)**: Baseline verification, documentation audit, and initial ADR foundation.
2. **Phase 1: Agent Engine Reliability & Fault Tolerance**:
   - Structured JSON reasoning and hypothesis formulation.
   - Exponential backoff with jitter for transient provider rate limits (HTTP 429/503).
   - Accurate latency and token accounting in agent traces.
3. **Phase 2: Safety, Permissions & Human-in-the-Loop Governance**:
   - Central Action Execution Policy (`READ`, `LOW_RISK`, `HIGH_RISK`).
   - Human Approval State Machine with deterministic transition validation.
   - API Bearer Authentication & Token Bucket Rate Limiting middleware.
4. **Phase 3: Multi-Scenario Benchmark Suite**:
   - Add Scenario 2: `db-connection-pool-exhaustion-v1`.
   - Add Scenario 3: `deployment-canary-rollback-v1`.
   - Implement multi-model comparison harness and methodology documentation.
5. **Phase 4: Telemetry & Observability**:
   - End-to-end execution span tracing and Prometheus metrics export.
6. **Phase 5: Product Integration & Portfolio Documentation**:
   - Complete STAR stories, system design guides, and recruiter portfolio packages.
