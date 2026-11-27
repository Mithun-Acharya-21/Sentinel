package remediation

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// AuditEvent records an immutable simulated infrastructure mutation.
type AuditEvent struct {
	ActionType     ActionType `json:"action_type"`
	TargetResource string     `json:"target_resource"`
	IdempotencyKey string     `json:"idempotency_key"`
	AppliedAt      time.Time  `json:"applied_at"`
	Details        string     `json:"details"`
}

// Snapshot represents a point-in-time copy of the simulated infrastructure state.
type Snapshot struct {
	PrimaryProviderHealth  bool         `json:"primary_provider_health"`
	PrimaryProviderEnabled bool         `json:"primary_provider_enabled"`
	BackupProviderHealth   bool         `json:"backup_provider_health"`
	ActivePaymentRoute     string       `json:"active_payment_route"`
	SimulatedWorkerHealthy bool         `json:"simulated_worker_healthy"`
	WorkerRestartCount     int          `json:"worker_restart_count"`
	AuditEvents            []AuditEvent `json:"audit_events"`
}

// Environment is the deterministic simulated infrastructure boundary.
type Environment interface {
	Apply(ctx context.Context, req ActionRequest) error
	Snapshot() Snapshot
}

// Simulator is a deterministic, process-local simulated infrastructure environment.
// It has no network, cloud, or database access.
type Simulator struct {
	mu    sync.Mutex
	state Snapshot
	clock func() time.Time
}

// NewSimulator creates a Simulator in a degraded-primary, healthy-backup initial state
// suitable for controlled incident remediation demonstrations and tests.
func NewSimulator(clock func() time.Time) *Simulator {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Simulator{
		clock: clock,
		state: Snapshot{
			PrimaryProviderHealth:  false,
			PrimaryProviderEnabled: true,
			BackupProviderHealth:   true,
			ActivePaymentRoute:     "primary",
			SimulatedWorkerHealthy: false,
			WorkerRestartCount:     0,
			AuditEvents:            make([]AuditEvent, 0),
		},
	}
}

// Snapshot returns a deep copy of the authoritative simulated state.
func (s *Simulator) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.state
	cp.AuditEvents = append([]AuditEvent(nil), s.state.AuditEvents...)
	return cp
}

// SetState replaces the simulator state for deterministic test scenarios.
func (s *Simulator) SetState(st Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := st
	cp.AuditEvents = append([]AuditEvent(nil), st.AuditEvents...)
	s.state = cp
}

// Apply validates preconditions and executes an allowlisted simulated action.
func (s *Simulator) Apply(ctx context.Context, req ActionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock().UTC()

	switch req.ActionType {
	case ActionSwitchPaymentRouteToBackup:
		if !s.state.BackupProviderHealth {
			return fmt.Errorf("%w: backup payment provider is unhealthy", ErrPreconditionFailed)
		}
		s.state.ActivePaymentRoute = "backup"
		s.state.AuditEvents = append(s.state.AuditEvents, AuditEvent{
			ActionType:     req.ActionType,
			TargetResource: req.TargetResource,
			IdempotencyKey: req.IdempotencyKey,
			AppliedAt:      now,
			Details:        "switched active_payment_route to backup",
		})
		return nil

	case ActionDisableUnhealthyProvider:
		provider := req.Parameters["provider"]
		if provider == "" {
			provider = "primary"
		}
		if provider != "primary" {
			return fmt.Errorf("%w: only unhealthy primary provider can be disabled", ErrPreconditionFailed)
		}
		if s.state.PrimaryProviderHealth {
			return fmt.Errorf("%w: primary provider is healthy; refusing to disable", ErrPreconditionFailed)
		}
		s.state.PrimaryProviderEnabled = false
		s.state.AuditEvents = append(s.state.AuditEvents, AuditEvent{
			ActionType:     req.ActionType,
			TargetResource: req.TargetResource,
			IdempotencyKey: req.IdempotencyKey,
			AppliedAt:      now,
			Details:        "disabled unhealthy primary payment provider",
		})
		return nil

	case ActionRestartSimulatedWorker:
		s.state.SimulatedWorkerHealthy = true
		s.state.WorkerRestartCount++
		s.state.AuditEvents = append(s.state.AuditEvents, AuditEvent{
			ActionType:     req.ActionType,
			TargetResource: req.TargetResource,
			IdempotencyKey: req.IdempotencyKey,
			AppliedAt:      now,
			Details:        "restarted simulated worker",
		})
		return nil

	default:
		return fmt.Errorf("%w: unsupported action %q", ErrInvalidAction, req.ActionType)
	}
}
