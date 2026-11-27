# Sentinel HTTP API Reference

**Base URL:** `http://127.0.0.1:8080`  
**Authentication:** `Authorization: Bearer snt_<16-hex-key-id>_<secret>` (required on all `/v1/*` routes)  
**Content-Type:** `application/json`

---

## 1. Operational & Health Endpoints (Unauthenticated)

| Method | Path | Response | Description |
|---|---|---|---|
| `GET` | `/health/live` | `200 {"status":"ok"}` | Process liveness probe; never fails due to PostgreSQL outages. |
| `GET` | `/health/ready` | `200 {"status":"ready"}` or `503 {"status":"not_ready"\|"draining"}` | Readiness probe; checks draining state and pings PostgreSQL (1s timeout). |
| `GET` | `/metrics` | `200 text/plain; version=0.0.4` | Prometheus metrics exposition (restrict at ingress in production). |

---

## 2. Error Envelope

All `/v1/*` error responses return a uniform JSON envelope:
```json
{
  "error": {
    "code": "string",
    "message": "string"
  }
}
```

Common error codes:
- `400 invalid_request` / `invalid_action`
- `401 unauthorized` / `invalid_credentials` (`WWW-Authenticate: Bearer realm="sentinel"`)
- `403 forbidden` / `approval_required` / `action_mismatch`
- `404 not_found`
- `409 idempotency_key_reused` / `approval_already_decided` / `approval_expired` / `execution_conflict`
- `429 rate_limit_exceeded` (includes `Retry-After` header in seconds)
- `503 storage_unavailable` / `auth_unavailable`

---

## 3. Incident Endpoints

### `POST /v1/incidents`
- **Required Scope:** `incidents:write`
- **Notes:** `tenant_id` is derived authoritatively from the authenticated `auth.Principal`. Supplying a mismatched `tenant_id` in the JSON body or `?tenant_id=` query parameter returns `403 forbidden`. Atomically inserts `incidents` and a `pending` row in `incident_jobs`.
- **Response:** `201 Created` (with `Location` header) or `200 OK` (`idempotent_replay: true`).

```powershell
curl.exe -s -X POST http://127.0.0.1:8080/v1/incidents `
  -H "Authorization: Bearer $env:SENTINEL_API_KEY" `
  -H "Content-Type: application/json" `
  -d '{"source":"pagerduty","source_event_id":"evt-1001","service":"payment-api","title":"504 timeouts","description":"Primary route returning 504","severity":"high","occurred_at":"2026-11-27T16:00:00Z"}'
```

### `GET /v1/incidents/{id}`
- **Required Scope:** `incidents:read`
- **Response:** `200 OK` (`incident.Incident`) or `404 not_found` (including cross-tenant IDs).

```powershell
curl.exe -s http://127.0.0.1:8080/v1/incidents/<INCIDENT_UUID> `
  -H "Authorization: Bearer $env:SENTINEL_API_KEY"
```

---

## 4. Triage & Human Approval Endpoints

### `GET /v1/incidents/{id}/triage`
- **Required Scope:** `incidents:read`
- **Response (`200 OK`):** `triage.TriageView` (`triage` + `approval`).

```powershell
curl.exe -s http://127.0.0.1:8080/v1/incidents/<INCIDENT_UUID>/triage `
  -H "Authorization: Bearer $env:SENTINEL_API_KEY"
```

### `POST /v1/incidents/{id}/triage/approve`
- **Required Scope:** `incidents:approve`
- **Request Body:** `{"reason": "verified backup route healthy"}`
- **Response (`200 OK`):** `triage.ApprovalRecord` (`status: "approved"`, `decided_by`, `decided_at`). Returns `409` if already decided or expired.

```powershell
curl.exe -s -X POST http://127.0.0.1:8080/v1/incidents/<INCIDENT_UUID>/triage/approve `
  -H "Authorization: Bearer $env:SENTINEL_API_KEY" `
  -H "Content-Type: application/json" `
  -d '{"reason":"Verified backup route healthy"}'
```

### `POST /v1/incidents/{id}/triage/reject`
- **Required Scope:** `incidents:approve`
- **Request Body:** `{"reason": "false alarm"}`
- **Response (`200 OK`):** `triage.ApprovalRecord` (`status: "rejected"`).

---

## 5. Controlled Remediation Endpoints

### `POST /v1/incidents/{id}/remediate`
- **Required Scope:** `incidents:execute`
- **Allowlisted `action_type` values:** `switch_payment_route_to_backup`, `disable_unhealthy_provider`, `restart_simulated_worker` (`action_version: "1.0.0"`).
- **Response (`200 OK`):** `remediation.ExecutionRecord` (`status: "succeeded" | "failed" | "verification_failed"`, `verification_passed`, `verification_summary`, `approver_id`).

```powershell
curl.exe -s -X POST http://127.0.0.1:8080/v1/incidents/<INCIDENT_UUID>/remediate `
  -H "Authorization: Bearer $env:SENTINEL_API_KEY" `
  -H "Content-Type: application/json" `
  -d '{"action_type":"switch_payment_route_to_backup","action_version":"1.0.0","target_resource":"payment-api","parameters":{"target_route":"backup"},"approval_id":"<APPROVAL_UUID>","idempotency_key":"rem-1001","requested_at":"2026-11-27T16:05:00Z"}'
```

### `GET /v1/incidents/{id}/remediation`
- **Required Scope:** `incidents:read`
- **Response (`200 OK`):** `remediation.ExecutionRecord`.
