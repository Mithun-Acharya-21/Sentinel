package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"sentinel/internal/evaluation/llmagent"
	"sentinel/internal/incident"
	"sentinel/internal/metrics"
	"sentinel/internal/worker"
)

const (
	DefaultMaxOutputTokens = 800
	DefaultApprovalTTL     = 24 * time.Hour
)

// ProcessorConfig configures the LLM-powered incident triage worker processor.
type ProcessorConfig struct {
	Provider        string
	Model           string
	MaxOutputTokens int
	ApprovalTTL     time.Duration
	Clock           func() time.Time
	Logger          *slog.Logger
	PromMetrics     *metrics.SentinelMetrics
}

// Processor implements worker.Processor using Sentinel's existing llmagent.Client
// to produce validated triage recommendations gated by human approval.
type Processor struct {
	incidents incident.Repository
	triage    Store
	llm       llmagent.Client
	cfg       ProcessorConfig
	clock     func() time.Time
	logger    *slog.Logger
	prom      *metrics.SentinelMetrics
}

// NewProcessor creates a new LLM incident triage processor.
func NewProcessor(incidents incident.Repository, triage Store, llm llmagent.Client, cfg ProcessorConfig) *Processor {
	if cfg.Provider == "" {
		cfg.Provider = llmagent.DefaultProvider
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if cfg.ApprovalTTL <= 0 {
		cfg.ApprovalTTL = DefaultApprovalTTL
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	p := &Processor{
		incidents: incidents,
		triage:    triage,
		llm:       llm,
		cfg:       cfg,
		clock:     cfg.Clock,
		logger:    cfg.Logger,
		prom:      cfg.PromMetrics,
	}

	if p.prom != nil && p.triage != nil {
		p.prom.Registry.OnScrape(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if count, err := p.triage.PendingApprovalCount(ctx); err == nil {
				p.prom.ApprovalsPending.Set(float64(count))
			}
		})
	}

	return p
}

var _ worker.Processor = (*Processor)(nil)

// Process executes tenant-scoped, idempotent LLM triage for a claimed incident job.
// No database transaction is held open during the external LLM request.
func (p *Processor) Process(ctx context.Context, job worker.Job) error {
	if p == nil || p.incidents == nil || p.triage == nil || p.llm == nil {
		return worker.MarkNonRetryable(errors.New("triage processor dependencies are not configured"))
	}

	// 1. Idempotency check: if triage already exists for this incident, do not call the LLM again.
	if _, err := p.triage.GetByIncidentID(ctx, job.TenantID, job.IncidentID); err == nil {
		p.logger.Info("triage already persisted for incident; skipping duplicate LLM call",
			slog.String("job_id", job.ID),
			slog.String("incident_id", job.IncidentID),
		)
		return nil
	} else if !errors.Is(err, ErrTriageNotFound) {
		if p.prom != nil {
			p.prom.TriageFailuresTotal.Inc("storage_error")
		}
		return err
	}

	// 2. Load incident using strictly tenant-scoped identity.
	inc, err := p.incidents.Get(ctx, job.TenantID, job.IncidentID)
	if err != nil {
		if errors.Is(err, incident.ErrNotFound) {
			if p.prom != nil {
				p.prom.TriageFailuresTotal.Inc("incident_not_found")
			}
			return worker.MarkNonRetryable(err)
		}
		if p.prom != nil {
			p.prom.TriageFailuresTotal.Inc("storage_error")
		}
		return err
	}
	if inc.TenantID != job.TenantID {
		return worker.MarkNonRetryable(errors.New("incident tenant mismatch"))
	}

	// 3. Build bounded model request with NO tools (cannot execute remediation).
	if p.prom != nil {
		p.prom.TriageRequestsTotal.Inc()
	}
	req, err := buildTriageRequest(p.cfg.Model, p.cfg.MaxOutputTokens, inc)
	if err != nil {
		return worker.MarkNonRetryable(err)
	}

	// 4. Invoke existing LLM client (reuses bounded retries, backoff, and safe HTTPError).
	start := p.clock()
	resp, err := p.llm.Complete(ctx, req)
	providerDuration := p.clock().Sub(start)
	if p.prom != nil && providerDuration >= 0 {
		p.prom.TriageProviderLatency.Observe(providerDuration.Seconds())
	}
	if err != nil {
		if p.prom != nil {
			p.prom.TriageFailuresTotal.Inc("provider_error")
		}
		return err
	}
	if len(resp.ToolCalls) > 0 {
		if p.prom != nil {
			p.prom.TriageInvalidResponsesTotal.Inc()
			p.prom.TriageFailuresTotal.Inc("invalid_model_output")
		}
		return fmt.Errorf("%w: model attempted unauthorized tool call", ErrInvalidModelOutput)
	}

	// 5. Parse and strictly validate structured JSON output.
	parsed, err := ParseAndValidateModelOutput(resp.Content)
	if err != nil {
		if p.prom != nil {
			p.prom.TriageInvalidResponsesTotal.Inc()
			p.prom.TriageFailuresTotal.Inc("invalid_model_output")
		}
		return err
	}

	// 6. Run independent verifier (separates task correctness from safety).
	now := p.clock().UTC()
	report := VerifyProposal(inc, parsed, nil, now)
	if !report.TaskPassed || !report.SafetyPassed {
		if p.prom != nil {
			p.prom.TriageInvalidResponsesTotal.Inc()
			p.prom.TriageFailuresTotal.Inc("invalid_model_output")
		}
		return fmt.Errorf("%w: triage failed independent safety/task verification", ErrInvalidModelOutput)
	}

	// 7. Persist triage result + pending_review human approval record atomically.
	expiresAt := now.Add(p.cfg.ApprovalTTL)
	result := TriageResult{
		TenantID:              inc.TenantID,
		IncidentID:            inc.ID,
		JobID:                 job.ID,
		ContractVersion:       ContractVersion,
		PromptVersion:         PromptVersion,
		Model:                 p.cfg.Model,
		Classification:        parsed.Classification,
		RecommendedSeverity:   parsed.RecommendedSeverity,
		Confidence:            parsed.Confidence,
		Evidence:              parsed.Evidence,
		SuspectedRootCause:    parsed.SuspectedRootCause,
		RecommendedSteps:      parsed.RecommendedSteps,
		ProposedAction:        parsed.ProposedAction,
		RequiresHumanApproval: true,
		Metadata: ProcessingMetadata{
			Model:             p.cfg.Model,
			Provider:          p.cfg.Provider,
			PromptVersion:     PromptVersion,
			ContractVersion:   ContractVersion,
			Attempts:          resp.Attempts,
			ProviderLatencyMs: providerDuration.Milliseconds(),
			Usage:             resp.Usage,
		},
		CreatedAt: now,
	}
	approval := ApprovalRecord{
		TenantID:       inc.TenantID,
		IncidentID:     inc.ID,
		Status:         ApprovalPendingReview,
		ProposedAction: parsed.ProposedAction,
		ExpiresAt:      &expiresAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if _, err := p.triage.SaveTriageAndApproval(ctx, result, approval); err != nil {
		if p.prom != nil {
			p.prom.TriageFailuresTotal.Inc("storage_error")
		}
		return err
	}

	if p.prom != nil {
		p.prom.TriageSuccessTotal.Inc()
	}
	p.logger.Info("incident triage persisted awaiting human approval",
		slog.String("job_id", job.ID),
		slog.String("incident_id", inc.ID),
		slog.String("classification", string(parsed.Classification)),
		slog.String("proposed_action", string(parsed.ProposedAction)),
	)
	return nil
}

func buildTriageRequest(model string, maxTokens int, inc incident.Incident) (llmagent.CompletionRequest, error) {
	payload := struct {
		Service     string            `json:"service"`
		Title       string            `json:"title"`
		Description string            `json:"description"`
		Severity    incident.Severity `json:"severity"`
		Source      string            `json:"source"`
		OccurredAt  time.Time         `json:"occurred_at"`
	}{
		Service:     inc.Service,
		Title:       inc.Title,
		Description: inc.Description,
		Severity:    inc.Severity,
		Source:      inc.Source,
		OccurredAt:  inc.OccurredAt,
	}
	userBytes, err := json.Marshal(payload)
	if err != nil {
		return llmagent.CompletionRequest{}, err
	}

	systemPrompt := `You are Sentinel's incident triage analyst. Analyze the incident JSON and respond with ONLY a single valid JSON object matching this exact schema (no markdown, no code fences, no extra fields):
{
  "classification": "availability_outage|latency_degradation|error_spike|dependency_failure|security_anomaly|capacity_exhaustion|unknown",
  "recommended_severity": "low|medium|high|critical",
  "confidence": 0.0,
  "evidence": ["factual observation from the incident"],
  "suspected_root_cause": "concise hypothesis",
  "recommended_steps": ["safe investigation or mitigation step"],
  "proposed_action": "none|route_failover|scale_service|rollback_deploy|rate_limit_caller|manual_investigation",
  "requires_human_approval": true
}
You must never claim an action has been executed or approved.`

	return llmagent.CompletionRequest{
		Model:           model,
		MaxOutputTokens: maxTokens,
		Tools:           nil, // Explicitly zero tools: model cannot invoke any remediation.
		Messages: []llmagent.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: string(userBytes)},
		},
	}, nil
}
