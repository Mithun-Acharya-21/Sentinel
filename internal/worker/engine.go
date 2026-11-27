package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultWorkerCount   = 2
	DefaultPollInterval  = 500 * time.Millisecond
	DefaultLeaseDuration = 30 * time.Second
	DefaultShutdownGrace = 10 * time.Second
	releaseTimeout       = 3 * time.Second
	maxErrorLen          = 512
)

// Processor executes the downstream work for a claimed incident job.
// Implementations must be idempotent because at-least-once delivery is possible
// if a worker crashes after performing an external action but before committing completion.
type Processor interface {
	Process(ctx context.Context, job Job) error
}

// DefaultIncidentProcessor records an idempotent triage effect in PostgreSQL using a
// deterministic idempotency key so duplicate executions do not duplicate external effects.
type DefaultIncidentProcessor struct {
	store JobStore
}

// NewDefaultIncidentProcessor creates the default idempotent incident processor.
func NewDefaultIncidentProcessor(store JobStore) *DefaultIncidentProcessor {
	return &DefaultIncidentProcessor{store: store}
}

// Process executes idempotent incident processing for the given job.
func (p *DefaultIncidentProcessor) Process(ctx context.Context, job Job) error {
	if p == nil || p.store == nil {
		return errors.New("job store is not configured")
	}
	idempotencyKey := fmt.Sprintf("job:%s:incident:%s:triage", job.ID, job.IncidentID)
	alreadyExecuted, err := p.store.RecordEffect(ctx, idempotencyKey, job.ID, job.TenantID, "triage_recorded", "incident accepted and triaged")
	if err != nil {
		return err
	}
	if alreadyExecuted {
		return nil
	}
	return nil
}

// Config configures the durable background worker engine.
type Config struct {
	WorkerCount   int
	PollInterval  time.Duration
	LeaseDuration time.Duration
	ShutdownGrace time.Duration
	RetryPolicy   RetryPolicy
	Clock         func() time.Time
	Logger        *slog.Logger
}

// Engine polls for durable jobs, claims them with leases, executes them with bounded
// concurrency, and handles retries, dead-lettering, lease recovery, and graceful shutdown.
type Engine struct {
	store     JobStore
	processor Processor
	cfg       Config
	clock     func() time.Time
	logger    *slog.Logger
	metrics   *Metrics

	mu         sync.Mutex
	started    bool
	pollCancel context.CancelFunc
	jobCancel  context.CancelFunc
	wg         sync.WaitGroup
	stopped    atomic.Bool
}

// NewEngine creates a durable worker Engine.
func NewEngine(store JobStore, processor Processor, cfg Config) *Engine {
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = DefaultWorkerCount
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = DefaultLeaseDuration
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = DefaultShutdownGrace
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if processor == nil {
		processor = NewDefaultIncidentProcessor(store)
	}

	return &Engine{
		store:     store,
		processor: processor,
		cfg:       cfg,
		clock:     cfg.Clock,
		logger:    cfg.Logger,
		metrics:   &Metrics{},
	}
}

// Metrics returns a point-in-time snapshot of worker observability metrics.
func (e *Engine) Metrics() MetricsSnapshot {
	return e.metrics.Snapshot()
}

// Start launches the configured number of worker goroutines.
func (e *Engine) Start(parentCtx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return
	}
	e.started = true

	pollCtx, pollCancel := context.WithCancel(parentCtx)
	jobCtx, jobCancel := context.WithCancel(parentCtx)
	e.pollCancel = pollCancel
	e.jobCancel = jobCancel

	for i := 0; i < e.cfg.WorkerCount; i++ {
		workerID := fmt.Sprintf("worker-%d", i+1)
		e.wg.Add(1)
		go e.runWorker(pollCtx, jobCtx, workerID)
	}
}

// Stop cancels polling for new jobs, waits for in-flight jobs up to ShutdownGrace,
// and cancels remaining job contexts before returning.
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	if !e.started || !e.stopped.CompareAndSwap(false, true) {
		e.mu.Unlock()
		return nil
	}
	pollCancel := e.pollCancel
	jobCancel := e.jobCancel
	e.mu.Unlock()

	pollCancel()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	graceCtx, cancelGrace := context.WithTimeout(ctx, e.cfg.ShutdownGrace)
	defer cancelGrace()

	select {
	case <-done:
		jobCancel()
		return nil
	case <-graceCtx.Done():
		jobCancel()
		<-done
		return graceCtx.Err()
	}
}

func (e *Engine) runWorker(pollCtx, jobCtx context.Context, workerID string) {
	defer e.wg.Done()

	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()

	for {
		if pollCtx.Err() != nil {
			return
		}

		processed, err := e.ProcessOne(pollCtx, jobCtx, workerID)
		if err != nil && pollCtx.Err() == nil {
			e.logger.Error("worker cycle error", slog.String("worker_id", workerID), slog.String("error", sanitizeError(err)))
		}
		if processed && pollCtx.Err() == nil {
			// Drain immediately if work was available instead of sleeping PollInterval.
			continue
		}

		select {
		case <-pollCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOne attempts to reclaim an expired lease or claim the next ready job and execute it once.
// Exported for deterministic unit and integration testing without relying on polling sleeps.
func (e *Engine) ProcessOne(pollCtx, jobCtx context.Context, workerID string) (bool, error) {
	if pollCtx.Err() != nil {
		return false, nil
	}

	now := e.clock().UTC()

	// 1. Recover expired leases first so abandoned work is not starved.
	job, claimed, err := e.store.ReclaimExpired(pollCtx, workerID, e.cfg.LeaseDuration, now)
	if err != nil {
		if pollCtx.Err() != nil {
			return false, nil
		}
		e.metrics.storeErrors.Add(1)
		return false, err
	}
	if claimed {
		e.metrics.leaseRecoveries.Add(1)
		e.logger.Warn("reclaimed expired job lease",
			slog.String("worker_id", workerID),
			slog.String("job_id", job.ID),
			slog.Int("attempt", job.Attempts),
		)
	} else {
		// 2. Otherwise claim the next ready pending/failed job.
		job, claimed, err = e.store.ClaimNext(pollCtx, workerID, e.cfg.LeaseDuration, now)
		if err != nil {
			if pollCtx.Err() != nil {
				return false, nil
			}
			e.metrics.storeErrors.Add(1)
			return false, err
		}
		if !claimed {
			return false, nil
		}
	}

	e.metrics.jobsProcessing.Add(1)
	defer e.metrics.jobsProcessing.Add(-1)

	// If lease recovery pushed attempts past MaxAttempts, dead-letter immediately without re-executing.
	if job.Attempts > job.MaxAttempts {
		failErr := e.store.Fail(jobCtx, job.ID, job.LeaseToken, "lease expired after max attempts", now, true, now)
		if errors.Is(failErr, ErrLeaseLost) {
			e.metrics.staleLeaseRejections.Add(1)
			return true, nil
		}
		if failErr != nil {
			e.metrics.storeErrors.Add(1)
			return true, failErr
		}
		e.metrics.jobsDeadLettered.Add(1)
		return true, nil
	}

	start := e.clock()
	procErr := e.processor.Process(jobCtx, job)
	duration := e.clock().Sub(start)
	if duration > 0 {
		e.metrics.processingTotalNs.Add(uint64(duration.Nanoseconds()))
	}
	e.metrics.processedTotal.Add(1)

	finishTime := e.clock().UTC()
	if procErr == nil {
		compErr := e.store.Complete(jobCtx, job.ID, job.LeaseToken, finishTime)
		if errors.Is(compErr, ErrLeaseLost) {
			e.metrics.staleLeaseRejections.Add(1)
			e.logger.Warn("stale worker completion rejected",
				slog.String("worker_id", workerID),
				slog.String("job_id", job.ID),
			)
			return true, nil
		}
		if compErr != nil {
			e.metrics.storeErrors.Add(1)
			return true, compErr
		}
		e.metrics.jobsCompleted.Add(1)
		e.logger.Info("incident job completed",
			slog.String("worker_id", workerID),
			slog.String("job_id", job.ID),
			slog.Int("attempt", job.Attempts),
		)
		return true, nil
	}

	// If shutting down and processor aborted due to context cancellation, release lease cleanly.
	if errors.Is(procErr, context.Canceled) && jobCtx.Err() != nil {
		relCtx, cancelRel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancelRel()
		if relErr := e.store.Release(relCtx, job.ID, job.LeaseToken, finishTime); relErr != nil && !errors.Is(relErr, ErrLeaseLost) {
			e.metrics.storeErrors.Add(1)
		}
		return true, nil
	}

	e.metrics.jobsFailed.Add(1)
	deadLetter := !IsRetryable(procErr) || job.Attempts >= job.MaxAttempts
	nextAvailableAt := finishTime
	if !deadLetter {
		nextAvailableAt = finishTime.Add(e.cfg.RetryPolicy.NextDelay(job.Attempts))
	}

	failErr := e.store.Fail(jobCtx, job.ID, job.LeaseToken, sanitizeError(procErr), nextAvailableAt, deadLetter, finishTime)
	if errors.Is(failErr, ErrLeaseLost) {
		e.metrics.staleLeaseRejections.Add(1)
		e.logger.Warn("stale worker failure update rejected",
			slog.String("worker_id", workerID),
			slog.String("job_id", job.ID),
		)
		return true, nil
	}
	if failErr != nil {
		e.metrics.storeErrors.Add(1)
		return true, failErr
	}

	if deadLetter {
		e.metrics.jobsDeadLettered.Add(1)
		e.logger.Error("incident job dead-lettered",
			slog.String("worker_id", workerID),
			slog.String("job_id", job.ID),
			slog.Int("attempt", job.Attempts),
		)
	} else {
		e.metrics.jobsRetried.Add(1)
		e.logger.Warn("incident job scheduled for retry",
			slog.String("worker_id", workerID),
			slog.String("job_id", job.ID),
			slog.Int("attempt", job.Attempts),
			slog.Time("available_at", nextAvailableAt),
		)
	}
	return true, nil
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > maxErrorLen {
		return msg[:maxErrorLen]
	}
	return msg
}
