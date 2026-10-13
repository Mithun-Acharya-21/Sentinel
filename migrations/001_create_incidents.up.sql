CREATE TABLE incidents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    source TEXT NOT NULL CHECK (length(btrim(source)) > 0),
    source_event_id TEXT NOT NULL CHECK (length(btrim(source_event_id)) > 0),
    service TEXT NOT NULL CHECK (length(btrim(service)) > 0),
    title TEXT NOT NULL CHECK (length(btrim(title)) > 0),
    description TEXT NOT NULL CHECK (length(btrim(description)) > 0),
    severity TEXT NOT NULL CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT incidents_tenant_source_event_key
        UNIQUE (tenant_id, source, source_event_id)
);
