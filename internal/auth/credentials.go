package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// TokenPrefix identifies Sentinel opaque API keys.
	TokenPrefix = "snt_"
	keyIDBytes  = 8  // 16 hex characters
	secretBytes = 32 // 256-bit random secret
)

var (
	ErrMissingCredentials  = errors.New("missing credentials")
	ErrInvalidCredentials  = errors.New("invalid credentials")
	ErrRevokedCredentials  = errors.New("credentials have been revoked")
	ErrExpiredCredentials  = errors.New("credentials have expired")
	ErrVerifierUnavailable = errors.New("credential verifier unavailable")
	ErrCredentialNotFound  = errors.New("credential not found")
	ErrDuplicateKeyID      = errors.New("credential key_id already exists")
	ErrStoreUnavailable    = errors.New("credential store unavailable")
)

// CredentialVerifier validates a raw Bearer token and returns the verified Principal.
// This interface decouples HTTP middleware from the backing credential mechanism so
// external identity providers can be integrated without rewriting handlers.
type CredentialVerifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

// StoredCredential represents persisted credential metadata and secret hash.
// It never contains the plaintext secret.
type StoredCredential struct {
	ID          string
	KeyID       string
	SecretHash  string
	TenantID    string
	PrincipalID string
	Scopes      []string
	RevokedAt   *time.Time
	ExpiresAt   *time.Time
	CreatedAt   time.Time
}

// CreateCredentialInput contains only non-secret metadata and the hashed secret for persistence.
type CreateCredentialInput struct {
	KeyID       string
	SecretHash  string
	TenantID    string
	PrincipalID string
	Scopes      []string
	ExpiresAt   *time.Time
}

// ProvisionedCredential is returned once at creation time. PlaintextToken must not be logged or persisted.
type ProvisionedCredential struct {
	KeyID          string
	TenantID       string
	PrincipalID    string
	Scopes         []string
	ExpiresAt      *time.Time
	PlaintextToken string `json:"-"`
}

// AuthMetricsSnapshot holds bounded-cardinality credential lifecycle telemetry.
// It never contains secrets, hashes, raw tokens, or unbounded tenant labels.
type AuthMetricsSnapshot struct {
	AuthSuccesses       uint64  `json:"auth_successes"`
	AuthRejections      uint64  `json:"auth_rejections"`
	RevokedRejections   uint64  `json:"revoked_rejections"`
	ExpiredRejections   uint64  `json:"expired_rejections"`
	DBLookupCount       uint64  `json:"db_lookup_count"`
	DBLookupErrors      uint64  `json:"db_lookup_errors"`
	AvgDBLookupMillis   float64 `json:"avg_db_lookup_ms"`
	RevocationSuccesses uint64  `json:"revocation_successes"`
	RevocationFailures  uint64  `json:"revocation_failures"`
}

// AuthMetrics tracks thread-safe atomic counters for credential verification and revocation.
type AuthMetrics struct {
	authSuccesses       atomic.Uint64
	authRejections      atomic.Uint64
	revokedRejections   atomic.Uint64
	expiredRejections   atomic.Uint64
	dbLookupCount       atomic.Uint64
	dbLookupErrors      atomic.Uint64
	dbLookupTotalNs     atomic.Uint64
	revocationSuccesses atomic.Uint64
	revocationFailures  atomic.Uint64
}

// RecordRevocation records a credential revocation outcome.
func (m *AuthMetrics) RecordRevocation(err error) {
	if m == nil {
		return
	}
	if err != nil {
		m.revocationFailures.Add(1)
		return
	}
	m.revocationSuccesses.Add(1)
}

// Snapshot returns a point-in-time copy of all authentication metrics.
func (m *AuthMetrics) Snapshot() AuthMetricsSnapshot {
	if m == nil {
		return AuthMetricsSnapshot{}
	}
	lookups := m.dbLookupCount.Load()
	totalNs := m.dbLookupTotalNs.Load()
	var avgMs float64
	if lookups > 0 {
		avgMs = (float64(totalNs) / float64(lookups)) / 1e6
	}
	return AuthMetricsSnapshot{
		AuthSuccesses:       m.authSuccesses.Load(),
		AuthRejections:      m.authRejections.Load(),
		RevokedRejections:   m.revokedRejections.Load(),
		ExpiredRejections:   m.expiredRejections.Load(),
		DBLookupCount:       lookups,
		DBLookupErrors:      m.dbLookupErrors.Load(),
		AvgDBLookupMillis:   avgMs,
		RevocationSuccesses: m.revocationSuccesses.Load(),
		RevocationFailures:  m.revocationFailures.Load(),
	}
}

// CredentialStore is the persistence boundary for opaque API-key credentials.
type CredentialStore interface {
	GetByKeyID(ctx context.Context, keyID string) (StoredCredential, error)
	Create(ctx context.Context, input CreateCredentialInput) (StoredCredential, error)
	Revoke(ctx context.Context, keyID string) error
}

// GenerateAPIKey creates a cryptographically random opaque API key with no automatic expiration.
func GenerateAPIKey(tenantID, principalID string, scopes []string) (ProvisionedCredential, CreateCredentialInput, error) {
	return GenerateAPIKeyWithExpiry(tenantID, principalID, scopes, nil)
}

// GenerateAPIKeyWithExpiry creates a cryptographically random opaque API key and its SHA-256 hash.
// If expiresAt is non-nil, it is normalized to UTC.
// The returned ProvisionedCredential holds the plaintext token to be displayed once to the operator.
func GenerateAPIKeyWithExpiry(tenantID, principalID string, scopes []string, expiresAt *time.Time) (ProvisionedCredential, CreateCredentialInput, error) {
	tenantID = strings.TrimSpace(tenantID)
	principalID = strings.TrimSpace(principalID)
	if tenantID == "" {
		return ProvisionedCredential{}, CreateCredentialInput{}, errors.New("tenant_id is required")
	}
	if principalID == "" {
		return ProvisionedCredential{}, CreateCredentialInput{}, errors.New("principal_id is required")
	}

	var expUTC *time.Time
	if expiresAt != nil {
		t := expiresAt.UTC()
		expUTC = &t
	}

	var idBuf [keyIDBytes]byte
	if _, err := rand.Read(idBuf[:]); err != nil {
		return ProvisionedCredential{}, CreateCredentialInput{}, fmt.Errorf("generate key id: %w", err)
	}
	keyID := TokenPrefix + hex.EncodeToString(idBuf[:])

	var secretBuf [secretBytes]byte
	if _, err := rand.Read(secretBuf[:]); err != nil {
		return ProvisionedCredential{}, CreateCredentialInput{}, fmt.Errorf("generate secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBuf[:])
	secretHash := HashSecret(secret)

	normalizedScopes := make([]string, 0, len(scopes))
	for _, s := range scopes {
		trimmed := strings.TrimSpace(s)
		if trimmed != "" {
			normalizedScopes = append(normalizedScopes, trimmed)
		}
	}

	plaintext := keyID + "_" + secret
	provisioned := ProvisionedCredential{
		KeyID:          keyID,
		TenantID:       tenantID,
		PrincipalID:    principalID,
		Scopes:         normalizedScopes,
		ExpiresAt:      expUTC,
		PlaintextToken: plaintext,
	}
	input := CreateCredentialInput{
		KeyID:       keyID,
		SecretHash:  secretHash,
		TenantID:    tenantID,
		PrincipalID: principalID,
		Scopes:      normalizedScopes,
		ExpiresAt:   expUTC,
	}
	return provisioned, input, nil
}

// HashSecret computes the hex-encoded SHA-256 digest of a credential secret.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// VerifySecretHash compares a candidate secret against a stored hex-encoded SHA-256 hash
// using constant-time comparison to prevent timing side-channel leaks.
func VerifySecretHash(secret, expectedHashHex string) bool {
	expectedBytes, err := hex.DecodeString(strings.TrimSpace(expectedHashHex))
	if err != nil || len(expectedBytes) != sha256.Size {
		return false
	}
	computed := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(computed[:], expectedBytes) == 1
}

// ParseToken splits a token of the form "snt_<16-hex-chars>_<secret>".
// Malformed tokens are rejected before any database lookup.
func ParseToken(token string) (keyID string, secret string, ok bool) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return "", "", false
	}
	rest := token[len(TokenPrefix):]
	idx := strings.IndexByte(rest, '_')
	if idx != keyIDBytes*2 {
		return "", "", false
	}
	idHex := rest[:idx]
	if _, err := hex.DecodeString(idHex); err != nil {
		return "", "", false
	}
	secret = rest[idx+1:]
	if len(secret) < 16 {
		return "", "", false
	}
	return TokenPrefix + idHex, secret, true
}

// StoreVerifier implements CredentialVerifier using a CredentialStore and constant-time hash verification.
type StoreVerifier struct {
	store   CredentialStore
	clock   func() time.Time
	metrics *AuthMetrics
}

// NewStoreVerifier creates a CredentialVerifier backed by the given CredentialStore using UTC wall clock.
func NewStoreVerifier(store CredentialStore) *StoreVerifier {
	return NewStoreVerifierWithClock(store, nil)
}

// NewStoreVerifierWithClock creates a CredentialVerifier with an injectable clock for deterministic expiration tests.
func NewStoreVerifierWithClock(store CredentialStore, clock func() time.Time) *StoreVerifier {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &StoreVerifier{
		store:   store,
		clock:   clock,
		metrics: &AuthMetrics{},
	}
}

// Metrics returns a snapshot of credential verification telemetry.
func (v *StoreVerifier) Metrics() AuthMetricsSnapshot {
	if v == nil || v.metrics == nil {
		return AuthMetricsSnapshot{}
	}
	return v.metrics.Snapshot()
}

// Verify validates an opaque Bearer token against stored credential hashes, revocation, and expiration.
func (v *StoreVerifier) Verify(ctx context.Context, token string) (Principal, error) {
	if v == nil || v.store == nil {
		return Principal{}, ErrVerifierUnavailable
	}
	keyID, secret, ok := ParseToken(strings.TrimSpace(token))
	if !ok {
		v.metrics.authRejections.Add(1)
		return Principal{}, ErrInvalidCredentials
	}

	start := v.clock()
	stored, err := v.store.GetByKeyID(ctx, keyID)
	elapsed := v.clock().Sub(start)
	v.metrics.dbLookupCount.Add(1)
	if elapsed > 0 {
		v.metrics.dbLookupTotalNs.Add(uint64(elapsed.Nanoseconds()))
	}

	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			v.metrics.authRejections.Add(1)
			return Principal{}, ErrInvalidCredentials
		}
		v.metrics.dbLookupErrors.Add(1)
		return Principal{}, fmt.Errorf("%w: %v", ErrVerifierUnavailable, err)
	}

	// Always verify hash in constant time before checking revocation or expiration,
	// avoiding timing differences between active, revoked, and expired keys.
	hashMatch := VerifySecretHash(secret, stored.SecretHash)
	if !hashMatch {
		v.metrics.authRejections.Add(1)
		return Principal{}, ErrInvalidCredentials
	}
	if stored.RevokedAt != nil {
		v.metrics.authRejections.Add(1)
		v.metrics.revokedRejections.Add(1)
		return Principal{}, ErrRevokedCredentials
	}
	if stored.ExpiresAt != nil && !v.clock().UTC().Before(stored.ExpiresAt.UTC()) {
		v.metrics.authRejections.Add(1)
		v.metrics.expiredRejections.Add(1)
		return Principal{}, ErrExpiredCredentials
	}

	v.metrics.authSuccesses.Add(1)
	scopes := make([]string, len(stored.Scopes))
	copy(scopes, stored.Scopes)

	return Principal{
		PrincipalID:  stored.PrincipalID,
		TenantID:     stored.TenantID,
		CredentialID: stored.ID,
		Scopes:       scopes,
		AuthMethod:   AuthMethodBearerAPIKey,
	}, nil
}
