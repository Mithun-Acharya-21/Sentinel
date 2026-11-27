# ADR-017: Controlled Remediation Execution, Simulator Boundary & Post-Action Verification

## Status
Accepted

## Date
2026-11-27

## Context
With Milestone 3C establishing LLM triage and human approval (`pending_review` $\rightarrow$ `approved`), Sentinel needed a controlled execution engine that enforces human approval, prevents concurrent/duplicate execution, and independently verifies post-action infrastructure state without touching real cloud or payment infrastructure.

## Decision

### 1. Allowlisted Simulated Environment (`remediation.Simulator`)
The initial execution engine targets an isolated, process-local deterministic `Simulator` supporting only three allowlisted actions:
- `switch_payment_route_to_backup`
- `disable_unhealthy_provider`
- `restart_simulated_worker`

Why a simulator first: It enables deterministic adversarial testing of precondition failures, verification failures, lease reclamation, and approval boundaries without risking real infrastructure or cloud credentials.

### 2. Approval-Bound Execution & Action Matching
Approval is bound to the specific `ProposedAction` in `incident_triage_results`. `Executor.Execute` enforces `triage.CanExecuteAction`, `req.ApprovalID == view.Approval.ID`, and `ActionMatchesApprovedRecommendation` before claiming an execution.

### 3. Durable State Machine & `execution_token` Fencing
`remediation_executions` (`migrations/006_create_remediation_executions.up.sql`) uses `FOR UPDATE` in `ClaimExecution` to issue a fresh UUID `execution_token` and `lease_expires_at`, releases the DB transaction before `Simulator.Apply`, and fences `CompleteExecution` on `AND status = 'executing' AND execution_token = $2::uuid`. Completed executions (`status = 'succeeded'`) return idempotently without re-applying side effects.

### 4. Independent Post-Action Verification (`VerifyPostAction`)
`VerifyPostAction` inspects the authoritative `before` and `after` simulator snapshots rather than trusting executor return values or LLM claims.

## Consequences
- Zero autonomous LLM execution; strict separation of duties (`incidents:write` $\neq$ `incidents:approve` $\neq$ `incidents:execute`).
- At-least-once execution semantics: a crash between `Simulator.Apply` and `CompleteExecution` is recovered after `lease_expires_at` via retry.
