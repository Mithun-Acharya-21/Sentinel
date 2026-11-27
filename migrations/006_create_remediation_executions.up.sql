CREATE TABLE remediation_executions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    triage_id UUID NOT NULL REFERENCES incident_triage_results(id) ON DELETE CASCADE,
    approval_id UUID NOT NULL REFERENCES incident_triage_approvals(id) ON DELETE CASCADE,
    action_type TEXT NOT NULL CHECK (action_type IN ('switch_payment_route_to_backup', 'disable_unhealthy_provider', 'restart_simulated_worker')),
    action_version TEXT NOT NULL CHECK (length(btrim(action_version)) > 0),
    target_resource TEXT NOT NULL CHECK (length(btrim(target_resource)) > 0),
    parameters JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL CHECK (length(btrim(idempotency_key)) > 0),
    approver_id TEXT NOT NULL CHECK (length(btrim(approver_id)) > 0),
    status TEXT NOT NULL CHECK (status IN ('pending', 'authorized', 'executing', 'succeeded', 'failed', 'verification_failed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    execution_token UUID NULL,
    lease_expires_at TIMESTAMPTZ NULL,
    verification_passed BOOLEAN NULL,
    verification_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    failure_category TEXT NOT NULL DEFAULT '',
    failure_message TEXT NOT NULL DEFAULT '',
    requested_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ NULL,
    completed_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT remediation_executions_tenant_idempotency_key UNIQUE (tenant_id, idempotency_key),
    CONSTRAINT remediation_executions_approval_id_key UNIQUE (approval_id)
);

CREATE INDEX idx_remediation_executions_tenant_incident
    ON remediation_executions (tenant_id, incident_id);

CREATE INDEX idx_remediation_executions_status
    ON remediation_executions (status, lease_expires_at);
