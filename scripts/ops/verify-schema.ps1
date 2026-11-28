#Requires -Version 5.1
<#
.SYNOPSIS
Verifies the AegisFlow PostgreSQL schema after a migration, restore or import.

.DESCRIPTION
Runs read-only checks against one database and reports each result:

  * schema_migrations exists, is contiguous from 1 and (optionally) matches
    -ExpectedVersion;
  * tenants exists and holds at least one row;
  * all 24 schema-v19 tables exist;
  * the 14 business tables have a NOT NULL tenant_id text column and a foreign
    key to tenants(id);
  * auth_users/auth_sessions/auth_audit have a NOT NULL tenant_id foreign key
    (oidc_login_states keeps its nullable FK);
  * 5 representative tenant-scoped unique indexes exist and are unique;
  * row counts for every table are printed as a summary.

Exit code 0 means every check passed; any failure prints "[FAIL]" lines, a
summary and exits 1 (script errors exit 1 as well). The script never writes to
the database and never prints credentials.

.PARAMETER Database
Database to verify (required).

.PARAMETER ExpectedVersion
Optional migration version the database must be at (for example 19 for the
schema that introduced tenant isolation). Omit to accept any contiguous version.

.EXAMPLE
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/verify-schema.ps1 `
  -Database aegisflow_restore_drill -Host db.internal -Port 5432 -User aegis_backup -ExpectedVersion 19
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $Database,

    [Alias('Host')] [string] $DatabaseHost = '127.0.0.1',

    [ValidateRange(1, 65535)] [int] $Port = 5433,

    [string] $User = 'postgres',

    [int] $ExpectedVersion = 0,

    [string] $PsqlPath,

    [string] $SslMode
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'ops-common.ps1')

$script:Failures = New-Object System.Collections.Generic.List[string]
$script:PassCount = 0
$script:CheckCount = 0

function Add-CheckResult {
    param([bool] $Ok, [string] $Message)
    $script:CheckCount++
    if ($Ok) {
        $script:PassCount++
        Write-OpsLog ('[PASS] ' + $Message)
    }
    else {
        $script:Failures.Add($Message)
        Write-OpsLog ('[FAIL] ' + $Message)
    }
}

function Get-QueryLines {
    param([string] $Sql)
    return @(Invoke-OpsPsqlQuery -PsqlPath $script:Psql -Database $Database -DatabaseHost $DatabaseHost -Port $Port -UserName $User -Sql $Sql)
}

function Invoke-VerifyMain {
    Assert-OpsDatabaseName -Name $Database -ParameterName 'Database'
    Assert-OpsConnectionSettings -DatabaseHost $DatabaseHost -Port $Port -UserName $User
    Set-OpsPgEnvironment -SslMode $SslMode -AppName 'aegisflow-verify-schema'

    $script:Psql = Resolve-OpsTool -ToolName 'psql' -ExplicitPath $PsqlPath -EnvironmentVariable 'AEGIS_PSQL_PATH'

    $businessTables = @(
        'projects', 'builds', 'deployment_targets', 'deployments', 'approvals', 'approval_notifications',
        'reviews', 'review_external_results', 'review_notifications', 'scans', 'security_events',
        'knowledge_docs', 'secrets', 'project_policies'
    )
    $requiredTables = @(
        'schema_migrations', 'tenants',
        'auth_users', 'auth_sessions', 'auth_refresh_tokens', 'auth_audit', 'oidc_login_states',
        'projects', 'project_members', 'builds', 'build_log_events',
        'deployment_targets', 'deployments', 'deployment_events',
        'approvals', 'approval_notifications',
        'reviews', 'review_external_results', 'review_notifications',
        'scans', 'security_events', 'knowledge_docs', 'secrets', 'project_policies'
    )
    $authOwnedTables = @('auth_users', 'auth_sessions', 'auth_audit')
    $expectedIndexes = @(
        [pscustomobject]@{ Name = 'projects_tenant_name_lower_idx'; Table = 'projects' },
        [pscustomobject]@{ Name = 'deployment_targets_tenant_name_lower_idx'; Table = 'deployment_targets' },
        [pscustomobject]@{ Name = 'secrets_tenant_name_key'; Table = 'secrets' },
        [pscustomobject]@{ Name = 'auth_users_tenant_username_lower_idx'; Table = 'auth_users' },
        [pscustomobject]@{ Name = 'reviews_project_number_idx'; Table = 'reviews' }
    )

    Write-OpsStage 'schema verification'
    Write-OpsLog "database: $Database host=${DatabaseHost}:$Port user=$User"
    Write-OpsLog "psql:     $($script:Psql) ($(Get-OpsToolVersion -ToolPath $script:Psql))"
    if ($ExpectedVersion -gt 0) { Write-OpsLog "expected schema version: $ExpectedVersion" }

    # --- migration ledger ---------------------------------------------------
    Write-OpsStage 'migration ledger'
    $ledgerExists = $false
    $ledgerLines = @(Get-QueryLines -Sql "SELECT COALESCE(to_regclass('public.schema_migrations')::text, '')")
    if ($ledgerLines.Count -eq 1 -and $ledgerLines[0] -eq 'schema_migrations') { $ledgerExists = $true }
    Add-CheckResult -Ok $ledgerExists -Message 'schema_migrations table exists'
    $schemaVersion = 0
    if ($ledgerExists) {
        $ledgerParts = @(Get-QueryLines -Sql 'SELECT COALESCE(MAX(version),0)::text || ''|'' || COUNT(*)::text || ''|'' || COALESCE(MIN(version),0)::text FROM schema_migrations')
        if ($ledgerParts.Count -eq 1 -and $ledgerParts[0] -match '^([0-9]+)\|([0-9]+)\|([0-9]+)$') {
            $schemaVersion = [int]$matches[1]
            $ledgerRows = [int]$matches[2]
            $ledgerMin = [int]$matches[3]
            Add-CheckResult -Ok ($schemaVersion -ge 1) -Message ("schema_migrations has at least one applied migration (max version $schemaVersion)")
            Add-CheckResult -Ok ($ledgerRows -eq $schemaVersion -and $ledgerMin -eq 1) -Message ("schema_migrations is contiguous from 1 to $schemaVersion ($ledgerRows rows)")
            if ($ExpectedVersion -gt 0) {
                Add-CheckResult -Ok ($schemaVersion -eq $ExpectedVersion) -Message ("schema version is $schemaVersion (expected $ExpectedVersion)")
            }
        }
        else {
            Add-CheckResult -Ok $false -Message 'could not read schema_migrations max/count/min version'
        }
    }

    # --- required tables ----------------------------------------------------
    Write-OpsStage 'tables'
    $presentTables = @(Get-QueryLines -Sql "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name")
    $missingTables = @($requiredTables | Where-Object { $presentTables -notcontains $_ })
    Add-CheckResult -Ok ($missingTables.Count -eq 0) -Message ("all " + $requiredTables.Count + ' required schema-v19 tables exist' + $(if ($missingTables.Count -gt 0) { ': missing ' + ($missingTables -join ', ') } else { '' }))

    # --- tenant ownership columns ------------------------------------------
    Write-OpsStage 'tenant ownership columns'
    $columnLines = @(Get-QueryLines -Sql "SELECT table_name || '|' || is_nullable || '|' || data_type FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'tenant_id' ORDER BY table_name")
    $columnInfo = @{}
    foreach ($line in $columnLines) {
        if ($line -match '^([a-z][a-z0-9_]*)\|(YES|NO)\|([a-z ]+)$') {
            $columnInfo[$matches[1]] = [pscustomobject]@{ Nullable = $matches[2]; DataType = $matches[3] }
        }
    }
    foreach ($table in $businessTables) {
        if (-not $columnInfo.ContainsKey($table)) {
            Add-CheckResult -Ok $false -Message "business table '$table' has a tenant_id column"
        }
        else {
            $info = $columnInfo[$table]
            Add-CheckResult -Ok ($info.Nullable -eq 'NO' -and $info.DataType -eq 'text') -Message "business table '$table'.tenant_id is NOT NULL text"
        }
    }
    foreach ($table in $authOwnedTables) {
        if (-not $columnInfo.ContainsKey($table)) {
            Add-CheckResult -Ok $false -Message "auth table '$table' has a tenant_id column"
        }
        else {
            $info = $columnInfo[$table]
            Add-CheckResult -Ok ($info.Nullable -eq 'NO' -and $info.DataType -eq 'text') -Message "auth table '$table'.tenant_id is NOT NULL text"
        }
    }
    if ($columnInfo.ContainsKey('oidc_login_states')) {
        Add-CheckResult -Ok ($columnInfo['oidc_login_states'].DataType -eq 'text') -Message "oidc_login_states.tenant_id is text (nullable by design)"
    }
    else {
        Add-CheckResult -Ok $false -Message 'oidc_login_states has a tenant_id column'
    }

    # --- tenant foreign keys ------------------------------------------------
    Write-OpsStage 'tenant foreign keys'
    $fkLines = @(Get-QueryLines -Sql "SELECT DISTINCT cl.relname FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey) JOIN pg_class cl ON cl.oid = c.conrelid JOIN pg_namespace n ON n.oid = cl.relnamespace JOIN pg_class ref ON ref.oid = c.confrelid JOIN pg_namespace rn ON rn.oid = ref.relnamespace WHERE c.contype = 'f' AND a.attname = 'tenant_id' AND n.nspname = 'public' AND ref.relname = 'tenants' AND rn.nspname = 'public' ORDER BY cl.relname")
    $fkTables = @($fkLines | Where-Object { $_ -match '^[a-z][a-z0-9_]*$' })
    foreach ($table in $businessTables) {
        Add-CheckResult -Ok ($fkTables -contains $table) -Message "business table '$table' has a foreign key tenant_id -> tenants(id)"
    }
    foreach ($table in @('auth_users', 'auth_sessions', 'auth_audit', 'oidc_login_states')) {
        Add-CheckResult -Ok ($fkTables -contains $table) -Message "auth table '$table' has a foreign key tenant_id -> tenants(id)"
    }

    # --- tenants table summary ---------------------------------------------
    Write-OpsStage 'tenants'
    if ($presentTables -contains 'tenants') {
        $tenantCountLines = @(Get-QueryLines -Sql 'SELECT count(*)::text FROM tenants')
        $tenantCount = -1
        if ($tenantCountLines.Count -eq 1) { $tenantCount = [int]$tenantCountLines[0] }
        Add-CheckResult -Ok ($tenantCount -ge 1) -Message "tenants table contains at least one tenant (found $tenantCount)"
        if ($tenantCount -ge 1) {
            $tenantLines = @(Get-QueryLines -Sql "SELECT t.slug || '|' || t.status || '|' || count(u.id)::text FROM tenants t LEFT JOIN auth_users u ON u.tenant_id = t.id GROUP BY t.id, t.slug, t.status ORDER BY t.slug")
            foreach ($line in $tenantLines) {
                if ($line -match '^([a-z0-9-]+)\|([a-z]+)\|([0-9]+)$') {
                    Write-OpsLog ("  tenant slug=" + $matches[1] + ' status=' + $matches[2] + ' users=' + $matches[3])
                }
            }
        }
    }
    else {
        Add-CheckResult -Ok $false -Message 'tenants table exists'
    }

    # --- representative unique indexes -------------------------------------
    Write-OpsStage 'unique indexes'
    $indexLines = @(Get-QueryLines -Sql "SELECT i.relname || '|' || t.relname || '|' || ix.indisunique::text FROM pg_index ix JOIN pg_class i ON i.oid = ix.indexrelid JOIN pg_class t ON t.oid = ix.indrelid JOIN pg_namespace n ON n.oid = t.relnamespace WHERE n.nspname = 'public' AND i.relname IN ('projects_tenant_name_lower_idx','deployment_targets_tenant_name_lower_idx','secrets_tenant_name_key','auth_users_tenant_username_lower_idx','reviews_project_number_idx')")
    $indexInfo = @{}
    foreach ($line in $indexLines) {
        if ($line -match '^([a-z0-9_]+)\|([a-z0-9_]+)\|(true|false)$') {
            $indexInfo[$matches[1]] = [pscustomobject]@{ Table = $matches[2]; Unique = ($matches[3] -eq 'true') }
        }
    }
    foreach ($expected in $expectedIndexes) {
        $ok = $false
        if ($indexInfo.ContainsKey($expected.Name)) {
            $info = $indexInfo[$expected.Name]
            $ok = ($info.Unique -and $info.Table -eq $expected.Table)
        }
        Add-CheckResult -Ok $ok -Message ("unique index '" + $expected.Name + "' exists on " + $expected.Table)
    }

    # --- row-count summary -------------------------------------------------
    Write-OpsStage 'row counts'
    if ($missingTables.Count -eq 0) {
        $countSelects = @()
        foreach ($table in $requiredTables) {
            if ($table -eq 'schema_migrations') { continue }
            $countSelects += "SELECT '$table|' || count(*)::text FROM $table"
        }
        $countLines = @(Get-QueryLines -Sql ($countSelects -join "`nUNION ALL`n"))
        $counts = @{}
        foreach ($line in $countLines) {
            if ($line -match '^([a-z_]+)\|([0-9]+)$') { $counts[$matches[1]] = [long]$matches[2] }
        }
        if ($counts.Count -ne $countSelects.Count) {
            Add-CheckResult -Ok $false -Message "read a row count for every table (got $($counts.Count) of $($countSelects.Count))"
        }
        else {
            Add-CheckResult -Ok $true -Message "read a row count for every table"
            foreach ($table in $requiredTables) {
                if ($counts.ContainsKey($table)) { Write-OpsLog ('  ' + $table.PadRight(30) + ' ' + ([string]$counts[$table]).PadLeft(10)) }
            }
            Write-OpsLog ('  ' + 'schema_migrations'.PadRight(30) + ' ' + ([string]$ledgerRows).PadLeft(10))
        }
    }

    # --- result ------------------------------------------------------------
    Write-OpsStage 'result'
    if ($script:Failures.Count -gt 0) {
        foreach ($failure in $script:Failures) { Write-OpsLog ('[FAIL] ' + $failure) }
        throw ("verify-schema found " + $script:Failures.Count + ' failed check(s); see [FAIL] lines above')
    }
    Write-OpsLog ("checks passed: " + $script:PassCount + '/' + $script:CheckCount)
    if ($schemaVersion -gt 0) { Write-OpsLog "schema version: $schemaVersion" }
    Write-OpsLog 'verify-schema: OK'
}

try {
    Invoke-VerifyMain
}
catch {
    Write-OpsError ('verify-schema failed: ' + $_.Exception.Message)
    exit 1
}
exit 0
