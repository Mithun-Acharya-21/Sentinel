# Sentinel Controlled Remediation Runbook

## 1. Operational Metrics
- `remediation_requests_total`, `remediation_authorized_total`
- `remediation_rejected_total{reason="invalid_action|triage_not_found|approval_invalid|approval_mismatch|action_mismatch|claim_conflict"}`
- `remediation_executions_total{action_type,status}`
- `remediation_failures_total{category="precondition_failed|timeout_or_canceled|execution_error"}`
- `remediation_verification_failures_total`
- `remediation_duration_seconds`

## 2. Troubleshooting Common Failures
- **`403 approval_required`:** Confirm the human operator approved the triage via `POST /v1/incidents/{id}/triage/approve` and that `expires_at` has not passed.
- **`403 action_mismatch`:** The requested `action_type` does not match the approved triage `proposed_action`.
- **`409 execution_conflict`:** Another execution lease is currently active (`status = 'executing'` and `now < lease_expires_at`), or `idempotency_key` was reused with a different approval.
- **`status = 'failed'` (`precondition_failed`):** Inspect simulator state (`BackupProviderHealth`, `PrimaryProviderHealth`) and retry once preconditions are met.
- **`status = 'verification_failed'`:** `remediation_verification_failures_total` incremented; inspect `verification_summary` in `remediation_executions` (`GET /v1/incidents/{id}/remediation`) to see which post-action check failed.
