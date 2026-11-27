package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"sentinel/internal/auth"
)

// CredentialRepository stores and retrieves opaque API-key metadata and secret hashes in PostgreSQL.
type CredentialRepository struct {
	db *sql.DB
}

// NewCredentialRepository creates a credential repository over an existing database pool.
func NewCredentialRepository(db *sql.DB) *CredentialRepository {
	return &CredentialRepository{db: db}
}

var _ auth.CredentialStore = (*CredentialRepository)(nil)

const credentialColumns = `id::text, key_id, secret_hash, tenant_id, principal_id, array_to_json(scopes)::text, revoked_at, expires_at, created_at`

// GetByKeyID retrieves credential metadata and secret hash by public key_id.
func (r *CredentialRepository) GetByKeyID(ctx context.Context, keyID string) (auth.StoredCredential, error) {
	query := `SELECT ` + credentialColumns + ` FROM api_credentials WHERE key_id = $1`
	cred, err := scanCredential(r.db.QueryRowContext(ctx, query, keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return auth.StoredCredential{}, auth.ErrCredentialNotFound
	}
	if err != nil {
		return auth.StoredCredential{}, credentialStorageError("get credential", err)
	}
	return cred, nil
}

// Create persists a new API credential's metadata, SHA-256 secret hash, and optional UTC expiration.
func (r *CredentialRepository) Create(ctx context.Context, input auth.CreateCredentialInput) (auth.StoredCredential, error) {
	scopes := input.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return auth.StoredCredential{}, fmt.Errorf("marshal scopes: %w", err)
	}

	var expiresAt sql.NullTime
	if input.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: input.ExpiresAt.UTC(), Valid: true}
	}

	query := `
INSERT INTO api_credentials (
	key_id, secret_hash, tenant_id, principal_id, scopes, expires_at
) VALUES (
	$1, $2, $3, $4, ARRAY(SELECT jsonb_array_elements_text($5::jsonb)), $6
)
RETURNING ` + credentialColumns

	cred, err := scanCredential(r.db.QueryRowContext(ctx, query,
		input.KeyID,
		input.SecretHash,
		input.TenantID,
		input.PrincipalID,
		string(scopesJSON),
		expiresAt,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return auth.StoredCredential{}, auth.ErrDuplicateKeyID
		}
		return auth.StoredCredential{}, credentialStorageError("create credential", err)
	}
	return cred, nil
}

// Revoke marks an API credential as revoked. Revoking an already-revoked credential
// is idempotent and returns nil; an unknown key_id returns auth.ErrCredentialNotFound.
func (r *CredentialRepository) Revoke(ctx context.Context, keyID string) error {
	const query = `
WITH target AS (
	SELECT id FROM api_credentials WHERE key_id = $1
),
updated AS (
	UPDATE api_credentials
	SET revoked_at = now()
	WHERE key_id = $1 AND revoked_at IS NULL
	RETURNING id
)
SELECT
	EXISTS(SELECT 1 FROM target) AS found,
	EXISTS(SELECT 1 FROM updated) AS newly_revoked`

	var found, newlyRevoked bool
	if err := r.db.QueryRowContext(ctx, query, keyID).Scan(&found, &newlyRevoked); err != nil {
		return credentialStorageError("revoke credential", err)
	}
	if !found {
		return auth.ErrCredentialNotFound
	}
	_ = newlyRevoked
	return nil
}

func scanCredential(row rowScanner) (auth.StoredCredential, error) {
	var cred auth.StoredCredential
	var scopesJSON string
	var revokedAt sql.NullTime
	var expiresAt sql.NullTime
	err := row.Scan(
		&cred.ID,
		&cred.KeyID,
		&cred.SecretHash,
		&cred.TenantID,
		&cred.PrincipalID,
		&scopesJSON,
		&revokedAt,
		&expiresAt,
		&cred.CreatedAt,
	)
	if err != nil {
		return auth.StoredCredential{}, err
	}
	if revokedAt.Valid {
		t := revokedAt.Time.UTC()
		cred.RevokedAt = &t
	}
	if expiresAt.Valid {
		t := expiresAt.Time.UTC()
		cred.ExpiresAt = &t
	}
	cred.CreatedAt = cred.CreatedAt.UTC()
	if err := json.Unmarshal([]byte(scopesJSON), &cred.Scopes); err != nil {
		return auth.StoredCredential{}, fmt.Errorf("unmarshal credential scopes: %w", err)
	}
	return cred, nil
}

func credentialStorageError(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", auth.ErrStoreUnavailable, operation, err)
}

// Ensure time import is used when building without tests.
var _ = time.UTC
