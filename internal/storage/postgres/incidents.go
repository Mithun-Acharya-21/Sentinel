package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"sentinel/internal/incident"
)

// Repository stores incidents in PostgreSQL. The caller owns the database pool lifecycle.
type Repository struct {
	db *sql.DB
}

// NewRepository creates an incident repository over an already configured database pool.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

var _ incident.Repository = (*Repository)(nil)

const incidentColumns = `id::text, tenant_id, source, source_event_id, service, title, description, severity, occurred_at, created_at`

// Create inserts a new incident or identifies a duplicate source event.
// replay is true only when the existing row has the same payload.
func (r *Repository) Create(ctx context.Context, input incident.CreateInput) (incident.Incident, bool, error) {
	// PostgreSQL timestamps store microseconds. Normalize before writing and comparing
	// so retries with higher-precision RFC3339 timestamps remain idempotent.
	input.OccurredAt = input.OccurredAt.UTC().Truncate(time.Microsecond)

	query := `
INSERT INTO incidents (
	tenant_id, source, source_event_id, service, title, description, severity, occurred_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (tenant_id, source, source_event_id) DO NOTHING
RETURNING ` + incidentColumns

	created, err := scanIncident(r.db.QueryRowContext(ctx, query,
		input.TenantID,
		input.Source,
		input.SourceEventID,
		input.Service,
		input.Title,
		input.Description,
		string(input.Severity),
		input.OccurredAt,
	))
	if err == nil {
		return created, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return incident.Incident{}, false, storageError("insert incident", err)
	}

	// ON CONFLICT waits for an in-flight insert on this unique key. This SELECT is a
	// new statement under READ COMMITTED, so it sees the row that won the conflict.
	existing, err := r.findByEventKey(ctx, input.TenantID, input.Source, input.SourceEventID)
	if err != nil {
		return incident.Incident{}, false, err
	}
	if !samePayload(existing, input) {
		return incident.Incident{}, false, incident.ErrIdempotencyConflict
	}
	return existing, true, nil
}

// Get retrieves an incident by its UUID string.
func (r *Repository) Get(ctx context.Context, id string) (incident.Incident, error) {
	query := `SELECT ` + incidentColumns + ` FROM incidents WHERE id = $1::uuid`
	got, err := scanIncident(r.db.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return incident.Incident{}, incident.ErrNotFound
	}
	if err != nil {
		return incident.Incident{}, storageError("get incident", err)
	}
	return got, nil
}

func (r *Repository) findByEventKey(ctx context.Context, tenantID, source, sourceEventID string) (incident.Incident, error) {
	query := `SELECT ` + incidentColumns + `
FROM incidents
WHERE tenant_id = $1 AND source = $2 AND source_event_id = $3`
	got, err := scanIncident(r.db.QueryRowContext(ctx, query, tenantID, source, sourceEventID))
	if err != nil {
		return incident.Incident{}, storageError("find incident by source event", err)
	}
	return got, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanIncident(row rowScanner) (incident.Incident, error) {
	var got incident.Incident
	var severity string
	err := row.Scan(
		&got.ID,
		&got.TenantID,
		&got.Source,
		&got.SourceEventID,
		&got.Service,
		&got.Title,
		&got.Description,
		&severity,
		&got.OccurredAt,
		&got.CreatedAt,
	)
	got.Severity = incident.Severity(severity)
	return got, err
}

func samePayload(existing incident.Incident, input incident.CreateInput) bool {
	return existing.TenantID == input.TenantID &&
		existing.Source == input.Source &&
		existing.SourceEventID == input.SourceEventID &&
		existing.Service == input.Service &&
		existing.Title == input.Title &&
		existing.Description == input.Description &&
		existing.Severity == input.Severity &&
		existing.OccurredAt.Equal(input.OccurredAt)
}

func storageError(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", incident.ErrUnavailable, operation, err)
}
