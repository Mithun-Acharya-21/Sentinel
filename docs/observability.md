# Sentinel Observability Guide

**Endpoint:** `GET /metrics` (Prometheus text exposition format `0.0.4`)  
**Health Probes:** `GET /health/live` (liveness), `GET /health/ready` (readiness)

> **Security Note:** `/metrics` is exempt from API tenant authentication and rate limiting so Prometheus scrapers can collect telemetry reliably. Restrict `/metrics` access to internal monitoring networks at the ingress/load-balancer layer. No tenant IDs, incident IDs, job IDs, API keys, or client IPs are ever emitted as metric labels.

---

## 1. Available Metrics

### HTTP Metrics
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `sentinel_http_requests_total` | Counter | `method`, `route`, `status` | Total HTTP requests by normalized route (`/v1/incidents`, `/v1/incidents/{id}`, `/other`). |
| `sentinel_http_request_duration_seconds` | Histogram | `method`, `route` | HTTP request latency distribution. |
| `sentinel_http_rate_limit_rejections_total` | Counter | `stage` | Rate-limit rejections (`pre_auth_ip` or `principal`). |
| `sentinel_http_auth_failures_total` | Counter | `reason` | Authentication rejections (`missing`, `invalid`, `revoked`, `expired`, `unavailable`). |

### Worker Metrics
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `sentinel_worker_active` | Gauge | — | Number of currently running worker goroutines. |
| `sentinel_worker_jobs_claimed_total` | Counter | — | Total jobs claimed or reclaimed by workers. |
| `sentinel_worker_jobs_completed_total` | Counter | — | Total jobs completed successfully. |
| `sentinel_worker_jobs_failed_total` | Counter | — | Total failed job execution attempts. |
| `sentinel_worker_jobs_retried_total` | Counter | — | Total attempts scheduled for exponential backoff retry. |
| `sentinel_worker_jobs_dead_letter_total` | Counter | — | Total jobs moved to `dead_letter`. |
| `sentinel_worker_lease_reclamations_total` | Counter | — | Total expired `processing` leases reclaimed by another worker. |
| `sentinel_worker_stale_lease_rejections_total` | Counter | — | Total stale worker updates rejected by `lease_token` fencing. |
| `sentinel_worker_errors_total` | Counter | `kind` | Worker errors (`store`, `panic`). |
| `sentinel_worker_job_duration_seconds` | Histogram | — | Downstream job processing latency. |
| `sentinel_worker_queue_depth` | Gauge | — | Queued/active jobs (`pending`, `failed`, `processing`), sampled on scrape. |
| `sentinel_worker_queue_oldest_age_seconds` | Gauge | — | Age of the oldest ready job, sampled on scrape. |

### PostgreSQL Metrics
| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `sentinel_db_pool_max_open_connections` | Gauge | — | Configured `MaxOpenConns`. |
| `sentinel_db_pool_open_connections` | Gauge | — | Current open connections (`InUse + Idle`). |
| `sentinel_db_pool_in_use_connections` | Gauge | — | Connections currently executing queries/transactions. |
| `sentinel_db_pool_idle_connections` | Gauge | — | Idle connections in the pool. |
| `sentinel_db_pool_wait_count_total` | Gauge | — | Cumulative waits for a free pool connection. |
| `sentinel_db_pool_wait_duration_seconds_total` | Gauge | — | Cumulative time spent waiting for a pool connection. |
| `sentinel_db_operation_errors_total` | Counter | `operation` | DB errors by operation (`incident_create`, `incident_get`, `job_queue_depth`, `job_oldest_age`). |
| `sentinel_db_operation_duration_seconds` | Histogram | `operation` | DB operation latency by operation. |

---

## 2. Health & Readiness Endpoints

| Endpoint | Healthy | Unhealthy / Draining | Semantics |
|---|---|---|---|
| `GET /health/live` | `200 {"status":"ok"}` | — | Process is alive; **never** fails due to PostgreSQL outages. |
| `GET /health/ready` | `200 {"status":"ready"}` | `503 {"status":"not_ready"}` or `503 {"status":"draining"}` | Checks `ReadinessState` (draining on shutdown) and pings PostgreSQL with a `1s` timeout. Never leaks SQL/connection errors. |

---

## 3. Example PromQL Queries

**HTTP 5xx Error Rate:**
```promql
sum(rate(sentinel_http_requests_total{status=~"5.."}[5m]))
  / sum(rate(sentinel_http_requests_total[5m]))
```

**p95 HTTP Request Latency:**
```promql
histogram_quantile(0.95, sum(rate(sentinel_http_request_duration_seconds_bucket[5m])) by (le, route))
```

**Growing Job Queue / Lagging Workers:**
```promql
sentinel_worker_queue_depth > 100 and sentinel_worker_queue_oldest_age_seconds > 60
```

**Dead-Letter & Retry Rate:**
```promql
rate(sentinel_worker_jobs_dead_letter_total[5m])
rate(sentinel_worker_jobs_retried_total[5m])
```

**PostgreSQL Connection Pool Saturation:**
```promql
sentinel_db_pool_in_use_connections / sentinel_db_pool_max_open_connections > 0.85
```
