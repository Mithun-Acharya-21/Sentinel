DROP INDEX IF EXISTS idx_api_credentials_expires_at;

ALTER TABLE api_credentials
    DROP COLUMN IF EXISTS expires_at;
