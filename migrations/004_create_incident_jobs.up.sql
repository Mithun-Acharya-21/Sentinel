CREATE TABLE incident_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'dead_letter')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_at TIMESTAMPTZ NULL,
    locked_by TEXT NULL,
    lease_token UUID NULL,
    lease_expires_at TIMESTAMPTZ NULL,
    last_error TEXT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ NULL,
    CONSTRAINT incident_jobs_incident_id_key UNIQUE (incident_id)
);

CREATE INDEX idx_incident_jobs_ready
    ON incident_jobs (available_at, created_at)
    WHERE status IN ('pending', 'failed');

CREATE INDEX idx_incident_jobs_expired_leases
    ON incident_jobs (lease_expires_at)
    WHERE status = 'processing';

CREATE INDEX idx_incident_jobs_tenant_status
    ON incident_jobs (tenant_id, status, created_at DESC);

CREATE TABLE incident_job_effects (
    idempotency_key TEXT PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES incident_jobs(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    effect_name TEXT NOT NULL CHECK (length(btrim(effect_name)) > 0),
    result_summary TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_incident_job_effects_job_id ON incident_job_effects (job_id);
