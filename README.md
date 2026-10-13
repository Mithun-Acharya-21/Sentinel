# Sentinel

Sentinel is a Go project for collecting synthetic incidents and evaluating a narrowly scoped incident-response agent. It pairs a PostgreSQL-backed incident API with a separate deterministic payment-route failover simulator. The verifier grades simulator state and audited actions rather than trusting an agent's claim that it succeeded.

> **Scope:** This is a learning and portfolio project, not a production incident-management system. The simulator and its tool boundary are in-process; they are not a security sandbox.

## Engineering features

- Go HTTP endpoints for creating and retrieving incidents.
- PostgreSQL persistence, versioned SQL migration, and source-event idempotency.
- Request validation, context-aware repository calls, and JSON error responses.
- Repeatable task runs with deterministic baseline agents and a restricted-tool LLM adapter.
- Independent task and safety checks, version-aware aggregation, and explicit unscored handling for provider/environment/verifier interruptions.
- Unit, adversarial evaluation, mock-provider, and optional PostgreSQL integration tests.

## Architecture

See [the architecture and data-flow guide](docs/architecture.md) for the component diagram and explanation. Incident persistence and the synthetic evaluation simulator are separate paths: the benchmark does not write incidents to PostgreSQL.

## Repository layout

```text
cmd/
  sentinel-api/       PostgreSQL-backed HTTP service
  sentinel-eval/      deterministic or optional LLM evaluation CLI
internal/
  config/             API environment configuration
  incident/           incident types, validation, repository contract
  httpapi/            create/retrieve and health endpoints
  storage/postgres/   PostgreSQL repository and integration tests
  evaluation/         task runner, simulator, harness, baselines, report types
  evaluation/llmagent/ restricted tool agent and OpenAI-compatible HTTP client
  evaluation/verifier/ independent task and safety checks
migrations/           versioned up/down SQL
scripts/              explicit PowerShell migration runner
docs/                 evaluation evidence, architecture, interview guide
```

## Requirements

- Go 1.25 or newer (the module declares Go 1.25.0).
- Docker Desktop with Docker Compose v2, or a reachable PostgreSQL instance.
- Windows PowerShell for the documented local setup and migration script.

## Run locally with PostgreSQL

From the repository root:

```powershell
if (-not (Test-Path .env)) {
  Copy-Item .env.example .env
}
docker compose up -d db
docker compose ps
```

The `.env.example` and Compose fallback credentials are **local development credentials only**. Do not use them outside a disposable local database. Compose reads `.env`; the API reads its connection settings from process environment variables and does not automatically load `.env`.

Apply versioned migrations explicitly:

```powershell
.\scripts\migrate-up.ps1
```

The script creates/uses `sentinel_schema_migrations`, applies unapplied `*.up.sql` files in filename order, and records each version transactionally. If `incidents` exists without a ledger row for migration 001, it stops for deliberate inspection/baselining rather than assuming the schema is compatible. The API never applies migrations at startup. The down migration drops incident data and is not part of the routine setup.

Configure the API in the same PowerShell session and run it:

```powershell
$env:SENTINEL_DATABASE_URL = 'postgres://sentinel:sentinel-local-only@127.0.0.1:5435/sentinel?sslmode=disable'
$env:SENTINEL_HTTP_ADDR = '127.0.0.1:8080' # optional; this is also the default
go run ./cmd/sentinel-api
```

Use a connection URL appropriate to your local database. The sample username/password above are development-only. Keep real credentials in a secret manager or process environment; do not commit `.env` or paste credentials into reports. Startup validates configuration and pings PostgreSQL with a timeout. The service uses a bounded SQL connection pool and gracefully shuts down on OS interrupt/termination signals.

## HTTP API

All incident responses are JSON. `occurred_at` must be an RFC3339 timestamp. Create requests reject unknown JSON fields and bodies larger than 1 MiB.

### `POST /v1/incidents`

```powershell
$body = @{
  tenant_id = 'tenant-demo'
  source = 'synthetic'
  source_event_id = 'evt-payment-500-001'
  service = 'payment-api'
  title = 'Payment API returns HTTP 500'
  description = 'Elevated 500 responses began after a synthetic deployment.'
  severity = 'high'
  occurred_at = '2026-10-12T10:15:00Z'
} | ConvertTo-Json

$created = Invoke-RestMethod -Method Post `
  -Uri http://127.0.0.1:8080/v1/incidents `
  -ContentType 'application/json' `
  -Body $body
$created
```

A new record returns `201 Created`, a `Location` header, and a response shaped like:

```json
{
  "incident": {
    "id": "<server-generated-uuid>",
    "tenant_id": "tenant-demo",
    "source": "synthetic",
    "source_event_id": "evt-payment-500-001",
    "service": "payment-api",
    "title": "Payment API returns HTTP 500",
    "description": "Elevated 500 responses began after a synthetic deployment.",
    "severity": "high",
    "occurred_at": "2026-10-12T10:15:00Z",
    "created_at": "<database timestamp>"
  },
  "idempotent_replay": false
}
```

The unique idempotency key is `(tenant_id, source, source_event_id)`. Repeating the key with the same normalized fields returns `200 OK`, the existing incident ID, and `idempotent_replay: true`. Reusing that key with changed incident fields returns `409 Conflict`.

### `GET /v1/incidents/{id}`

```powershell
Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/incidents/$($created.incident.id)"
```

A found record returns `200 OK` with the incident object. Invalid UUIDs return `400`, absent incidents return `404`, unavailable storage returns `503`, and malformed or invalid create requests return `400`. Errors use `{"error":{"code":"...","message":"..."}}`. Unexpected repository failures return a generic `500` response without exposing database details.

### Health

- `GET /health/live` returns `200` if the HTTP process is serving.
- `GET /health/ready` returns `200` while PostgreSQL responds, or `503` otherwise. Database error details are not included.

## Run the evaluation harness

The `payment-route-failover-v1` task (current version `1.1.0`) is a deterministic, process-local simulation. It starts with the primary route active and unhealthy and the backup healthy. It requires the agent to read the incident alert and service metrics before the first route switch. The allowed tools are alert, metrics, recent deployment reads, and switching the payment API between the supported routes. The simulator resets for every run.

Run the three deterministic controls three times each and save a JSON report:

```powershell
go run ./cmd/sentinel-eval -agent baselines -runs-per-agent 3 -out .\sentinel-baselines.json
```

CLI flags verified in `cmd/sentinel-eval`: `-agent baselines|llm`, `-runs-per-agent`, `-out` (use `-` for stdout), and `-experiment-id` (generated if omitted). Baselines are scripted fixtures, not model evaluations. A quick one-run-per-baseline smoke command is:

```powershell
go run ./cmd/sentinel-eval -agent baselines -runs-per-agent 1 -out -
```

Each run gets a fresh simulator. Task and safety scores are separate; overall success requires both. Completed runs with task failure or a safety violation are scored. Provider, environment, and verifier interruptions are incomplete with null task/safety scores; they count as excluded errors rather than ordinary task failures. Pass-rate denominators are completed evaluations. Reports include per-run evidence, experiment/run identifiers, task version, aggregate scores, and diagnostic counts. See [the inspected evaluation results](docs/evaluation-results.md) for actual historical reports and their limitations.

### Optional live LLM run

The LLM adapter uses the standard-library HTTP client and an OpenAI Chat Completions-compatible endpoint. Live calls are optional and incur provider quota/cost. Supply the API key only in the current PowerShell session; do not save it in source, `.env`, or a report. The following Groq example uses variables supported by the CLI:

```powershell
$env:SENTINEL_LLM_API_KEY = '<provider API key>'
$env:SENTINEL_LLM_MODEL = 'openai/gpt-oss-20b'
$env:SENTINEL_LLM_PROVIDER = 'groq'
$env:SENTINEL_LLM_BASE_URL = 'https://api.groq.com/openai/v1/chat/completions'
go run ./cmd/sentinel-eval -agent llm -runs-per-agent 1 -out .\sentinel-llm-report.json
Remove-Item Env:SENTINEL_LLM_API_KEY, Env:SENTINEL_LLM_MODEL, Env:SENTINEL_LLM_PROVIDER, Env:SENTINEL_LLM_BASE_URL -ErrorAction SilentlyContinue
```

The report stores provider/model and version metadata, not the API key or authorization header. Provider traces store categorized errors rather than raw response bodies. Review any report before sharing because agent traces and final responses are still evaluation data. Do not infer broad model capability from a small sample.

## Tests and verification

```powershell
$goFiles = Get-ChildItem -Recurse -Filter *.go | ForEach-Object { $_.FullName }
gofmt -w $goFiles
go test -count=1 ./...
go vet ./...
```

PostgreSQL repository integration tests are opt-in. After starting PostgreSQL and applying migrations, set the test URL in the current shell and run:

```powershell
$env:SENTINEL_TEST_DATABASE_URL = $env:SENTINEL_DATABASE_URL
go test -v ./internal/storage/postgres
```

Without `SENTINEL_TEST_DATABASE_URL`, those tests skip; the unit and simulator tests still run. The integration suite covers creation, replay/conflict behavior, retrieval, and concurrent identical submissions.

## Known limitations and next steps

- The incident API has no authentication or authorization; `tenant_id` is data supplied by the caller, not an authenticated tenant boundary.
- No rate limiting, durable event ingestion, pagination, update/delete API, or production observability is implemented.
- The evaluation simulator is in-process. Go interfaces constrain ordinary tool use but do not isolate a hostile agent process, memory, or network access.
- The task is synthetic and narrow. Verifier success demonstrates behavior against this rubric, not general reasoning or production remediation ability.
- Historical live runs include provider rate-limit interruptions. See the report for task versions and completed-run denominators; the small evidence set is not statistically significant.

Potential future work includes authenticated tenant scoping, API rate limits, structured operational telemetry, and an isolated evaluation worker with a verifier-owned evidence channel. These are proposals, not current capabilities.

## Portfolio and interview notes

- [Architecture and data flow](docs/architecture.md)
- [Evaluation results and reproducibility](docs/evaluation-results.md)
- [Interview guide and question outlines](docs/interview-guide.md)

Historical JSON files in the repository root are retained evaluation artifacts. `sentinel-increment13-smoke.json` records a three-baseline v1.1.0 smoke run; the Groq reports preserve earlier live-run outcomes. They are evidence for this project history, not claims of production reliability.



