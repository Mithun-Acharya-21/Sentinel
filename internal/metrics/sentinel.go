package metrics

import (
	"database/sql"
	"time"
)

// SentinelMetrics holds all Prometheus-compatible HTTP, Worker, and PostgreSQL metrics.
type SentinelMetrics struct {
	Registry *Registry

	// HTTP metrics
	HTTPRequestsTotal       *CounterVec
	HTTPRequestDuration     *HistogramVec
	HTTPRateLimitRejections *CounterVec
	HTTPAuthFailures        *CounterVec

	// Worker metrics
	WorkerActive               *GaugeVec
	WorkerJobsClaimed          *CounterVec
	WorkerJobsCompleted        *CounterVec
	WorkerJobsFailed           *CounterVec
	WorkerJobsRetried          *CounterVec
	WorkerJobsDeadLettered     *CounterVec
	WorkerLeaseReclamations    *CounterVec
	WorkerStaleLeaseRejections *CounterVec
	WorkerErrors               *CounterVec
	WorkerJobDuration          *HistogramVec
	WorkerQueueDepth           *GaugeVec
	WorkerQueueOldestAge       *GaugeVec

	// PostgreSQL metrics
	DBPoolMaxOpen      *GaugeVec
	DBPoolOpen         *GaugeVec
	DBPoolInUse        *GaugeVec
	DBPoolIdle         *GaugeVec
	DBPoolWaitCount    *GaugeVec
	DBPoolWaitDuration *GaugeVec
	DBOperationErrors  *CounterVec
	DBOperationLatency *HistogramVec

	// Triage & Human Approval metrics
	TriageRequestsTotal         *CounterVec
	TriageSuccessTotal          *CounterVec
	TriageFailuresTotal         *CounterVec
	TriageProviderLatency       *HistogramVec
	TriageInvalidResponsesTotal *CounterVec
	ApprovalsPending            *GaugeVec
	ApprovalDecisionsTotal      *CounterVec

	// Controlled Remediation metrics
	RemediationRequestsTotal             *CounterVec
	RemediationAuthorizedTotal           *CounterVec
	RemediationRejectedTotal             *CounterVec
	RemediationExecutionsTotal           *CounterVec
	RemediationFailuresTotal             *CounterVec
	RemediationVerificationFailuresTotal *CounterVec
	RemediationDuration                  *HistogramVec
}

// NewSentinelMetrics creates and registers all Sentinel Prometheus metrics.
func NewSentinelMetrics() *SentinelMetrics {
	reg := NewRegistry()

	mustCounter := func(name, help string, labels ...string) *CounterVec {
		c, err := reg.RegisterCounter(name, help, labels...)
		if err != nil {
			panic(err)
		}
		return c
	}
	mustGauge := func(name, help string, labels ...string) *GaugeVec {
		g, err := reg.RegisterGauge(name, help, labels...)
		if err != nil {
			panic(err)
		}
		return g
	}
	mustHist := func(name, help string, bounds []float64, labels ...string) *HistogramVec {
		h, err := reg.RegisterHistogram(name, help, bounds, labels...)
		if err != nil {
			panic(err)
		}
		return h
	}

	return &SentinelMetrics{
		Registry: reg,

		HTTPRequestsTotal:       mustCounter("sentinel_http_requests_total", "Total HTTP requests processed by method, route, and status code.", "method", "route", "status"),
		HTTPRequestDuration:     mustHist("sentinel_http_request_duration_seconds", "HTTP request duration in seconds.", DefaultDurationBuckets, "method", "route"),
		HTTPRateLimitRejections: mustCounter("sentinel_http_rate_limit_rejections_total", "Total HTTP requests rejected by rate limiters.", "stage"),
		HTTPAuthFailures:        mustCounter("sentinel_http_auth_failures_total", "Total HTTP authentication failures by reason.", "reason"),

		WorkerActive:               mustGauge("sentinel_worker_active", "Number of currently active background workers."),
		WorkerJobsClaimed:          mustCounter("sentinel_worker_jobs_claimed_total", "Total incident processing jobs claimed by workers."),
		WorkerJobsCompleted:        mustCounter("sentinel_worker_jobs_completed_total", "Total incident processing jobs completed successfully."),
		WorkerJobsFailed:           mustCounter("sentinel_worker_jobs_failed_total", "Total incident processing job attempts that failed."),
		WorkerJobsRetried:          mustCounter("sentinel_worker_jobs_retried_total", "Total incident processing jobs scheduled for retry."),
		WorkerJobsDeadLettered:     mustCounter("sentinel_worker_jobs_dead_letter_total", "Total incident processing jobs transitioned to dead_letter."),
		WorkerLeaseReclamations:    mustCounter("sentinel_worker_lease_reclamations_total", "Total expired job leases reclaimed by workers."),
		WorkerStaleLeaseRejections: mustCounter("sentinel_worker_stale_lease_rejections_total", "Total stale worker completion/failure updates rejected due to lease loss."),
		WorkerErrors:               mustCounter("sentinel_worker_errors_total", "Total worker errors by kind.", "kind"),
		WorkerJobDuration:          mustHist("sentinel_worker_job_duration_seconds", "Duration of incident job processing in seconds.", DefaultDurationBuckets),
		WorkerQueueDepth:           mustGauge("sentinel_worker_queue_depth", "Number of queued or active incident jobs (pending, failed, processing)."),
		WorkerQueueOldestAge:       mustGauge("sentinel_worker_queue_oldest_age_seconds", "Age in seconds of the oldest ready incident job."),

		DBPoolMaxOpen:      mustGauge("sentinel_db_pool_max_open_connections", "Maximum number of open connections to PostgreSQL."),
		DBPoolOpen:         mustGauge("sentinel_db_pool_open_connections", "Number of established open connections to PostgreSQL."),
		DBPoolInUse:        mustGauge("sentinel_db_pool_in_use_connections", "Number of PostgreSQL connections currently in use."),
		DBPoolIdle:         mustGauge("sentinel_db_pool_idle_connections", "Number of idle PostgreSQL connections."),
		DBPoolWaitCount:    mustGauge("sentinel_db_pool_wait_count_total", "Total number of connections waited for."),
		DBPoolWaitDuration: mustGauge("sentinel_db_pool_wait_duration_seconds_total", "Total time blocked waiting for a new connection in seconds."),
		DBOperationErrors:  mustCounter("sentinel_db_operation_errors_total", "Total database operation errors by operation.", "operation"),
		DBOperationLatency: mustHist("sentinel_db_operation_duration_seconds", "Database operation duration in seconds.", DefaultDurationBuckets, "operation"),

		TriageRequestsTotal:         mustCounter("sentinel_triage_requests_total", "Total LLM incident triage requests initiated."),
		TriageSuccessTotal:          mustCounter("sentinel_triage_success_total", "Total LLM incident triage runs that succeeded and persisted."),
		TriageFailuresTotal:         mustCounter("sentinel_triage_failures_total", "Total LLM incident triage failures by reason.", "reason"),
		TriageProviderLatency:       mustHist("sentinel_triage_provider_duration_seconds", "Duration of LLM provider calls for incident triage in seconds.", DefaultDurationBuckets),
		TriageInvalidResponsesTotal: mustCounter("sentinel_triage_invalid_responses_total", "Total malformed or schema-invalid LLM triage responses rejected."),
		ApprovalsPending:            mustGauge("sentinel_triage_approvals_pending", "Number of triage recommendations currently pending human approval."),
		ApprovalDecisionsTotal:      mustCounter("sentinel_triage_approval_decisions_total", "Total human approval decisions recorded by outcome.", "decision"),

		RemediationRequestsTotal:             mustCounter("remediation_requests_total", "Total controlled remediation execution requests received."),
		RemediationAuthorizedTotal:           mustCounter("remediation_authorized_total", "Total remediation requests passing approval and allowlist authorization."),
		RemediationRejectedTotal:             mustCounter("remediation_rejected_total", "Total remediation requests rejected before execution.", "reason"),
		RemediationExecutionsTotal:           mustCounter("remediation_executions_total", "Total remediation executions completed by action_type and status.", "action_type", "status"),
		RemediationFailuresTotal:             mustCounter("remediation_failures_total", "Total remediation execution failures by category.", "category"),
		RemediationVerificationFailuresTotal: mustCounter("remediation_verification_failures_total", "Total remediation executions that failed independent post-action verification."),
		RemediationDuration:                  mustHist("remediation_duration_seconds", "Duration of controlled remediation execution and verification in seconds.", DefaultDurationBuckets),
	}
}

// RegisterDBPool samples sql.DBStats on every /metrics scrape.
func (m *SentinelMetrics) RegisterDBPool(db *sql.DB) {
	if m == nil || db == nil {
		return
	}
	m.Registry.OnScrape(func() {
		stats := db.Stats()
		m.DBPoolMaxOpen.Set(float64(stats.MaxOpenConnections))
		m.DBPoolOpen.Set(float64(stats.OpenConnections))
		m.DBPoolInUse.Set(float64(stats.InUse))
		m.DBPoolIdle.Set(float64(stats.Idle))
		m.DBPoolWaitCount.Set(float64(stats.WaitCount))
		m.DBPoolWaitDuration.Set(stats.WaitDuration.Seconds())
	})
}

// ObserveDBOperation records latency and optional error count for a PostgreSQL operation.
func (m *SentinelMetrics) ObserveDBOperation(operation string, duration time.Duration, err error) {
	if m == nil {
		return
	}
	m.DBOperationLatency.Observe(duration.Seconds(), operation)
	if err != nil {
		m.DBOperationErrors.Inc(operation)
	}
}
