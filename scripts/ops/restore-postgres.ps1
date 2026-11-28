#Requires -Version 5.1
<#
.SYNOPSIS
Restores an AegisFlow PostgreSQL backup created by backup-postgres.ps1.

.DESCRIPTION
Restores one custom-format pg_dump archive into a target database. Safety rules:

  * the target database must not exist unless -Force is passed; there is no
    implicit overwrite of an online database;
  * when -Force is passed the target database is dropped (WITH (FORCE), which
    terminates leftover connections) and recreated before the restore;
  * a "<dump>.sha256" sidecar next to the dump is verified when present, and a
    malformed or mismatching sidecar aborts before the target is touched;
  * pg_restore runs with --exit-on-error --single-transaction, so a failed
    archive leaves no half-restored schema;
  * after the restore the script reads the restored schema_migrations version,
    checks that the ledger is present and contiguous and that the 24 tables this
    script expects are present, then prints a "schema version X / table count /
    row summary" block. The schema version is whatever the dump contains:
    restoring an older dump is a legitimate rollback path, so an archive below
    v19 is reported with a warning and is NOT treated as a failure (the v19
    tenant-column checks are not applied to an older archive).

The password is never a parameter; use PGPASSWORD/PGPASSFILE/pgpass or a
peer/trust connection exactly like pg_restore itself.

.EXAMPLE
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/restore-postgres.ps1 `
  -DumpFile D:\backups\aegisflow-20261128-231500.dump -TargetDatabase aegisflow_restore_drill

.EXAMPLE
# Replace a disposable drill database on purpose (never a live production DB):
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/restore-postgres.ps1 `
  -DumpFile D:\backups\aegisflow-20261128-231500.dump -TargetDatabase aegisflow_restore_drill -Force
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $DumpFile,

    [Parameter(Mandatory = $true)] [string] $TargetDatabase,

    [Alias('Host')] [string] $DatabaseHost = '127.0.0.1',

    [ValidateRange(1, 65535)] [int] $Port = 5433,

    [string] $User = 'postgres',

    [switch] $Force,

    [string] $PgRestorePath,

    [string] $PsqlPath,

    [string] $SslMode
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'ops-common.ps1')

function Assert-ChecksumSidecar {
    param([string] $Archive, [string] $SidecarPath)

    if (-not (Test-Path -LiteralPath $SidecarPath -PathType Leaf)) {
        Write-OpsWarn "no checksum sidecar next to the dump ($SidecarPath); continuing without an integrity comparison"
        return
    }
    $text = [System.IO.File]::ReadAllText($SidecarPath, $script:OpsUtf8NoBom)
    $token = ($text.Trim() -split '\s+')[0]
    if ($token -notmatch '^[0-9a-fA-F]{64}$') {
        throw "checksum sidecar '$SidecarPath' is malformed; refusing to restore an archive whose integrity cannot be checked"
    }
    Write-OpsLog 'checksum: computing SHA-256 of the dump'
    $actual = Get-OpsSha256 -Path $Archive
    if ($actual -ne $token.ToLowerInvariant()) {
        throw "SHA-256 mismatch for '$Archive': sidecar=$($token.ToLowerInvariant()) actual=$actual; the dump is corrupted or was modified"
    }
    Write-OpsLog "checksum: OK ($actual)"
}

function Get-RequiredTableNames {
    return @(
        'schema_migrations', 'tenants',
        'auth_users', 'auth_sessions', 'auth_refresh_tokens', 'auth_audit', 'oidc_login_states',
        'projects', 'project_members', 'builds', 'build_log_events',
        'deployment_targets', 'deployments', 'deployment_events',
        'approvals', 'approval_notifications',
        'reviews', 'review_external_results', 'review_notifications',
        'scans', 'security_events', 'knowledge_docs', 'secrets', 'project_policies'
    )
}

function Invoke-RestoreMain {
    Assert-OpsDatabaseName -Name $TargetDatabase -ParameterName 'TargetDatabase'
    $maintenanceDatabases = @('postgres', 'template0', 'template1')
    if ($maintenanceDatabases -contains $TargetDatabase) {
        throw "refusing to restore into PostgreSQL maintenance database '$TargetDatabase'"
    }
    Assert-OpsConnectionSettings -DatabaseHost $DatabaseHost -Port $Port -UserName $User
    Set-OpsPgEnvironment -SslMode $SslMode -AppName 'aegisflow-restore'

    Write-OpsStage 'restore target'
    $resolvedDump = Resolve-OpsPath -Path $DumpFile
    if (-not (Test-Path -LiteralPath $resolvedDump -PathType Leaf)) { throw "dump file not found: $resolvedDump" }
    $dumpSize = (Get-Item -LiteralPath $resolvedDump).Length
    if ($dumpSize -le 0) { throw "dump file is empty: $resolvedDump" }
    Write-OpsLog "dump:     $resolvedDump ($(Format-OpsByteSize -Bytes $dumpSize))"
    Write-OpsLog "target:   database=$TargetDatabase host=${DatabaseHost}:$Port user=$User force=$($Force.IsPresent)"

    $psql = Resolve-OpsTool -ToolName 'psql' -ExplicitPath $PsqlPath -EnvironmentVariable 'AEGIS_PSQL_PATH'
    $pgRestore = Resolve-OpsTool -ToolName 'pg_restore' -ExplicitPath $PgRestorePath -EnvironmentVariable 'AEGIS_PG_RESTORE_PATH'
    Write-OpsLog "psql:      $psql ($(Get-OpsToolVersion -ToolPath $psql))"
    Write-OpsLog "pg_restore: $pgRestore ($(Get-OpsToolVersion -ToolPath $pgRestore))"

    Assert-ChecksumSidecar -Archive $resolvedDump -SidecarPath ($resolvedDump + '.sha256')

    Write-OpsStage 'check target database'
    $existing = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database 'postgres' -DatabaseHost $DatabaseHost -Port $Port -UserName $User `
            -Sql "SELECT count(*) FROM pg_database WHERE datname = '$TargetDatabase'")
    $existingCount = 0
    if ($existing.Count -gt 0) { $existingCount = [int]$existing[0] }
    if ($existingCount -gt 0 -and -not $Force) {
        throw "target database '$TargetDatabase' already exists; refusing to overwrite it without -Force (choose a new database name or pass -Force deliberately)"
    }

    $prepareSql = ''
    if ($existingCount -gt 0) {
        Write-OpsLog "database '$TargetDatabase' exists and -Force was passed: it will be dropped and recreated"
        $prepareSql += 'DROP DATABASE IF EXISTS "' + $TargetDatabase + '" WITH (FORCE);' + "`n"
    }
    else {
        Write-OpsLog "database '$TargetDatabase' does not exist; creating it"
    }
    $prepareSql += 'CREATE DATABASE "' + $TargetDatabase + '";' + "`n"

    Write-OpsStage 'prepare target database'
    $prepareFile = New-OpsTempFile -Prefix 'aegisflow-restore-prepare'
    try {
        Write-OpsTextFile -Path $prepareFile -Text $prepareSql
        $prepareArguments = @(
            '-X', '-w', '-q', '-v', 'ON_ERROR_STOP=1',
            '-h', $DatabaseHost, '-p', [string]$Port, '-d', 'postgres',
            '-U', $User, '-f', $prepareFile
        )
        Invoke-OpsNative -ToolPath $psql -Arguments $prepareArguments
        $prepareExit = $LASTEXITCODE
    }
    finally {
        Remove-OpsFile -Path $prepareFile
    }
    if ($prepareExit -ne 0) {
        throw "psql exited with code $prepareExit while preparing database '$TargetDatabase' (DROP/CREATE DATABASE needs CREATEDB)"
    }

    Write-OpsStage 'pg_restore'
    $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
    $restoreArguments = @(
        '--no-owner',
        '--no-privileges',
        '--exit-on-error',
        '--single-transaction',
        '--no-password',
        '-h', $DatabaseHost,
        '-p', [string]$Port,
        '-U', $User,
        "--dbname=$TargetDatabase",
        $resolvedDump
    )
    Invoke-OpsNative -ToolPath $pgRestore -Arguments $restoreArguments
    $restoreExit = $LASTEXITCODE
    $stopwatch.Stop()
    if ($restoreExit -ne 0) {
        throw "pg_restore exited with code $restoreExit; database '$TargetDatabase' was not restored (--single-transaction rolled it back). Rerun with -Force after fixing the archive"
    }
    Write-OpsLog ("pg_restore finished in " + $stopwatch.Elapsed.TotalSeconds.ToString('0.0') + 's')

    Write-OpsStage 'verify restored schema'
    try {
        $ledgerLines = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $TargetDatabase -DatabaseHost $DatabaseHost -Port $Port -UserName $User `
                -Sql "SELECT COALESCE(MAX(version),0)::text || '|' || COUNT(*)::text || '|' || COALESCE(MIN(version),0)::text FROM schema_migrations")
    }
    catch {
        throw "restored database '$TargetDatabase' has no readable schema_migrations table; the archive did not contain the AegisFlow schema"
    }
    if ($ledgerLines.Count -ne 1) {
        throw "could not read schema_migrations from '$TargetDatabase' after the restore"
    }
    $ledgerParts = $ledgerLines[0].Split('|')
    $schemaVersion = [int]$ledgerParts[0]
    $ledgerRows = [int]$ledgerParts[1]
    $ledgerMin = [int]$ledgerParts[2]
    $structureFailures = New-Object System.Collections.Generic.List[string]

    Write-OpsLog "schema_migrations: max version=$schemaVersion rows=$ledgerRows"
    if ($schemaVersion -lt 1) {
        $structureFailures.Add('schema_migrations is empty: the archive restored no migration ledger')
    }
    if ($ledgerRows -ne $schemaVersion -or $ledgerMin -ne 1) {
        $structureFailures.Add("schema_migrations is not contiguous (min=$ledgerMin max=$schemaVersion rows=$ledgerRows)")
    }
    if ($schemaVersion -ge 1 -and $schemaVersion -lt 19) {
        Write-OpsWarn ("the restored archive is at schema version $schemaVersion, which is older than the v19 structure this script is written for.")
        Write-OpsWarn 'Restoring an older backup is a legitimate rollback path and is NOT a failure; the checks below only verify'
        Write-OpsWarn 'the required table set and row counts. Point the matching older server binary at this database.'
    }

    $tableNamesSql = "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name"
    $presentTables = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $TargetDatabase -DatabaseHost $DatabaseHost -Port $Port -UserName $User -Sql $tableNamesSql)
    $requiredTables = Get-RequiredTableNames
    $missingTables = @($requiredTables | Where-Object { $presentTables -notcontains $_ })
    if ($missingTables.Count -gt 0) {
        $structureFailures.Add('missing tables: ' + ($missingTables -join ', '))
    }
    else {
        Write-OpsLog ("tables: " + $requiredTables.Count + '/' + $requiredTables.Count + " required tables are present (schema version is determined by the archive: v$schemaVersion)")
    }

    $countSelects = @(
        "SELECT 'tenants|' || count(*)::text FROM tenants",
        "SELECT 'auth_users|' || count(*)::text FROM auth_users",
        "SELECT 'auth_sessions|' || count(*)::text FROM auth_sessions",
        "SELECT 'auth_audit|' || count(*)::text FROM auth_audit",
        "SELECT 'projects|' || count(*)::text FROM projects",
        "SELECT 'project_members|' || count(*)::text FROM project_members",
        "SELECT 'builds|' || count(*)::text FROM builds",
        "SELECT 'build_log_events|' || count(*)::text FROM build_log_events",
        "SELECT 'deployment_targets|' || count(*)::text FROM deployment_targets",
        "SELECT 'deployment_targets_with_secret|' || count(*)::text FROM deployment_targets WHERE secret_cipher <> ''",
        "SELECT 'deployments|' || count(*)::text FROM deployments",
        "SELECT 'deployments_with_review|' || count(*)::text FROM deployments WHERE review_id IS NOT NULL",
        "SELECT 'deployment_events|' || count(*)::text FROM deployment_events",
        "SELECT 'approvals|' || count(*)::text FROM approvals",
        "SELECT 'approvals_pending|' || count(*)::text FROM approvals WHERE status = 'pending'",
        "SELECT 'approval_notifications_undelivered|' || count(*)::text FROM approval_notifications WHERE status <> 'delivered'",
        "SELECT 'reviews|' || count(*)::text FROM reviews",
        "SELECT 'review_external_results|' || count(*)::text FROM review_external_results",
        "SELECT 'review_notifications_undelivered|' || count(*)::text FROM review_notifications WHERE status <> 'delivered'",
        "SELECT 'scans|' || count(*)::text FROM scans",
        "SELECT 'security_events|' || count(*)::text FROM security_events",
        "SELECT 'knowledge_docs|' || count(*)::text FROM knowledge_docs",
        "SELECT 'secrets|' || count(*)::text FROM secrets",
        "SELECT 'project_policies|' || count(*)::text FROM project_policies"
    )
    $counts = @{}
    if ($missingTables.Count -eq 0) {
        $countLines = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $TargetDatabase -DatabaseHost $DatabaseHost -Port $Port -UserName $User -Sql ($countSelects -join "`nUNION ALL`n"))
        foreach ($line in $countLines) {
            if ($line -match '^([a-z_]+)\|([0-9]+)$') { $counts[$matches[1]] = [long]$matches[2] }
        }
        if ($counts.Count -ne $countSelects.Count) {
            $structureFailures.Add("could not read the post-restore row counts (got $($counts.Count) of $($countSelects.Count) values)")
        }
        elseif ($counts['tenants'] -lt 1) {
            $structureFailures.Add('tenants table is empty: the restore produced a deployment without any tenant')
        }
    }

    if ($counts.Count -gt 0) {
        Write-OpsStage 'restored row counts'
        foreach ($key in @('tenants', 'auth_users', 'auth_sessions', 'auth_audit', 'projects', 'project_members', 'builds',
                'build_log_events', 'deployment_targets', 'deployment_targets_with_secret', 'deployments', 'deployments_with_review',
                'deployment_events', 'approvals', 'approvals_pending', 'approval_notifications_undelivered', 'reviews',
                'review_external_results', 'review_notifications_undelivered', 'scans', 'security_events', 'knowledge_docs',
                'secrets', 'project_policies')) {
            if ($counts.ContainsKey($key)) { Write-OpsLog ($key + ': ' + $counts[$key]) }
        }
    }

    if ($structureFailures.Count -gt 0) {
        foreach ($failure in $structureFailures) { Write-OpsError ('verification: ' + $failure) }
        throw "restored database '$TargetDatabase' failed structural verification; do not point a production instance at it"
    }

    Write-OpsStage 'restore result'
    Write-OpsLog "database:      $TargetDatabase"
    Write-OpsLog "schema:        version $schemaVersion ($ledgerRows migrations)"
    Write-OpsLog ("tables:        " + ($requiredTables.Count - $missingTables.Count) + '/' + $requiredTables.Count + ' required tables present')
    if ($counts.Count -gt 0) {
        $summaryKeys = @('tenants', 'auth_users', 'projects', 'builds', 'deployment_targets', 'deployments', 'approvals', 'secrets')
        $summaryParts = @()
        foreach ($key in $summaryKeys) {
            if ($counts.ContainsKey($key)) { $summaryParts += ($key + '=' + $counts[$key]) }
        }
        if ($summaryParts.Count -gt 0) { Write-OpsLog ('row summary:   ' + ($summaryParts -join ' ')) }
    }
    Write-OpsLog 'verification:  OK (ledger, tables and row counts)'
    Write-OpsLog 'restore: OK'
    Write-OpsLog ''
    Write-OpsLog 'Next steps for a drill:'
    Write-OpsLog '  1. verify sealed credentials can be decrypted with the AEGIS_SECRET_KEY that existed when this backup'
    Write-OpsLog '     was taken. cmd/rekey is a ROTATION tool, not a restore check: it requires AEGIS_SECRET_KEY_PREVIOUS'
    Write-OpsLog '     and that key must differ from AEGIS_SECRET_KEY, so a single-key run fails with exit 1. To use it as a'
    Write-OpsLog '     readability probe, run a rotation simulation (new random key as AEGIS_SECRET_KEY, the backup-time key as'
    Write-OpsLog '     AEGIS_SECRET_KEY_PREVIOUS) and require failed=0 on the default dry run; never pass -apply against a'
    Write-OpsLog '     restore you want to keep comparable. Prefer the application path (temporary instance, admin login,'
    Write-OpsLog '     deployment-target probe) to prove the backup-time key still opens credentials. See scripts/ops/README.md.'
    Write-OpsLog '  2. start a temporary API instance against this database with a separate Redis namespace and confirm'
    Write-OpsLog '     /readyz, an admin login and a project listing before switching production to it.'
    if ($schemaVersion -ge 1 -and $schemaVersion -lt 19) {
        Write-OpsLog ("  3. this restore is at schema v" + $schemaVersion + ": run the matching older server binary and record its")
        Write-OpsLog '     structural baseline with the psql checks in scripts/ops/README.md (verify-schema only supports v19+).'
    }
}

try {
    Invoke-RestoreMain
}
catch {
    Write-OpsError ('restore failed: ' + $_.Exception.Message)
    exit 1
}
exit 0
