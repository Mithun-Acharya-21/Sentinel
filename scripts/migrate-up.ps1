param(
    [string] $Database = "sentinel",
    [string] $User = "sentinel"
)

$ErrorActionPreference = "Stop"
$migrationDirectory = Join-Path (Get-Location) "migrations"
$migrationFiles = Get-ChildItem -LiteralPath $migrationDirectory -Filter "*.up.sql" -File | Sort-Object Name

if ($migrationFiles.Count -eq 0) {
    throw "No versioned *.up.sql files found in $migrationDirectory"
}

$ledgerSQL = @"
CREATE TABLE IF NOT EXISTS sentinel_schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
"@
$ledgerSQL | docker compose exec -T db psql -v ON_ERROR_STOP=1 -U $User -d $Database
if ($LASTEXITCODE -ne 0) {
    throw "Could not initialize the migration ledger. Confirm the db service is running."
}

foreach ($migration in $migrationFiles) {
    if ($migration.Name -notmatch '^(\d+_[a-z0-9_]+)\.up\.sql$') {
        throw "Migration filename must use <version>_<name>.up.sql: $($migration.Name)"
    }
    $version = $Matches[1]
    $versionSQL = "SELECT EXISTS (SELECT 1 FROM sentinel_schema_migrations WHERE version = '$version');"
    $isApplied = docker compose exec -T db psql -Atq -U $User -d $Database -c $versionSQL
    if ($LASTEXITCODE -ne 0) {
        throw "Could not inspect migration state for $version."
    }
    if ($isApplied.Trim() -eq "t") {
        Write-Host "Already applied: $version"
        continue
    }

    $incidentsExists = docker compose exec -T db psql -Atq -U $User -d $Database -c "SELECT to_regclass('public.incidents') IS NOT NULL;"
    if ($LASTEXITCODE -ne 0) {
        throw "Could not inspect database schema before applying $version."
    }
    if ($version -eq "001_create_incidents" -and $incidentsExists.Trim() -eq "t") {
        throw "The incidents table exists without a migration-ledger entry. It may have been created before migration tracking was added; inspect it and record a baseline explicitly instead of reapplying."
    }

    $migrationSQL = Get-Content -LiteralPath $migration.FullName -Raw
    $transactionSQL = @"
BEGIN;
$migrationSQL
INSERT INTO sentinel_schema_migrations (version) VALUES ('$version');
COMMIT;
"@
    $transactionSQL | docker compose exec -T db psql -v ON_ERROR_STOP=1 -U $User -d $Database
    if ($LASTEXITCODE -ne 0) {
        throw "Migration $version failed and was rolled back."
    }
    Write-Host "Applied: $version"
}
