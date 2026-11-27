# Sentinel — Quality Baseline

Date: 2026-11-26  
Branch: `portfolio-transformation`  
Commit: clean working tree based on `origin/main`  

---

## 1. Automated Verification Command Baseline

| Check / Test Command | Exit Code | Outcome | Execution Details |
|---|---|---|---|
| `go test -count=1 ./...` | `0` | **PASS** | Evaluated 6 packages (`config`, `evaluation`, `evaluation/llmagent`, `httpapi`, `postgres`). Total time: ~18s. |
| `go vet ./...` | `0` | **PASS** | 0 warnings, 0 issues detected. |
| `gofmt -l .` | `0` | **PASS** | 0 unformatted Go files. |
| `go run ./cmd/sentinel-eval -runs-per-agent 1 -out -` | `0` | **PASS** | Produces valid JSON report with task `payment-route-failover-v1` v1.1.0; 3 scheduled, 3 completed, pass rate 1/3 (0.333). |
| Docker daemon status | `1` | **UNAVAILABLE** | Docker desktop/daemon is not running with elevated privileges on this local Windows machine. Docker-dependent integration tests and container actions are skipped cleanly. |
| PostgreSQL integration test (`incidents_integration_test.go`) | `0` | **SKIPPED** | `SENTINEL_TEST_DATABASE_URL` unset. Tests skip with clear log notice rather than failing. |

---

## 2. Static Code & Artifact Inventory

- **Go Code**:
  - `cmd/sentinel-api/main.go`: 119 lines. HTTP daemon entrypoint.
  - `cmd/sentinel-eval/main.go`: 94 lines. CLI evaluation runner entrypoint.
  - `internal/config`: Configuration loader for environment variables with type parsing.
  - `internal/incident`: Core domain model and repository interface.
  - `internal/storage/postgres`: PostgreSQL driver implementation using `database/sql` + pgx.
  - `internal/httpapi`: HTTP handlers, router, JSON decoding, error response mapping.
  - `internal/evaluation`: Benchmark task contracts, simulator, runner, harness, aggregation.
  - `internal/evaluation/verifier`: Independent snapshot and audit verification.
  - `internal/evaluation/llmagent`: OpenAI Chat Completions client and tool dispatcher.
- **Historical Reports**:
  - `sentinel-groq-gpt-oss-20b-increment8.json` (Task v1.0.0, 1 run, 100% pass)
  - `sentinel-groq-gpt-oss-20b-increment9.json` (Task v1.0.0, 5 runs, 3 completed, 2 provider errors)
  - `sentinel-groq-gpt-oss-20b-increment12.json` (Task v1.1.0, 5 runs, 2 completed, 3 provider errors)
  - `sentinel-groq-gpt-oss-20b-increment14-probe.json` (Task v1.1.0, 1 run, 1 completed pass)
  - `sentinel-increment13-smoke.json` (Task v1.1.0, 3 runs, scripted baseline smoke)

All historical evaluation artifacts are preserved and intact.
