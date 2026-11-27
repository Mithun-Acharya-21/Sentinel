ALTER TABLE api_credentials
    ADD COLUMN expires_at TIMESTAMPTZ NULL;

CREATE INDEX idx_api_credentials_expires_at
    ON api_credentials (expires_at)
    WHERE expires_at IS NOT NULL;
