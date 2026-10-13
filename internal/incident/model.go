package incident

import (
	"errors"
	"strings"
	"time"
)

// Severity is the bounded set of incident urgency values accepted by Sentinel.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Incident is the persisted representation returned by the incident repository.
type Incident struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	Source        string    `json:"source"`
	SourceEventID string    `json:"source_event_id"`
	Service       string    `json:"service"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Severity      Severity  `json:"severity"`
	OccurredAt    time.Time `json:"occurred_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateInput contains client-supplied incident fields; IDs and creation time are server-owned.
type CreateInput struct {
	TenantID      string    `json:"tenant_id"`
	Source        string    `json:"source"`
	SourceEventID string    `json:"source_event_id"`
	Service       string    `json:"service"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Severity      Severity  `json:"severity"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// Validate checks required fields and supported severity before persistence.
func (in CreateInput) Validate() error {
	fields := []struct{ name, value string }{
		{name: "tenant_id", value: in.TenantID},
		{name: "source", value: in.Source},
		{name: "source_event_id", value: in.SourceEventID},
		{name: "service", value: in.Service},
		{name: "title", value: in.Title},
		{name: "description", value: in.Description},
	}
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return errors.New(field.name + " is required")
		}
	}

	switch in.Severity {
	case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
	default:
		return errors.New("severity must be one of: low, medium, high, critical")
	}
	if in.OccurredAt.IsZero() {
		return errors.New("occurred_at is required")
	}
	return nil
}
