# Common helpers for the AegisFlow operational scripts in scripts/ops.
#
# This file is dot-sourced by backup-postgres.ps1, restore-postgres.ps1,
# export-tenant.ps1, import-tenant.ps1 and verify-schema.ps1 and is not meant to
# be run directly. It deliberately uses only Windows PowerShell 5.1 compatible
# syntax (no pwsh-only operators) so every entry point works with both
# powershell.exe and pwsh.
#
# Security conventions:
#   * A password is never a script parameter and never appears on any command
#     line. libpq resolves it from PGPASSWORD / PGPASSFILE / ~/.pgpass or from a
#     trusted/peer connection, and the scripts never echo that environment.
#   * Every SQL string that embeds a database identifier or tenant id is built
#     only after the matching validator below has rejected quotes and
#     whitespace. File names are quoted for the psql \copy meta-command.
#   * Helper output is plain text on the host stream; callers own the exit code.

$script:OpsUtf8NoBom = New-Object System.Text.UTF8Encoding($false)
$script:OpsToolVersion = '1.0.0'
$script:OpsPgCliFallbackDir = 'D:\AegisFlow-tools\pgsql\pgsql\bin'

function Write-OpsLog {
    param([string] $Message)
    Write-Host ((Get-Date).ToString('HH:mm:ss') + ' ' + $Message)
}

function Write-OpsStage {
    param([string] $Message)
    Write-Host ''
    Write-Host ('== ' + $Message)
}

function Write-OpsWarn {
    param([string] $Message)
    Write-Host ('WARNING: ' + $Message) -ForegroundColor Yellow
}

function Write-OpsError {
    param([string] $Message)
    Write-Host ('ERROR: ' + $Message) -ForegroundColor Red
}

# Resolves a PostgreSQL client executable. Order: explicit parameter, named
# environment variable, sibling of an already resolved tool (keeps pg_dump and
# pg_restore from the same installation), local Windows tooling fallback, PATH.
function Resolve-OpsTool {
    param(
        [Parameter(Mandatory = $true)] [string] $ToolName,
        [string] $ExplicitPath,
        [string] $EnvironmentVariable,
        [string] $SiblingOf
    )
    if ($ExplicitPath) {
        if (-not (Test-Path -LiteralPath $ExplicitPath -PathType Leaf)) {
            throw "$ToolName not found at the explicit path: $ExplicitPath"
        }
        return (Resolve-Path -LiteralPath $ExplicitPath).ProviderPath
    }
    if ($EnvironmentVariable) {
        $fromEnvironment = [System.Environment]::GetEnvironmentVariable($EnvironmentVariable)
        if ($fromEnvironment) {
            if (-not (Test-Path -LiteralPath $fromEnvironment -PathType Leaf)) {
                throw "$ToolName not found at ${EnvironmentVariable}: $fromEnvironment"
            }
            return (Resolve-Path -LiteralPath $fromEnvironment).ProviderPath
        }
    }
    if ($SiblingOf) {
        $candidate = Join-Path (Split-Path -Parent $SiblingOf) ($ToolName + '.exe')
        if (Test-Path -LiteralPath $candidate -PathType Leaf) { return $candidate }
    }
    $fallback = Join-Path $script:OpsPgCliFallbackDir ($ToolName + '.exe')
    if (Test-Path -LiteralPath $fallback -PathType Leaf) { return $fallback }
    $command = Get-Command $ToolName -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($command) { return $command.Source }
    throw "$ToolName not found; install the PostgreSQL client tools, set $EnvironmentVariable, or pass the explicit path parameter"
}

function Get-OpsToolVersion {
    param([Parameter(Mandatory = $true)] [string] $ToolPath)
    $line = & $ToolPath --version
    if ($LASTEXITCODE -ne 0 -or -not $line) { return 'unknown' }
    return (($line | Select-Object -First 1) -join ' ').Trim()
}

function Assert-OpsConnectionSettings {
    param(
        [Parameter(Mandatory = $true)] [string] $DatabaseHost,
        [Parameter(Mandatory = $true)] [int] $Port,
        [Parameter(Mandatory = $true)] [string] $UserName
    )
    if ([string]::IsNullOrWhiteSpace($DatabaseHost)) { throw 'database host must not be empty' }
    if ($Port -lt 1 -or $Port -gt 65535) { throw "database port must be between 1 and 65535, got $Port" }
    if ([string]::IsNullOrWhiteSpace($UserName)) { throw 'database user must not be empty' }
}

function Assert-OpsDatabaseName {
    param(
        [Parameter(Mandatory = $true)] [string] $Name,
        [string] $ParameterName = 'Database'
    )
    if ($Name -notmatch '^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$') {
        throw "$ParameterName '$Name' is not a valid PostgreSQL database name (letters, digits, underscore, dot and hyphen, 1-63 characters)"
    }
}

function Assert-OpsTenantId {
    param([Parameter(Mandatory = $true)] [string] $TenantId)
    if ($TenantId -notmatch '^[A-Za-z0-9_-]{1,64}$') {
        throw "tenant id '$TenantId' is not a safe identifier; refusing to build SQL with it"
    }
}

function Assert-OpsTenantSlug {
    param([Parameter(Mandatory = $true)] [string] $Slug)
    if ($Slug -notmatch '^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$') {
        throw "tenant slug '$Slug' does not match the documented format (1-32 lowercase letters, digits and inner hyphens)"
    }
}

function Assert-OpsSchemaObjectName {
    param(
        [Parameter(Mandatory = $true)] [string] $Name,
        [string] $Kind = 'identifier'
    )
    if ($Name -notmatch '^[a-z][a-z0-9_]{0,62}$') {
        throw "$Kind '$Name' is not a safe lowercase schema object name"
    }
}

# Configures the libpq environment for one script process. -SslMode is optional;
# when omitted an operator-provided PGSSLMODE is left untouched on purpose.
function Set-OpsPgEnvironment {
    param([string] $SslMode, [string] $AppName)
    $env:PGCLIENTENCODING = 'UTF8'
    $env:PGCONNECT_TIMEOUT = '10'
    if ($AppName) { $env:PGAPPNAME = $AppName }
    if ($SslMode) {
        $valid = @('disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full')
        if ($valid -notcontains $SslMode) {
            throw "SslMode must be one of: $($valid -join ', ')"
        }
        $env:PGSSLMODE = $SslMode
    }
}

function Write-OpsTextFile {
    param(
        [Parameter(Mandatory = $true)] [string] $Path,
        [Parameter(Mandatory = $true)] [string] $Text
    )
    [System.IO.File]::WriteAllText($Path, $Text, $script:OpsUtf8NoBom)
}

function New-OpsTempFile {
    param([string] $Prefix = 'aegisflow-ops')
    return (Join-Path ([System.IO.Path]::GetTempPath()) ($Prefix + '-' + [guid]::NewGuid().ToString('N') + '.sql'))
}

function Remove-OpsFile {
    param([string] $Path)
    if ($Path) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}

function ConvertTo-OpsPsqlPath {
    param([Parameter(Mandatory = $true)] [string] $Path)
    $full = [System.IO.Path]::GetFullPath($Path)
    # psql handles forward slashes on Windows; the doubled quote is the psql
    # meta-command escape for a literal single quote inside a quoted file name.
    return $full.Replace('\', '/').Replace("'", "''")
}

# Resolves a user-supplied path against the PowerShell current location. .NET's
# process current directory is not updated by "cd", so a relative -OutputDir or
# -DumpFile must be resolved with the PowerShell provider path instead.
function Resolve-OpsPath {
    param([Parameter(Mandatory = $true)] [string] $Path)
    if ([System.IO.Path]::IsPathRooted($Path)) { return [System.IO.Path]::GetFullPath($Path) }
    return [System.IO.Path]::GetFullPath((Join-Path (Get-Location).ProviderPath $Path))
}

function Get-OpsSha256 {
    param([Parameter(Mandatory = $true)] [string] $Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Format-OpsByteSize {
    param([long] $Bytes)
    if ($Bytes -ge 1073741824) { return ('{0:N2} GiB' -f ($Bytes / 1073741824.0)) }
    if ($Bytes -ge 1048576) { return ('{0:N2} MiB' -f ($Bytes / 1048576.0)) }
    if ($Bytes -ge 1024) { return ('{0:N1} KiB' -f ($Bytes / 1024.0)) }
    return "$Bytes B"
}

function Get-OpsLocalTimestamp { return (Get-Date).ToString('yyyyMMdd-HHmmss') }
function Get-OpsUtcTimestamp { return (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ') }

# Runs one native tool with streamed stdout/stderr. The caller inspects
# $LASTEXITCODE afterwards; nothing is captured so large dumps and psql scripts
# keep flowing to the operator's terminal or redirected log.
function Invoke-OpsNative {
    param(
        [Parameter(Mandatory = $true)] [string] $ToolPath,
        [string[]] $Arguments = @()
    )
    & $ToolPath @Arguments
}

# Runs SQL through psql and returns its stdout lines (tuples only, unaligned).
# stderr is deliberately left on the host stream: with $ErrorActionPreference =
# 'Stop' merging native stderr in Windows PowerShell 5.1 can turn a harmless
# notice into a terminating error, so only the exit code decides success.
function Invoke-OpsPsqlQuery {
    param(
        [Parameter(Mandatory = $true)] [string] $PsqlPath,
        [Parameter(Mandatory = $true)] [string] $Database,
        [Parameter(Mandatory = $true)] [string] $Sql,
        [string] $DatabaseHost = '127.0.0.1',
        [int] $Port = 5433,
        [string] $UserName = 'postgres'
    )
    Assert-OpsConnectionSettings -DatabaseHost $DatabaseHost -Port $Port -UserName $UserName
    $sqlFile = New-OpsTempFile -Prefix 'aegisflow-ops-query'
    try {
        Write-OpsTextFile -Path $sqlFile -Text $Sql
        $arguments = @(
            '-X', '-w', '-q', '-A', '-t', '-F', '|',
            '-v', 'ON_ERROR_STOP=1',
            '-h', $DatabaseHost, '-p', [string]$Port, '-d', $Database,
            '-U', $UserName, '-f', $sqlFile
        )
        $lines = & $PsqlPath @arguments
        $exitCode = $LASTEXITCODE
        if ($exitCode -ne 0) {
            throw "psql exited with code $exitCode while querying database '$Database' on ${DatabaseHost}:$Port"
        }
        return $lines
    }
    finally {
        Remove-OpsFile -Path $sqlFile
    }
}

# Returns the canonical tenant-scoped table list in foreign-key-safe import
# order. ScopeSql contains a single {0} placeholder for the validated tenant id.
# project_members has no tenant_id column in schema v19 (the tenant is implied by
# projects), but it is exported/imported explicitly so project membership
# survives a tenant move; without it private projects would lose their
# maintainers/viewers.
function Get-OpsTenantTableDefinitions {
    return @(
        [pscustomobject]@{ Name = 'tenants'; ScopeSql = "SELECT * FROM tenants WHERE id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'auth_users'; ScopeSql = "SELECT * FROM auth_users WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'auth_audit'; ScopeSql = "SELECT * FROM auth_audit WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'projects'; ScopeSql = "SELECT * FROM projects WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'builds'; ScopeSql = "SELECT * FROM builds WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'build_log_events'; ScopeSql = "SELECT e.* FROM build_log_events e JOIN builds b ON b.id = e.build_id WHERE b.tenant_id = '{0}'"; OrderBy = 'e.id' },
        [pscustomobject]@{ Name = 'deployment_targets'; ScopeSql = "SELECT * FROM deployment_targets WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'reviews'; ScopeSql = "SELECT * FROM reviews WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'review_external_results'; ScopeSql = "SELECT * FROM review_external_results WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'review_notifications'; ScopeSql = "SELECT * FROM review_notifications WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'deployments'; ScopeSql = "SELECT * FROM deployments WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'deployment_events'; ScopeSql = "SELECT e.* FROM deployment_events e JOIN deployments d ON d.id = e.deployment_id WHERE d.tenant_id = '{0}'"; OrderBy = 'e.id' },
        [pscustomobject]@{ Name = 'approvals'; ScopeSql = "SELECT * FROM approvals WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'approval_notifications'; ScopeSql = "SELECT * FROM approval_notifications WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'scans'; ScopeSql = "SELECT * FROM scans WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'security_events'; ScopeSql = "SELECT * FROM security_events WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'knowledge_docs'; ScopeSql = "SELECT * FROM knowledge_docs WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'secrets'; ScopeSql = "SELECT * FROM secrets WHERE tenant_id = '{0}'"; OrderBy = 'id' },
        [pscustomobject]@{ Name = 'project_policies'; ScopeSql = "SELECT * FROM project_policies WHERE tenant_id = '{0}'"; OrderBy = 'project_id' },
        [pscustomobject]@{ Name = 'project_members'; ScopeSql = "SELECT m.* FROM project_members m JOIN projects p ON p.id = m.project_id WHERE p.tenant_id = '{0}'"; OrderBy = 'm.project_id, m.user_id' }
    )
}

function Format-OpsTenantScope {
    param(
        [Parameter(Mandatory = $true)] [string] $ScopeSql,
        [Parameter(Mandatory = $true)] [string] $TenantId
    )
    Assert-OpsTenantId -TenantId $TenantId
    return [string]::Format($ScopeSql, $TenantId)
}

function Get-OpsTenantTableDefinition {
    param([Parameter(Mandatory = $true)] [string] $TableName)
    foreach ($definition in Get-OpsTenantTableDefinitions) {
        if ($definition.Name -eq $TableName) { return $definition }
    }
    throw "table '$TableName' is not part of the tenant export set"
}

# One "SELECT count(*)" per tenant-scoped table, in import order.
function Get-OpsTenantCountSelects {
    param([Parameter(Mandatory = $true)] [string] $TenantId)
    $parts = @()
    foreach ($definition in Get-OpsTenantTableDefinitions) {
        $scope = Format-OpsTenantScope -ScopeSql $definition.ScopeSql -TenantId $TenantId
        $parts += ("SELECT 'ROWCOUNT|" + $definition.Name + "|' || count(*)::text FROM (" + $scope + ") AS aegis_scope")
    }
    return $parts
}

function Get-OpsTenantScopeCountSql {
    param(
        [Parameter(Mandatory = $true)] [string] $TableName,
        [Parameter(Mandatory = $true)] [string] $TenantId
    )
    $definition = Get-OpsTenantTableDefinition -TableName $TableName
    $scope = Format-OpsTenantScope -ScopeSql $definition.ScopeSql -TenantId $TenantId
    return "SELECT count(*) FROM (" + $scope + ") AS aegis_scope"
}

# Returns the bare tenant-scoped row set of one table (no aggregate wrapper), so
# callers can wrap it with their own count/aggregate SELECT.
function Get-OpsTenantScopeSql {
    param(
        [Parameter(Mandatory = $true)] [string] $TableName,
        [Parameter(Mandatory = $true)] [string] $TenantId
    )
    $definition = Get-OpsTenantTableDefinition -TableName $TableName
    return (Format-OpsTenantScope -ScopeSql $definition.ScopeSql -TenantId $TenantId)
}

# Delete statements that remove every tenant-owned row before a -Force import.
# Order is child-first so ON DELETE RESTRICT foreign keys never block the pass,
# and the final "DELETE FROM tenants" acts as a safety net: if any tenant-owned
# row was missed the whole import transaction fails instead of committing a
# partial replacement. -TenantId here is the id of the tenant row that already
# exists in the target database.
function Get-OpsTenantDeleteStatements {
    param([Parameter(Mandatory = $true)] [string] $TenantId)
    Assert-OpsTenantId -TenantId $TenantId
    $id = $TenantId
    return @(
        "DELETE FROM project_members m USING projects p WHERE m.project_id = p.id AND p.tenant_id = '$id'",
        "DELETE FROM build_log_events e USING builds b WHERE e.build_id = b.id AND b.tenant_id = '$id'",
        "DELETE FROM deployment_events e USING deployments d WHERE e.deployment_id = d.id AND d.tenant_id = '$id'",
        "DELETE FROM approval_notifications WHERE tenant_id = '$id'",
        "DELETE FROM approvals WHERE tenant_id = '$id'",
        "DELETE FROM deployments WHERE tenant_id = '$id'",
        "DELETE FROM review_external_results WHERE tenant_id = '$id'",
        "DELETE FROM review_notifications WHERE tenant_id = '$id'",
        "DELETE FROM reviews WHERE tenant_id = '$id'",
        "DELETE FROM scans WHERE tenant_id = '$id'",
        "DELETE FROM project_policies WHERE tenant_id = '$id'",
        "DELETE FROM builds WHERE tenant_id = '$id'",
        "DELETE FROM deployment_targets WHERE tenant_id = '$id'",
        "DELETE FROM secrets WHERE tenant_id = '$id'",
        "DELETE FROM knowledge_docs WHERE tenant_id = '$id'",
        "DELETE FROM security_events WHERE tenant_id = '$id'",
        "DELETE FROM auth_refresh_tokens r USING auth_sessions s WHERE r.session_id = s.id AND s.tenant_id = '$id'",
        "DELETE FROM auth_sessions WHERE tenant_id = '$id'",
        "DELETE FROM oidc_login_states WHERE tenant_id = '$id'",
        "DELETE FROM auth_audit WHERE tenant_id = '$id'",
        "DELETE FROM auth_users WHERE tenant_id = '$id'",
        "DELETE FROM projects WHERE tenant_id = '$id'",
        "DELETE FROM tenants WHERE id = '$id'"
    )
}

# Identity columns that must keep increasing after an import. A tenant export
# copies explicit build_log_events/deployment_events ids; the target sequence has
# to be moved past every existing id or the next insert would collide.
function Get-OpsIdentityTableNames {
    return @('build_log_events', 'deployment_events')
}

function Get-OpsSequenceRepairSql {
    return @'
DO $aegis_sequence_repair$
DECLARE
  table_name text;
  sequence_name text;
  max_id bigint;
  last_used bigint;
  was_called boolean;
  desired bigint;
BEGIN
  FOREACH table_name IN ARRAY ARRAY['build_log_events','deployment_events'] LOOP
    sequence_name := pg_get_serial_sequence('public.' || quote_ident(table_name), 'id');
    IF sequence_name IS NULL THEN
      CONTINUE;
    END IF;
    EXECUTE format('SELECT COALESCE(MAX(id), 0) FROM %I', table_name) INTO max_id;
    EXECUTE format('SELECT last_value, is_called FROM %s', sequence_name) INTO last_used, was_called;
    desired := GREATEST(max_id, CASE WHEN was_called THEN last_used ELSE last_used - 1 END);
    IF desired >= 1 THEN
      PERFORM setval(sequence_name, desired, true);
    END IF;
  END LOOP;
END
$aegis_sequence_repair$;
'@
}
