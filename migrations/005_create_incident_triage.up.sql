CREATE TABLE incident_triage_results (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    job_id UUID NOT NULL REFERENCES incident_jobs(id) ON DELETE CASCADE,
    contract_version TEXT NOT NULL CHECK (length(btrim(contract_version)) > 0),
    prompt_version TEXT NOT NULL CHECK (length(btrim(prompt_version)) > 0),
    model TEXT NOT NULL CHECK (length(btrim(model)) > 0),
    classification TEXT NOT NULL CHECK (length(btrim(classification)) > 0),
    recommended_severity TEXT NOT NULL CHECK (recommended_severity IN ('low', 'medium', 'high', 'critical')),
    confidence DOUBLE PRECISION NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    evidence JSONB NOT NULL,
    suspected_root_cause TEXT NOT NULL CHECK (length(btrim(suspected_root_cause)) > 0),
    recommended_steps JSONB NOT NULL,
    proposed_action TEXT NOT NULL DEFAULT '',
    requires_human_approval BOOLEAN NOT NULL DEFAULT true,
    processing_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT incident_triage_results_incident_id_key UNIQUE (incident_id),
    CONSTRAINT incident_triage_results_job_id_key UNIQUE (job_id)
);

CREATE INDEX idx_incident_triage_results_tenant
    ON incident_triage_results (tenant_id, created_at DESC);

CREATE TABLE incident_triage_approvals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    triage_id UUID NOT NULL REFERENCES incident_triage_results(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('pending_review', 'approved', 'rejected', 'expired')),
    proposed_action TEXT NOT NULL DEFAULT '',
    decided_by TEXT NULL,
    decision_reason TEXT NOT NULL DEFAULT '',
    decided_at TIMESTAMPTZ NULL,
    expires_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT incident_triage_approvals_triage_id_key UNIQUE (triage_id),
    CONSTRAINT incident_triage_approvals_incident_id_key UNIQUE (incident_id)
);

CREATE INDEX idx_incident_triage_approvals_tenant_status
    ON incident_triage_approvals (tenant_id, status, created_at DESC);
