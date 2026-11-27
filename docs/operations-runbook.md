# Sentinel Operations Runbook

## 1. Diagnosing a Growing Job Queue
- **Symptoms:** `sentinel_worker_queue_depth` rising and `sentinel_worker_queue_oldest_age_seconds` > 60s.
- **Checks:**
  1. Verify `sentinel_worker_active > 0` (check `SENTINEL_WORKER_COUNT`).
  2. Inspect `histogram_quantile(0.95, rate(sentinel_worker_job_duration_seconds_bucket[5m]))` for slow downstream processing.
  3. Query PostgreSQL job status breakdown:
     ```sql
     SELECT status, count(*), min(available_at)
     FROM incident_jobs
     GROUP BY status;
     ```
- **Mitigation:** Increase `SENTINEL_WORKER_COUNT` (ensuring `MaxOpenConns` has headroom above worker + HTTP concurrency) or reduce `SENTINEL_WORKER_POLL_INTERVAL`.

## 2. Investigating Repeated Retries
- **Symptoms:** `rate(sentinel_worker_jobs_retried_total[5m])` elevated.
- **Checks:**
  1. Check structured logs for `"incident job scheduled for retry"` (contains `worker_id`, `job_id`, `attempt`, `available_at`).
  2. Inspect `last_error` in PostgreSQL:
     ```sql
     SELECT id, tenant_id, attempts, available_at, last_error
     FROM incident_jobs
     WHERE status = 'failed'
     ORDER BY updated_at DESC
     LIMIT 20;
     ```
  3. Check `sentinel_worker_errors_total{kind="panic"}` for recovered processor panics.

## 3. Investigating Dead-Lettered Jobs
- **Symptoms:** `sentinel_worker_jobs_dead_letter_total` incrementing.
- **Checks:**
  ```sql
  SELECT id, tenant_id, incident_id, attempts, max_attempts, last_error, updated_at
  FROM incident_jobs
  WHERE status = 'dead_letter'
  ORDER BY updated_at DESC
  LIMIT 50;
  ```
- **Recovery:** After resolving the underlying bug or downstream outage, re-queue a dead-lettered job manually:
  ```sql
  UPDATE incident_jobs
  SET status = 'pending', attempts = 0, available_at = now(), last_error = NULL, updated_at = now()
  WHERE id = '<job-uuid>' AND status = 'dead_letter';
  ```

## 4. Diagnosing Database Connection Exhaustion
- **Symptoms:** `sentinel_db_pool_in_use_connections == sentinel_db_pool_max_open_connections`, `rate(sentinel_db_pool_wait_count_total[5m]) > 0`, HTTP `503 storage_unavailable` or `503 auth_unavailable`.
- **Checks:**
  1. Check `sentinel_db_operation_duration_seconds` for slow queries.
  2. Inspect active PostgreSQL queries (`pg_stat_activity`) for locks or long transactions.
  3. Confirm pre-auth IP rate limiting (`SENTINEL_PREAUTH_RATE_LIMIT_*`) is enabled so unauthenticated credential floods are dropped before reaching `api_credentials`.

## 5. Identifying Worker Lease Problems
- **Symptoms:** `sentinel_worker_lease_reclamations_total` or `sentinel_worker_stale_lease_rejections_total` rising.
- **Cause:** Workers are taking longer than `SENTINEL_WORKER_LEASE_DURATION` (default `30s`) to process jobs, or worker processes are crashing mid-job.
- **Mitigation:** Increase `SENTINEL_WORKER_LEASE_DURATION` above the p99 `sentinel_worker_job_duration_seconds`, and ensure downstream processors are idempotent (`incident_job_effects`).

## 6. Safe Restart & Recovery Procedures
1. Send `SIGTERM` to `sentinel-api`.
2. `ReadinessState.SetDraining(true)` immediately flips `GET /health/ready` to `503 {"status":"draining"}` while `GET /health/live` remains `200`.
3. `server.Shutdown` drains in-flight HTTP requests.
4. `workerEngine.Stop` stops new job claims, waits up to `shutdownTimeout` (`10s`) for in-flight jobs, and releases interrupted leases back to `pending`.
5. `database.Close()` executes only after workers have stopped.

## 7. Current Limitations & Known Operational Risks
- **At-Least-Once Processing:** Downstream processors must remain idempotent (`incident_job_effects`); Sentinel does not provide distributed two-phase commit for external side effects.
- **Process-Local Rate Limits:** Token buckets are per-process; multi-replica deployments scale effective limits by replica count.
- **No Built-In Alerting / Dead-Letter Replay CLI:** Operators must configure Prometheus alerts and manually re-queue `dead_letter` rows.
