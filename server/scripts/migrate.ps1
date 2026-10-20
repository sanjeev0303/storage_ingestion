# Applies pending migrations in order to the local Docker Compose PostgreSQL.
# Applied files are recorded in schema_migrations and skipped next time:
# re-running an old migration can fail once a later one has changed the
# same constraint. Uses psql inside the container: nothing to install.
#
#   .\scripts\migrate.ps1                         # object_storage
#   .\scripts\migrate.ps1 -Database object_storage_test -Create
#
# A database migrated before schema_migrations existed is marked up to a
# known version once, without running anything:
#
#   .\scripts\migrate.ps1 -Baseline 000006
param(
    [string]$Database = "object_storage",
    [string]$Container = "object-storage-postgres",
    [string]$User = "storage_app",
    [switch]$Create,
    [string]$Baseline = ""
)

$ErrorActionPreference = "Stop"

function Invoke-Psql([string]$Sql) {
    $out = docker exec -e PGOPTIONS="-c client_min_messages=warning" $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 -Atqc $Sql
    if ($LASTEXITCODE -ne 0) {
        throw "psql failed on $Database"
    }
    return $out
}

if ($Create) {
    $exists = docker exec $Container psql -U $User -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = '$Database'"
    if ($exists -ne "1") {
        docker exec $Container psql -U $User -d postgres -c "CREATE DATABASE $Database" | Out-Null
        Write-Host "created database $Database"
    }
}

Invoke-Psql "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())" | Out-Null

$migrations = Get-ChildItem -Path (Join-Path $PSScriptRoot "..\migrations") -Filter "*.sql" | Sort-Object Name

if ($Baseline -ne "") {
    foreach ($migration in $migrations) {
        if ($migration.Name.Substring(0, 6) -le $Baseline) {
            Invoke-Psql "INSERT INTO schema_migrations (version) VALUES ('$($migration.Name)') ON CONFLICT DO NOTHING" | Out-Null
            Write-Host "marked $($migration.Name) as applied on $Database"
        }
    }
}

$applied = @(Invoke-Psql "SELECT version FROM schema_migrations")

foreach ($migration in $migrations) {
    if ($applied -contains $migration.Name) {
        continue
    }
    # Each file runs in its own transaction; the version is recorded only
    # after it commits.
    Get-Content -Raw $migration.FullName | docker exec -i -e PGOPTIONS="-c client_min_messages=warning" $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 -q
    if ($LASTEXITCODE -ne 0) {
        throw "migration $($migration.Name) failed on $Database"
    }
    Invoke-Psql "INSERT INTO schema_migrations (version) VALUES ('$($migration.Name)')" | Out-Null
    Write-Host "applied $($migration.Name) to $Database"
}
