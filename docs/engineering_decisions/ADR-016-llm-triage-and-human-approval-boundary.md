# ADR-016: LLM-Powered Incident Triage & Human Approval Boundary

## Status
Accepted

## Date
2026-11-27

## Context
Sentinel needed to integrate an LLM-powered incident triage step into the asynchronous worker pipeline (`internal/worker`) so accepted incidents receive structured root-cause analysis and a recommended action—without allowing the model to execute remediation autonomously, bypass tenant isolation, or duplicate existing LLM retry/worker infrastructure.

## Decision

### 1. Reuse `llmagent.Client` & `llmagent.HTTPClient`
`internal/triage.Processor` invokes `llmagent.Client.Complete` directly, reusing Milestone 1's bounded exponential backoff, jitter, `Retry-After` parsing, safe `HTTPError` classification, and token/latency accounting.

### 2. Zero-Tool Bounded Model Request & Strict Schema Contract (`v1.0.0`)
- `CompletionRequest.Tools` is explicitly `nil`; the model cannot request or execute any tool calls.
- Responses are decoded with `DisallowUnknownFields()` and validated against strict enums (`Classification`, `incident.Severity`, `ProposedAction`), `[0.0, 1.0]` confidence bounds, and non-empty `Evidence` and `RecommendedSteps`.
- `VerifyProposal` separates `TaskPassed` from `SafetyPassed`, rejects prohibited shell/self-approval claims, and keeps `ActionExecuted = false`.

### 3. Explicit Human Approval State Machine
- `SaveTriageAndApproval` persists `incident_triage_results` and `incident_triage_approvals` (`status = pending_review`) in one PostgreSQL transaction *after* the LLM HTTP call returns (no DB transaction held open during LLM latency).
- `POST /v1/incidents/{id}/triage/approve` and `/reject` require `incidents:approve` scope, derive `tenant_id` and `decided_by` exclusively from `auth.Principal`, and use `SELECT ... FOR UPDATE` to reject duplicate (`409 approval_already_decided`) or expired (`409 approval_expired`) transitions.

## Consequences
- No remediation can proceed without an authenticated human principal's explicit `approved` decision in PostgreSQL.
- `SENTINEL_LLM_API_KEY` remains optional; when unset, `sentinel-api` falls back to `worker.NewDefaultIncidentProcessor`.
