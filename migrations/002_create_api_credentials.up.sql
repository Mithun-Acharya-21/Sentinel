CREATE TABLE api_credentials (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_id TEXT NOT NULL UNIQUE CHECK (length(btrim(key_id)) > 0),
    secret_hash TEXT NOT NULL CHECK (length(btrim(secret_hash)) = 64),
    tenant_id TEXT NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    principal_id TEXT NOT NULL CHECK (length(btrim(principal_id)) > 0),
    scopes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    revoked_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_credentials_tenant_id ON api_credentials (tenant_id);
CREATE INDEX idx_api_credentials_active_key_id ON api_credentials (key_id) WHERE revoked_at IS NULL;
