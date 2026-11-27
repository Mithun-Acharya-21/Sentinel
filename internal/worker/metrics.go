package worker

import "sync/atomic"

// MetricsSnapshot holds bounded-cardinality worker execution telemetry.
// No tenant IDs, incident IDs, or job IDs are used as labels.
type MetricsSnapshot struct {
	JobsCompleted        uint64  `json:"jobs_completed"`
	JobsFailed           uint64  `json:"jobs_failed"`
	JobsRetried          uint64  `json:"jobs_retried"`
	JobsDeadLettered     uint64  `json:"jobs_dead_lettered"`
	JobsProcessing       int64   `json:"jobs_processing"`
	LeaseRecoveries      uint64  `json:"lease_recoveries"`
	StaleLeaseRejections uint64  `json:"stale_lease_rejections"`
	AvgProcessingMillis  float64 `json:"avg_processing_ms"`
	ProcessedTotal       uint64  `json:"processed_total"`
	StoreErrors          uint64  `json:"store_errors"`
	WorkerPanics         uint64  `json:"worker_panics"`
}

// Metrics tracks thread-safe atomic counters for the worker engine.
type Metrics struct {
	jobsCompleted        atomic.Uint64
	jobsFailed           atomic.Uint64
	jobsRetried          atomic.Uint64
	jobsDeadLettered     atomic.Uint64
	jobsProcessing       atomic.Int64
	leaseRecoveries      atomic.Uint64
	staleLeaseRejections atomic.Uint64
	processingTotalNs    atomic.Uint64
	processedTotal       atomic.Uint64
	storeErrors          atomic.Uint64
	workerPanics         atomic.Uint64
}

// Snapshot returns a point-in-time copy of worker metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	count := m.processedTotal.Load()
	totalNs := m.processingTotalNs.Load()
	var avgMs float64
	if count > 0 {
		avgMs = (float64(totalNs) / float64(count)) / 1e6
	}
	return MetricsSnapshot{
		JobsCompleted:        m.jobsCompleted.Load(),
		JobsFailed:           m.jobsFailed.Load(),
		JobsRetried:          m.jobsRetried.Load(),
		JobsDeadLettered:     m.jobsDeadLettered.Load(),
		JobsProcessing:       m.jobsProcessing.Load(),
		LeaseRecoveries:      m.leaseRecoveries.Load(),
		StaleLeaseRejections: m.staleLeaseRejections.Load(),
		AvgProcessingMillis:  avgMs,
		ProcessedTotal:       count,
		StoreErrors:          m.storeErrors.Load(),
		WorkerPanics:         m.workerPanics.Load(),
	}
}
