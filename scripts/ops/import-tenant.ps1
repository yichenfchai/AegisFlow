#Requires -Version 5.1
<#
.SYNOPSIS
Imports one AegisFlow tenant export (see export-tenant.ps1) into a database.

.DESCRIPTION
Validates the export manifest, every CSV checksum and every CSV header, checks
that the target schema is new enough, then imports all rows in one transaction.
Ordering is foreign-key safe; project_members is restored through its projects.
The transaction also compares every imported table row count with the manifest
and verifies the tenant row/slug, so a partial or damaged export rolls back
without leaving a half-imported tenant.

Safety rules:
  * the target database must already be migrated (schema >= manifest
    schemaVersion); it is never migrated by this script;
  * a target schema NEWER than the export is rejected unless -AllowNewerSchema
    is passed explicitly (review the newer migrations first);
  * an existing tenant with the same slug or the same tenant id is rejected
    unless -Force is passed; -Force deletes that tenant's rows in the same
    transaction before re-importing, never touches other tenants;
  * nothing is written before all checksums and headers pass.

After a successful import the tenant's sealed secrets still require the same
AEGIS_SECRET_KEY that sealed them (plus AEGIS_SECRET_KEY_PREVIOUS while a
rotation is in progress). The deployment's AEGIS_DEFAULT_TENANT_SLUG must still
exist in the target database, otherwise the server refuses to start.

The password is never a parameter; use PGPASSWORD/PGPASSFILE/pgpass or a
peer/trust connection.

.PARAMETER TenantDir
The "tenant-<slug>-<timestamp>" directory produced by export-tenant.ps1.

.PARAMETER Database
Target database name (required). Must already be migrated by the server.

.PARAMETER Force
Replace an existing tenant with the same slug (or the same tenant id) in the
target database. Destructive: its current rows are deleted inside the import
transaction. Other tenants are not touched.

.PARAMETER AllowNewerSchema
Allow importing into a target schema whose migration version is higher than the
export's schemaVersion. Only meaningful for additive migrations; verify the
newer schema first.

.EXAMPLE
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/import-tenant.ps1 `
  -TenantDir D:\tenant-moves\tenant-acme-20261128-231500 -Database aegisflow_drill `
  -Host db.internal -Port 5432 -User aegis_backup
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $TenantDir,

    [Parameter(Mandatory = $true)] [string] $Database,

    [Alias('Host')] [string] $DatabaseHost = '127.0.0.1',

    [ValidateRange(1, 65535)] [int] $Port = 5433,

    [string] $User = 'postgres',

    [switch] $Force,

    [switch] $AllowNewerSchema,

    [string] $PsqlPath,

    [string] $SslMode
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'ops-common.ps1')

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

function Read-TenantManifest {
    param([string] $Directory)

    $manifestPath = Join-Path $Directory 'manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        throw "manifest.json not found in '$Directory'; point -TenantDir at an export-tenant.ps1 output directory"
    }
    $manifest = [System.IO.File]::ReadAllText($manifestPath, $script:OpsUtf8NoBom) | ConvertFrom-Json
    if ($manifest.format -ne 'aegisflow-tenant-export') { throw "unexpected manifest format '$($manifest.format)'; expected aegisflow-tenant-export" }
    if ([int]$manifest.formatVersion -ne 1) { throw "unsupported manifest formatVersion '$($manifest.formatVersion)'; this script supports 1" }
    Assert-OpsTenantId -TenantId ([string]$manifest.tenant.id)
    Assert-OpsTenantSlug -Slug ([string]$manifest.tenant.slug)
    if ([int]$manifest.schemaVersion -lt 19) { throw "manifest schemaVersion $($manifest.schemaVersion) is older than v19; no AegisFlow multi-tenant export was written by that version" }
    if (-not $manifest.tables) { throw 'manifest.json lists no tables' }
    return [pscustomobject]@{ Path = $manifestPath; Data = $manifest }
}

function Get-ValidatedTableMap {
    param($Manifest, [string] $Directory)

    $definitions = Get-OpsTenantTableDefinitions
    $expectedNames = @($definitions | ForEach-Object { $_.Name })
    $map = @{}
    foreach ($entry in $Manifest.tables) {
        $name = [string]$entry.name
        Assert-OpsSchemaObjectName -Name $name -Kind 'table name'
        if ($map.ContainsKey($name)) { throw "manifest.json lists table '$name' twice" }
        if ($expectedNames -notcontains $name) { throw "manifest.json lists table '$name' that is not part of the tenant export set" }
        $fileName = [string]$entry.file
        if ($fileName -ne ($name + '.csv')) { throw "manifest entry for '$name' must reference '$name.csv', got '$fileName'" }
        $csvPath = Join-Path $Directory $fileName
        if (-not (Test-Path -LiteralPath $csvPath -PathType Leaf)) { throw "export file missing: $csvPath" }
        $columns = @($entry.columns)
        if ($columns.Count -eq 0) { throw "manifest entry for '$name' has no column list" }
        foreach ($column in $columns) { Assert-OpsSchemaObjectName -Name ([string]$column) -Kind 'column name' }
        $expectedHeader = ($columns -join ',')
        $actualHeader = Get-FirstLine -Path $csvPath
        if ($actualHeader -ne $expectedHeader) {
            throw "CSV header mismatch for '$name': manifest [$expectedHeader] file [$actualHeader]"
        }
        $actualHash = Get-OpsSha256 -Path $csvPath
        $manifestHash = ([string]$entry.sha256).ToLowerInvariant()
        if ($manifestHash -notmatch '^[0-9a-f]{64}$') { throw "manifest entry for '$name' has no valid sha256" }
        if ($actualHash -ne $manifestHash) { throw "SHA-256 mismatch for '$name.csv': manifest=$manifestHash actual=$actualHash" }
        if ($entry.bytes -and ([long]$entry.bytes -ne (Get-Item -LiteralPath $csvPath).Length)) {
            throw "size mismatch for '$name.csv': manifest=$($entry.bytes) actual=$((Get-Item -LiteralPath $csvPath).Length)"
        }
        $map[$name] = [pscustomobject]@{
            Name    = $name
            File    = $csvPath
            Rows    = [long]$entry.rows
            Columns = $columns
        }
    }
    $missing = @($expectedNames | Where-Object { -not $map.ContainsKey($_) })
    if ($missing.Count -gt 0) { throw "manifest.json is incomplete; missing tables: $($missing -join ', ')" }
    return $map
}

function Assert-TargetColumns {
    param([string] $Psql, $TableMap, [string[]] $TableNames)

    $inList = ($TableNames | ForEach-Object { "'" + $_ + "'" }) -join ','
    $sql = "SELECT table_name || '|' || string_agg(column_name, ',' ORDER BY ordinal_position) " +
        "FROM information_schema.columns WHERE table_schema = 'public' AND table_name IN ($inList) " +
        "GROUP BY table_name"
    $lines = @(Invoke-OpsPsqlQuery -PsqlPath $Psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User -Sql $sql)
    $targetColumns = @{}
    foreach ($line in $lines) {
        if ($line -match '^([a-z][a-z0-9_]*)\|([a-z0-9_,]+)$') { $targetColumns[$matches[1]] = $matches[2] }
    }
    foreach ($name in $TableNames) {
        if (-not $targetColumns.ContainsKey($name)) { throw "target database '$Database' is missing table '$name'; migrate it with the server binary first" }
        $available = @($targetColumns[$name].Split(','))
        foreach ($column in $TableMap[$name].Columns) {
            if ($available -notcontains [string]$column) {
                throw "target table '$name' has no column '$column'; the target schema is not compatible with this export"
            }
        }
    }
}

function Get-RowCountMap {
    param([string] $Psql, [string] $TenantId)

    $selects = Get-OpsTenantCountSelects -TenantId $TenantId
    $lines = @(Invoke-OpsPsqlQuery -PsqlPath $Psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User -Sql ($selects -join "`nUNION ALL`n"))
    $counts = @{}
    foreach ($line in $lines) {
        if ($line -match '^ROWCOUNT\|([a-z][a-z0-9_]*)\|([0-9]+)$') { $counts[$matches[1]] = [long]$matches[2] }
    }
    return $counts
}

function Invoke-ImportMain {
    if (-not (Test-Path -LiteralPath $TenantDir -PathType Container)) { throw "tenant export directory not found: $TenantDir" }
    $resolvedDir = (Resolve-Path -LiteralPath $TenantDir).ProviderPath
    Assert-OpsDatabaseName -Name $Database -ParameterName 'Database'
    Assert-OpsConnectionSettings -DatabaseHost $DatabaseHost -Port $Port -UserName $User
    Set-OpsPgEnvironment -SslMode $SslMode -AppName 'aegisflow-import-tenant'

    $psql = Resolve-OpsTool -ToolName 'psql' -ExplicitPath $PsqlPath -EnvironmentVariable 'AEGIS_PSQL_PATH'

    Write-OpsStage 'tenant import'
    Write-OpsLog "export:   $resolvedDir"
    Write-OpsLog "target:   database=$Database host=${DatabaseHost}:$Port user=$User force=$($Force.IsPresent) allowNewerSchema=$($AllowNewerSchema.IsPresent)"
    Write-OpsLog "psql:     $psql ($(Get-OpsToolVersion -ToolPath $psql))"

    Write-OpsStage 'validate export'
    $manifestResult = Read-TenantManifest -Directory $resolvedDir
    $manifest = $manifestResult.Data
    $tableMap = Get-ValidatedTableMap -Manifest $manifest -Directory $resolvedDir
    $tenantId = [string]$manifest.tenant.id
    $tenantSlug = [string]$manifest.tenant.slug
    $exportSchemaVersion = [int]$manifest.schemaVersion
    Write-OpsLog "manifest: tenant=$tenantSlug id=$tenantId schema=v$exportSchemaVersion generated=$($manifest.generatedAtUtc)"
    Write-OpsLog ("checksums:" + ' all ' + $tableMap.Count + ' CSV files match manifest.json')

    Write-OpsStage 'check target schema'
    $schemaLines = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User `
            -Sql 'SELECT COALESCE(MAX(version),0) FROM schema_migrations')
    if ($schemaLines.Count -ne 1) { throw "could not read schema_migrations from target database '$Database'" }
    $targetSchemaVersion = [int]$schemaLines[0]
    Write-OpsLog "target schema: version $targetSchemaVersion (export v$exportSchemaVersion)"
    if ($targetSchemaVersion -lt $exportSchemaVersion) {
        throw "target database '$Database' is at schema version $targetSchemaVersion but the export needs v$exportSchemaVersion; start the target server once so it applies the migrations, then retry"
    }
    if ($targetSchemaVersion -gt $exportSchemaVersion -and -not $AllowNewerSchema) {
        throw "target database '$Database' is newer (v$targetSchemaVersion) than the export (v$exportSchemaVersion); review the newer migrations and rerun with -AllowNewerSchema only when they are additive"
    }

    $tableNames = @(Get-OpsTenantTableDefinitions | ForEach-Object { $_.Name })
    Assert-TargetColumns -Psql $psql -TableMap $tableMap -TableNames $tableNames

    Write-OpsStage 'check existing tenant'
    $existingLines = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User `
            -Sql "SELECT id || '|' || slug FROM tenants WHERE slug = '$tenantSlug' OR id = '$tenantId'")
    $existing = @()
    foreach ($line in $existingLines) {
        if ($line -match '^([A-Za-z0-9_-]+)\|([a-z0-9-]+)$') {
            $existing += [pscustomobject]@{ Id = $matches[1]; Slug = $matches[2] }
        }
    }
    $existingId = $null
    if ($existing.Count -gt 1) {
        throw "target database '$Database' has multiple tenants matching the export (slug '$tenantSlug' or id '$tenantId'); resolve this manually before importing"
    }
    if ($existing.Count -eq 1) {
        if (-not $Force) {
            throw "target database '$Database' already contains tenant '$($existing[0].Slug)' (id $($existing[0].Id)); refusing to import without -Force (that would replace its data)"
        }
        $existingId = $existing[0].Id
        Write-OpsLog "existing tenant '$($existing[0].Slug)' (id $existingId) will be replaced inside the import transaction (-Force)"
    }
    else {
        Write-OpsLog "no tenant with slug '$tenantSlug' or id '$tenantId' exists in '$Database'; importing into an empty tenant scope"
    }

    Write-OpsStage 'import in one transaction'
    $scriptLines = New-Object System.Collections.Generic.List[string]
    $scriptLines.Add('BEGIN;')
    if ($existingId) {
        foreach ($statement in Get-OpsTenantDeleteStatements -TenantId $existingId) {
            $scriptLines.Add($statement + ';')
        }
    }
    foreach ($name in $tableNames) {
        $entry = $tableMap[$name]
        $psqlCsvPath = ConvertTo-OpsPsqlPath -Path $entry.File
        $columnList = (@($entry.Columns) -join ', ')
        $scriptLines.Add("\copy " + $name + ' (' + $columnList + ") FROM '" + $psqlCsvPath + "' CSV HEADER")
    }
    $scriptLines.Add((Get-OpsSequenceRepairSql))

    # In-transaction verification: every scoped row count must equal the
    # manifest and the tenant row itself must carry the exported id and slug.
    $scriptLines.Add('CREATE TEMP TABLE aegis_import_expected(table_name text PRIMARY KEY, rows bigint NOT NULL) ON COMMIT DROP;')
    $expectedValues = @()
    foreach ($name in $tableNames) { $expectedValues += "('$name', " + [string]$tableMap[$name].Rows + ')' }
    $scriptLines.Add('INSERT INTO aegis_import_expected(table_name, rows) VALUES ' + ($expectedValues -join ', ') + ';')

    $actualCounts = @()
    foreach ($name in $tableNames) {
        $countSql = Get-OpsTenantScopeSql -TableName $name -TenantId $tenantId
        $actualCounts += "SELECT '$name' AS table_name, count(*) AS rows FROM (" + $countSql + ") AS aegis_scope"
    }
    $scriptLines.Add(@'
DO $aegis_import_check$
DECLARE
  mismatch text;
BEGIN
  SELECT string_agg(e.table_name || ' expected=' || e.rows || ' actual=' || COALESCE(a.rows::text, 'missing'), '; ' ORDER BY e.table_name)
    INTO mismatch
  FROM aegis_import_expected e
  LEFT JOIN (
'@)
    $scriptLines.Add('    ' + ($actualCounts -join "`n    UNION ALL`n"))
    $checkTail = @'
  ) a ON a.table_name = e.table_name
  WHERE a.rows IS DISTINCT FROM e.rows;
  IF mismatch IS NOT NULL THEN
    RAISE EXCEPTION 'tenant import row count mismatch: %', mismatch;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM tenants WHERE id = '<tenantId>' AND slug = '<tenantSlug>') THEN
    RAISE EXCEPTION 'imported tenant row is missing or its slug does not match the manifest';
  END IF;
END
$aegis_import_check$;
'@
    $checkTail = $checkTail.Replace('<tenantId>', $tenantId).Replace('<tenantSlug>', $tenantSlug)
    $scriptLines.Add($checkTail)
    $scriptLines.Add('COMMIT;')

    $importScript = New-OpsTempFile -Prefix 'aegisflow-import-tenant'
    try {
        Write-OpsTextFile -Path $importScript -Text ($scriptLines -join "`n")
        $psqlArguments = @(
            '-X', '-w', '-q', '-A', '-t', '-F', '|',
            '-v', 'ON_ERROR_STOP=1',
            '-h', $DatabaseHost, '-p', [string]$Port, '-d', $Database,
            '-U', $User, '-f', $importScript
        )
        Invoke-OpsNative -ToolPath $psql -Arguments $psqlArguments
        $importExit = $LASTEXITCODE
    }
    finally {
        Remove-OpsFile -Path $importScript
    }
    if ($importExit -ne 0) {
        throw "psql exited with code $importExit during the tenant import; the transaction was rolled back and database '$Database' is unchanged. Check the psql error above (checksum/header/schema problems are reported before any write)"
    }

    Write-OpsStage 'post-import verification'
    $counts = Get-RowCountMap -Psql $psql -TenantId $tenantId
    if ($counts.Count -ne $tableNames.Count) {
        throw "could not read all post-import row counts (got $($counts.Count) of $($tableNames.Count))"
    }
    foreach ($name in $tableNames) {
        if ($counts[$name] -ne $tableMap[$name].Rows) {
            throw "post-import row count mismatch for '$name': manifest=$($tableMap[$name].Rows) actual=$($counts[$name])"
        }
    }
    $tenantLines = @(Invoke-OpsPsqlQuery -PsqlPath $psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User `
            -Sql "SELECT json_build_object('id',id,'slug',slug,'name',name,'status',status)::text FROM tenants WHERE id = '$tenantId'")
    if ($tenantLines.Count -ne 1) { throw "imported tenant '$tenantId' is not readable after import" }
    $importedTenant = $tenantLines[0] | ConvertFrom-Json
    if ($importedTenant.slug -ne $tenantSlug) { throw "imported tenant slug '$($importedTenant.slug)' does not match '$tenantSlug'" }

    Write-OpsStage 'import result'
    Write-OpsLog "database:   $Database (schema version $targetSchemaVersion)"
    Write-OpsLog "tenant:     id=$($importedTenant.id) slug=$($importedTenant.slug) name=$($importedTenant.name) status=$($importedTenant.status)"
    foreach ($name in $tableNames) {
        Write-OpsLog ('  ' + $name.PadRight(30) + ' ' + ([string]$counts[$name]).PadLeft(8) + ' rows')
    }
    $totalRows = [long]0
    foreach ($name in $tableNames) { $totalRows += $counts[$name] }
    Write-OpsLog ('total rows: ' + $totalRows)
    Write-OpsLog 'import: OK'
    Write-OpsLog ''
    Write-OpsLog 'Next steps:'
    Write-OpsLog '  1. keep AEGIS_SECRET_KEY (and AEGIS_SECRET_KEY_PREVIOUS during a rotation) identical to the source'
    Write-OpsLog '     deployment, otherwise secrets/deployment-target credentials in the imported rows cannot be decrypted.'
    Write-OpsLog '  2. keep AEGIS_DEFAULT_TENANT_SLUG pointing at a tenant that exists in this database, then start/reload'
    Write-OpsLog '     the API and verify the tenant login (tenantSlug), project list and /readyz.'
}

try {
    Invoke-ImportMain
}
catch {
    Write-OpsError ('import failed: ' + $_.Exception.Message)
    exit 1
}
exit 0
