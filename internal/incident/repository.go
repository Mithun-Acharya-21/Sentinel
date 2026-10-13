package incident

import (
	"context"
	"errors"
)

var (
	ErrNotFound            = errors.New("incident not found")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different fields")
	ErrUnavailable         = errors.New("incident storage unavailable")
)

// Repository is the persistence boundary used by the HTTP API.
// Create reports replay=true when an identical source event already exists.
type Repository interface {
	Create(ctx context.Context, input CreateInput) (created Incident, replay bool, err error)
	Get(ctx context.Context, id string) (Incident, error)
}
