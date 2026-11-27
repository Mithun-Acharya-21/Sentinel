# ADR-015: Zero-Dependency Prometheus Exposition & Readiness Draining

## Status
Accepted

## Date
2026-11-27

## Context
During Milestone 3B's repository audit, Sentinel had internal atomic metric snapshots (`Limiter.Metrics()`, `StoreVerifier.Metrics()`, `worker.Metrics`), but **no** `/metrics` HTTP endpoint, **no** HTTP request/latency/status metrics, **no** PostgreSQL pool/latency metrics, **no** worker panic recovery, and **no** draining state on `/health/ready`.

## Decision

### 1. Zero-Dependency Prometheus Text Exposition (`internal/metrics`)
We implemented a lightweight Prometheus `0.0.4` text exposition registry (`CounterVec`, `GaugeVec`, `HistogramVec`, `OnScrape` hooks) using only the Go standard library rather than adding `github.com/prometheus/client_golang`.
- **Label Cardinality Guard:** `RegisterCounter`, `RegisterGauge`, and `RegisterHistogram` explicitly reject forbidden high-cardinality or sensitive label names (`tenant_id`, `incident_id`, `job_id`, `api_key`, `key_id`, `token`, `secret`, `principal_id`, `ip`, `client_ip`, `remote_addr`), and `normalizeMetricRoute` collapses UUID paths into `/v1/incidents/{id}`.
- **On-Scrape Sampling:** `sql.DB.Stats()`, `QueueDepth`, and `OldestReadyJobAgeSeconds` are sampled on `/metrics` scrape rather than running extra background polling goroutines.

### 2. Readiness Draining Semantics (`ReadinessHandlerWithState`)
`GET /health/live` remains strictly process-local (`200 ok`). `GET /health/ready` checks `ReadinessState.IsDraining()` first (returning `503 {"status":"draining"}` immediately upon `SIGTERM`) before pinging PostgreSQL with a bounded `1s` timeout.

### 3. Worker Panic Recovery (`runProcessorSafely`)
`Engine.ProcessOne` wraps `processor.Process` in `defer recover()`, converting panics into errors, incrementing `sentinel_worker_errors_total{kind="panic"}`, and scheduling bounded retries instead of crashing the API/worker process.

## Consequences
- Sentinel exposes full HTTP, Worker, and PostgreSQL Prometheus metrics with zero new external dependencies.
- If OpenTelemetry or native histograms are required later, `internal/metrics` can be swapped behind `SentinelMetrics` without changing middleware or worker call sites.
