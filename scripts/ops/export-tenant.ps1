#Requires -Version 5.1
<#
.SYNOPSIS
Exports one AegisFlow tenant to a self-describing directory of CSV files.

.DESCRIPTION
Resolves the tenant by slug, then copies every tenant-owned table into one CSV
file per table plus a manifest.json and a README. The export runs inside a
single REPEATABLE READ, READ ONLY transaction, so all files come from one
consistent snapshot even while other tenants keep working; stop writes to the
exported tenant if you need a quiescent export for a migration rehearsal.

Exported tables (foreign-key-safe order):
  tenants, auth_users, auth_audit, projects, builds, build_log_events,
  deployment_targets, reviews, review_external_results, review_notifications,
  deployments, deployment_events, approvals, approval_notifications, scans,
  security_events, knowledge_docs, secrets, project_policies, project_members.

project_members has no tenant_id in schema v19 (the tenant is implied by its
project), but it is included so project membership survives a tenant move.

Deliberately excluded: auth_sessions, auth_refresh_tokens and
oidc_login_states are short-lived session/one-time state; users sign in again
after a tenant move. auth_users.password_hash is exported as the stored bcrypt
hash (never plaintext). secrets.ciphertext and deployment_targets.secret_cipher
are exported as the stored AES-256-GCM ciphertext: importing/decrypting them
requires the same AEGIS_SECRET_KEY that sealed them.

The password is never a parameter; use PGPASSWORD/PGPASSFILE/pgpass or a
peer/trust connection.

.PARAMETER TenantSlug
Tenant slug to export (the value shown by "cmd/tenant list").

.PARAMETER Database
Source database name (required).

.PARAMETER OutputDir
Parent directory of the generated "tenant-<slug>-<timestamp>" directory.
Defaults to the current directory. The parent is created when missing, the
export directory itself is never overwritten.

.EXAMPLE
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/export-tenant.ps1 `
  -TenantSlug acme -Database aegisflow -Host db.internal -Port 5432 -User aegis_backup -OutputDir D:\tenant-moves
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $TenantSlug,

    [Parameter(Mandatory = $true)] [string] $Database,

    [Alias('Host')] [string] $DatabaseHost = '127.0.0.1',

    [ValidateRange(1, 65535)] [int] $Port = 5433,

    [string] $User = 'postgres',

    [string] $OutputDir,

    [string] $PsqlPath,

    [string] $SslMode
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'ops-common.ps1')

function Get-RequiredColumnList {
    param([string] $Psql)

    $definitions = Get-OpsTenantTableDefinitions
    $tableNames = @($definitions | ForEach-Object { $_.Name })
    $inList = ($tableNames | ForEach-Object { "'" + $_ + "'" }) -join ','
    $sql = "SELECT table_name || '|' || string_agg(column_name, ',' ORDER BY ordinal_position) " +
        "FROM information_schema.columns WHERE table_schema = 'public' AND table_name IN ($inList) " +
        "GROUP BY table_name ORDER BY table_name"
    $lines = @(Invoke-OpsPsqlQuery -PsqlPath $Psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User -Sql $sql)
    $columnMap = @{}
    foreach ($line in $lines) {
        if ($line -match '^([a-z][a-z0-9_]*)\|([a-z0-9_,]+)$') {
            $columnMap[$matches[1]] = $matches[2]
        }
    }
    foreach ($name in $tableNames) {
        if (-not $columnMap.ContainsKey($name)) { throw "table '$name' is missing from database '$Database'; apply the server migrations before exporting" }
    }
    return $columnMap
}

function Get-FirstLine {
    param([string] $Path)
    $reader = New-Object System.IO.StreamReader($Path, $script:OpsUtf8NoBom)
    try {
        return $reader.ReadLine()
    }
    finally {
        $reader.Dispose()
    }
}

function Invoke-ExportMain {
    if (-not $OutputDir) { $OutputDir = (Get-Location).ProviderPath }
    Assert-OpsTenantSlug -Slug $TenantSlug
    Assert-OpsDatabaseName -Name $Database -ParameterName 'Database'
    Assert-OpsConnectionSettings -DatabaseHost $DatabaseHost -Port $Port -UserName $User
    Set-OpsPgEnvironment -SslMode $SslMode -AppName 'aegisflow-export-tenant'

    $readmeTemplate = Join-Path $PSScriptRoot 'tenant-export-readme.md'
    if (-not (Test-Path -LiteralPath $readmeTemplate -PathType Leaf)) {
        throw "export README template not found: $readmeTemplate"
    }

    $psql = Resolve-OpsTool -ToolName 'psql' -ExplicitPath $PsqlPath -EnvironmentVariable 'AEGIS_PSQL_PATH'
    Write-OpsStage 'tenant export'
    Write-OpsLog "source:   database=$Database host=${DatabaseHost}:$Port user=$User"
    Write-OpsLog "tenant:   slug=$TenantSlug"
    Write-OpsLog "psql:     $psql ($(Get-OpsToolVersion -ToolPath $psql))"

    $schemaLines = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User `
            -Sql 'SELECT COALESCE(MAX(version),0) FROM schema_migrations')
    if ($schemaLines.Count -ne 1) { throw "could not read schema_migrations from database '$Database'" }
    $schemaVersion = [int]$schemaLines[0]
    if ($schemaVersion -lt 19) {
        throw "database '$Database' is at schema version $schemaVersion; tenant export requires version 19 (multi-tenant business isolation). Run the server migrations first"
    }

    $tenantJson = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User `
            -Sql "SELECT json_build_object('id',id,'slug',slug,'name',name,'status',status,'createdAt',created_at,'updatedAt',updated_at)::text FROM tenants WHERE slug = '$TenantSlug'")
    if ($tenantJson.Count -eq 0) {
        throw "tenant '$TenantSlug' does not exist in database '$Database'"
    }
    $tenant = $tenantJson[0] | ConvertFrom-Json
    Assert-OpsTenantId -TenantId $tenant.id
    if ($tenant.slug -ne $TenantSlug) { throw "resolved tenant slug '$($tenant.slug)' does not match '$TenantSlug'" }
    Write-OpsLog "resolved: id=$($tenant.id) name=$($tenant.name) status=$($tenant.status) schema=v$schemaVersion"

    $columnMap = Get-RequiredColumnList -Psql $psql
    Write-OpsLog ("tables:   " + $columnMap.Count + ' tenant-scoped tables')

    $outputRoot = Resolve-OpsPath -Path $OutputDir
    if (-not (Test-Path -LiteralPath $outputRoot -PathType Container)) {
        New-Item -ItemType Directory -Path $outputRoot -Force | Out-Null
        Write-OpsLog "created output parent directory $outputRoot"
    }
    $exportDirectory = Join-Path $outputRoot ('tenant-' + $TenantSlug + '-' + (Get-OpsLocalTimestamp))
    if (Test-Path -LiteralPath $exportDirectory) { throw "refusing to overwrite an existing export directory: $exportDirectory" }

    $createdDirectory = $false
    try {
        New-Item -ItemType Directory -Path $exportDirectory | Out-Null
        $createdDirectory = $true

        # --- consistent snapshot plus one export file per table -----------------
        $definitions = Get-OpsTenantTableDefinitions
        $scriptLines = New-Object System.Collections.Generic.List[string]
        $scriptLines.Add('BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;')
        $exportedFiles = @{}
        foreach ($definition in $definitions) {
            $scope = Format-OpsTenantScope -ScopeSql $definition.ScopeSql -TenantId $tenant.id
            $csvPath = Join-Path $exportDirectory ($definition.Name + '.csv')
            $exportedFiles[$definition.Name] = $csvPath
            $psqlCsvPath = ConvertTo-OpsPsqlPath -Path $csvPath
            $scriptLines.Add("\copy (" + $scope + " ORDER BY " + $definition.OrderBy + ") TO '" + $psqlCsvPath + "' CSV HEADER")
        }
        $countSelects = Get-OpsTenantCountSelects -TenantId $tenant.id
        $scriptLines.Add(($countSelects -join "`nUNION ALL`n") + ';')
        $scriptLines.Add('COMMIT;')

        $exportScript = New-OpsTempFile -Prefix 'aegisflow-export-tenant'
        try {
            Write-OpsTextFile -Path $exportScript -Text ($scriptLines -join "`n")
            Write-OpsStage 'copy tenant rows (single repeatable-read snapshot)'
            $psqlArguments = @(
                '-X', '-w', '-q', '-A', '-t', '-F', '|',
                '-v', 'ON_ERROR_STOP=1',
                '-h', $DatabaseHost, '-p', [string]$Port, '-d', $Database,
                '-U', $User, '-f', $exportScript
            )
            $countLines = & $psql @psqlArguments
            $exportExit = $LASTEXITCODE
            if ($exportExit -ne 0) { throw "psql exited with code $exportExit while exporting tenant '$TenantSlug'; the partial export directory will be removed" }
        }
        finally {
            Remove-OpsFile -Path $exportScript
        }

        $rowCounts = @{}
        foreach ($line in $countLines) {
            if ($line -match '^ROWCOUNT\|([a-z][a-z0-9_]*)\|([0-9]+)$') {
                $rowCounts[$matches[1]] = [long]$matches[2]
            }
        }
        if ($rowCounts.Count -ne $definitions.Count) {
            throw "could not read all tenant row counts (got $($rowCounts.Count) of $($definitions.Count)); the export is incomplete"
        }

        # --- verify headers, hash files, build the manifest ---------------------
        Write-OpsStage 'verify CSV headers and compute checksums'
        $tableEntries = @()
        $totalRows = [long]0
        foreach ($definition in $definitions) {
            $csvPath = $exportedFiles[$definition.Name]
            if (-not (Test-Path -LiteralPath $csvPath -PathType Leaf)) { throw "export file is missing: $csvPath" }
            $expectedHeader = $columnMap[$definition.Name]
            $actualHeader = Get-FirstLine -Path $csvPath
            if ($actualHeader -ne $expectedHeader) {
                throw "CSV header mismatch for '$($definition.Name)': expected [$expectedHeader] got [$actualHeader]"
            }
            $fileInfo = Get-Item -LiteralPath $csvPath
            $rows = [long]$rowCounts[$definition.Name]
            $totalRows += $rows
            $tableEntries += [pscustomobject][ordered]@{
                name    = $definition.Name
                file    = $definition.Name + '.csv'
                rows    = $rows
                bytes   = [long]$fileInfo.Length
                sha256  = Get-OpsSha256 -Path $csvPath
                columns = @($expectedHeader.Split(','))
            }
        }

        $manifest = [pscustomobject][ordered]@{
            format         = 'aegisflow-tenant-export'
            formatVersion  = 1
            generatedAtUtc = (Get-OpsUtcTimestamp)
            schemaVersion  = $schemaVersion
            tool           = [pscustomobject][ordered]@{
                script  = 'export-tenant.ps1'
                version = $script:OpsToolVersion
                psql    = (Get-OpsToolVersion -ToolPath $psql)
            }
            source         = [pscustomobject][ordered]@{
                host     = $DatabaseHost
                port     = $Port
                database = $Database
            }
            tenant         = [pscustomobject][ordered]@{
                id        = [string]$tenant.id
                slug      = [string]$tenant.slug
                name      = [string]$tenant.name
                status    = [string]$tenant.status
                createdAt = [long]$tenant.createdAt
                updatedAt = [long]$tenant.updatedAt
            }
            tables         = $tableEntries
            totalRows      = $totalRows
            excludedTables = @('auth_sessions', 'auth_refresh_tokens', 'oidc_login_states')
            notes          = @(
                'secrets.ciphertext and deployment_targets.secret_cipher are sealed with AEGIS_SECRET_KEY: import/decryption requires the same key and, during a rotation, AEGIS_SECRET_KEY_PREVIOUS.',
                'auth_users.password_hash is the stored bcrypt hash, never a plaintext password.',
                'Import with import-tenant.ps1 into a database already migrated to at least schemaVersion; project_members is exported even though it has no tenant_id column.',
                'The .sha256 values cover the CSV files as exported; import-tenant.ps1 verifies them before writing anything.'
            )
        }
        $manifestPath = Join-Path $exportDirectory 'manifest.json'
        Write-OpsTextFile -Path $manifestPath -Text (($manifest | ConvertTo-Json -Depth 8) + "`n")
        Copy-Item -LiteralPath $readmeTemplate -Destination (Join-Path $exportDirectory 'README.md')

        Write-OpsStage 'export result'
        Write-OpsLog "directory:     $exportDirectory"
        Write-OpsLog "tenant:        id=$($tenant.id) slug=$($tenant.slug) name=$($tenant.name)"
        Write-OpsLog "schema:        version $schemaVersion"
        Write-OpsLog ("files:         " + $definitions.Count + ' CSV files + manifest.json + README.md')
        foreach ($entry in $tableEntries) {
            Write-OpsLog ('  ' + $entry.name.PadRight(30) + ' ' + ([string]$entry.rows).PadLeft(8) + ' rows  ' + (Format-OpsByteSize -Bytes $entry.bytes))
        }
        Write-OpsLog ('total rows:    ' + $totalRows)
        Write-OpsLog 'export: OK'
        Write-OpsLog ''
        Write-OpsLog 'Keep this directory encrypted and access controlled. It contains user password hashes, audit rows and'
        Write-OpsLog 'sealed credentials; the secrets CSV is only useful with the AEGIS_SECRET_KEY that sealed it.'
    }
    catch {
        if ($createdDirectory -and (Test-Path -LiteralPath $exportDirectory)) {
            Remove-Item -LiteralPath $exportDirectory -Recurse -Force -ErrorAction SilentlyContinue
            Write-OpsWarn "removed the partial export directory $exportDirectory"
        }
        throw
    }
}

try {
    Invoke-ExportMain
}
catch {
    Write-OpsError ('export failed: ' + $_.Exception.Message)
    exit 1
}
exit 0
