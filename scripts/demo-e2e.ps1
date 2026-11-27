$ErrorActionPreference = "Stop"

Write-Host "=== 1. Starting PostgreSQL via Docker Compose ==="
docker compose up -d db
if ($LASTEXITCODE -ne 0) { throw "Failed to start PostgreSQL container." }

Write-Host "Waiting for PostgreSQL readiness..."
for ($i = 0; $i -lt 30; $i++) {
    docker compose exec -T db pg_isready -U sentinel -d sentinel | Out-Null
    if ($LASTEXITCODE -eq 0) { break }
    Start-Sleep -Seconds 1
}

Write-Host "=== 2. Applying Versioned SQL Migrations ==="
.\scripts\migrate-up.ps1

$env:SENTINEL_DATABASE_URL = "postgres://sentinel:sentinel-local-only@127.0.0.1:5435/sentinel?sslmode=disable"
$env:SENTINEL_HTTP_ADDR = "127.0.0.1:8080"
$env:SENTINEL_LLM_API_KEY = "demo-local-key"
$env:SENTINEL_LLM_ENDPOINT = "http://127.0.0.1:18081/v1/chat/completions"
$env:SENTINEL_LLM_MODEL = "mock-triage-model"

Write-Host "=== 3. Building & Starting Deterministic Mock LLM + Sentinel API ==="
go build -o sentinel-mock-llm.exe ./cmd/sentinel-mock-llm
go build -o sentinel-api.exe ./cmd/sentinel-api
go build -o sentinel-admin.exe ./cmd/sentinel-admin

$mockProc = Start-Process -FilePath ".\sentinel-mock-llm.exe" -PassThru -NoNewWindow
$apiProc = Start-Process -FilePath ".\sentinel-api.exe" -PassThru -NoNewWindow

try {
    $readyOk = $false
    for ($i = 0; $i -lt 20; $i++) {
        try {
            $ready = Invoke-RestMethod -Uri "http://127.0.0.1:8080/health/ready" -Method Get
            if ($ready.status -eq "ready") { $readyOk = $true; break }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    if (-not $readyOk) { throw "Timed out waiting for sentinel-api readiness." }

    Write-Host "=== 4. Provisioning Scoped API Key (sentinel-admin) ==="
    $adminOut = .\sentinel-admin.exe create-key -tenant tenant-demo -principal human-demo -scopes "incidents:read,incidents:write,incidents:approve,incidents:execute"
    $adminOut | Write-Host
    $apiKey = ($adminOut | Select-String -Pattern "^snt_").Line.Trim()

    $headers = @{
        "Authorization" = "Bearer $apiKey"
        "Content-Type"  = "application/json"
    }

    Write-Host "=== 5. Submitting Incident (POST /v1/incidents) ==="
    $eventId = "evt-demo-" + [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
    $incidentBody = @{
        source          = "demo-cli"
        source_event_id = $eventId
        service         = "payment-api"
        title           = "Primary payment route returning 504 timeouts"
        description     = "504 gateway timeouts on /charge for 5 minutes."
        severity        = "high"
        occurred_at     = [DateTime]::UtcNow.ToString("o")
    } | ConvertTo-Json

    $created = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/incidents" -Method Post -Headers $headers -Body $incidentBody
    $incidentId = $created.incident.id
    Write-Host "Created Incident ID: $incidentId"

    Write-Host "=== 6. Polling Triage Result (GET /v1/incidents/$incidentId/triage) ==="
    $triageView = $null
    for ($i = 0; $i -lt 20; $i++) {
        try {
            $triageView = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/incidents/$incidentId/triage" -Method Get -Headers $headers
            break
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    if (-not $triageView) { throw "Timed out waiting for triage result." }
    $triageView | ConvertTo-Json -Depth 6 | Write-Host

    Write-Host "=== 7. Approving Triage Recommendation (POST /v1/incidents/$incidentId/triage/approve) ==="
    $approveBody = @{ reason = "Verified backup route healthy during demo" } | ConvertTo-Json
    $approval = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/incidents/$incidentId/triage/approve" -Method Post -Headers $headers -Body $approveBody
    $approval | ConvertTo-Json -Depth 4 | Write-Host

    Write-Host "=== 8. Executing Controlled Simulated Remediation (POST /v1/incidents/$incidentId/remediate) ==="
    $remBody = @{
        action_type     = "switch_payment_route_to_backup"
        action_version  = "1.0.0"
        target_resource = "payment-api"
        parameters      = @{ target_route = "backup" }
        approval_id     = $approval.id
        idempotency_key = "demo-rem-$incidentId"
        requested_at    = [DateTime]::UtcNow.ToString("o")
    } | ConvertTo-Json
    $execResult = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/incidents/$incidentId/remediate" -Method Post -Headers $headers -Body $remBody
    $execResult | ConvertTo-Json -Depth 6 | Write-Host

    Write-Host "=== 9 & 10. Inspecting Execution Status & Independent Verification (GET /v1/incidents/$incidentId/remediation) ==="
    $status = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/incidents/$incidentId/remediation" -Method Get -Headers $headers
    $status | ConvertTo-Json -Depth 6 | Write-Host

    Write-Host "=== 11. Inspecting Prometheus Metrics (GET /metrics) ==="
    $metricsText = Invoke-RestMethod -Uri "http://127.0.0.1:8080/metrics" -Method Get
    $metricsText -split "`n" | Select-String -Pattern "^(sentinel_http_requests_total|sentinel_triage_|remediation_|sentinel_worker_jobs_)" | ForEach-Object { $_.Line }
} finally {
    if ($apiProc -and !$apiProc.HasExited) { Stop-Process -Id $apiProc.Id -Force }
    if ($mockProc -and !$mockProc.HasExited) { Stop-Process -Id $mockProc.Id -Force }
    Remove-Item -Force sentinel-mock-llm.exe, sentinel-api.exe, sentinel-admin.exe -ErrorAction SilentlyContinue
}
