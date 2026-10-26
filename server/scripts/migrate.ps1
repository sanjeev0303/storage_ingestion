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
#
# Every run is one psql session holding an advisory lock, so two runs at
# once take turns, and the second finds the first one's work recorded. Each
# file's version is inserted inside the file's own transaction: a file is
# applied and recorded together, or neither.
param(
    [string]$Database = "object_storage",
    [string]$Container = "object-storage-postgres",
    [string]$User = "storage_app",
    [switch]$Create,
    [string]$Baseline = ""
)

$ErrorActionPreference = "Stop"
# Windows PowerShell pipes ASCII to native commands by default.
$OutputEncoding = [System.Text.UTF8Encoding]::new($false)

# Any fixed number works; every run of this script uses the same one.
$LockKey = 7240512

function Invoke-Psql([string]$Sql) {
    $out = docker exec -e PGOPTIONS="-c client_min_messages=warning" $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 -Atqc $Sql
    if ($LASTEXITCODE -ne 0) {
        throw "psql failed on $Database"
    }
    return $out
}

if ($Baseline -ne "" -and $Baseline -notmatch '^\d{1,6}$') {
    throw "-Baseline must be a migration number such as 000006"
}

if ($Create) {
    $exists = docker exec $Container psql -U $User -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = '$Database'"
    if ($exists -ne "1") {
        docker exec $Container psql -U $User -d postgres -c "CREATE DATABASE $Database" | Out-Null
        Write-Host "created database $Database"
    }
}

# Under the lock too: two runs creating the table at once collide.
Invoke-Psql "BEGIN; SELECT pg_advisory_xact_lock($LockKey); CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()); COMMIT;" | Out-Null

$migrations = Get-ChildItem -Path (Join-Path $PSScriptRoot "..\migrations") -Filter "*.sql" | Sort-Object Name

$script = [System.Text.StringBuilder]::new()
[void]$script.AppendLine("SELECT pg_advisory_lock($LockKey) AS locked \gset")

if ($Baseline -ne "") {
    foreach ($migration in $migrations) {
        # Compared as numbers: "6" and "000006" are the same baseline.
        if ([int]$migration.Name.Substring(0, 6) -le [int]$Baseline) {
            [void]$script.AppendLine("INSERT INTO schema_migrations (version) VALUES ('$($migration.Name)') ON CONFLICT DO NOTHING;")
            [void]$script.AppendLine("\echo marked $($migration.Name) as applied on $Database")
        }
    }
}

# Files already recorded are left out; the check under the lock catches
# one another run applied meanwhile.
$applied = @(Invoke-Psql "SELECT version FROM schema_migrations")
$pending = 0
foreach ($migration in $migrations) {
    if ($applied -contains $migration.Name) {
        continue
    }
    $pending++
    $name = $migration.Name
    $record = "INSERT INTO schema_migrations (version) VALUES ('$name');"
    $sql = Get-Content -Raw -Encoding UTF8 $migration.FullName
    # The version goes in just before the file's own COMMIT; a file without
    # one is wrapped in a transaction of its own.
    $commit = $sql.LastIndexOf("COMMIT;", [System.StringComparison]::OrdinalIgnoreCase)
    if ($commit -ge 0) {
        $sql = $sql.Substring(0, $commit) + $record + "`n" + $sql.Substring($commit)
    } else {
        $sql = "BEGIN;`n" + $sql + "`n" + $record + "`nCOMMIT;"
    }
    [void]$script.AppendLine("SELECT NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '$name') AS pending \gset")
    [void]$script.AppendLine("\if :pending")
    [void]$script.AppendLine("\echo applying $name to $Database")
    [void]$script.AppendLine($sql)
    [void]$script.AppendLine("\echo applied $name to $Database")
    [void]$script.AppendLine("\else")
    [void]$script.AppendLine("\echo $name was applied meanwhile")
    [void]$script.AppendLine("\endif")
}
[void]$script.AppendLine("SELECT pg_advisory_unlock($LockKey) AS unlocked \gset")

if ($pending -eq 0 -and $Baseline -eq "") {
    return
}
# One session for the whole run: the lock holds until it ends, and a failed
# file stops the run with its transaction rolled back, nothing recorded.
$script.ToString() | docker exec -i -e PGOPTIONS="-c client_min_messages=warning" $Container psql -U $User -d $Database -v ON_ERROR_STOP=1 -Atq
if ($LASTEXITCODE -ne 0) {
    throw "migrating $Database failed; the file being applied was rolled back and not recorded"
}
