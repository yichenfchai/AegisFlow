#requires -Version 5.1
<#
.SYNOPSIS
Drops and recreates the disposable PostgreSQL database used by the local browser
(E2E) test run, so a repeated run starts from an empty schema.

.DESCRIPTION
Implements the "一次性 E2E 数据库" workflow described in AGENTS.md and
deploy/README.md. The CI e2e job uses a fresh PostgreSQL service container on
every run, so this script is only for local runs that reuse one database.

The URL is parsed for host, port, user and database name. The password, when
present, is handed to psql through PGPASSWORD and is never printed or placed on a
command line. Only the target database is dropped; the PostgreSQL server, its
other databases and the schema_migrations ledger bootstrapped by the server are
not touched beyond the drop/create.

Refusals (validation, fail closed):
  * a missing URL (pass -DatabaseUrl or set AEGIS_E2E_DATABASE_URL),
  * a scheme other than postgres:// or postgresql://,
  * a URL without host or database name,
  * a database name outside [A-Za-z0-9_.-],
  * the maintenance databases postgres, template0 and template1.

psql is resolved from -PsqlPath, then $env:AEGIS_PSQL_PATH, then PATH, then the
local tooling fallback D:\AegisFlow-tools\pgsql\pgsql\bin\psql.exe. The E2E suite
expects the target database to exist: Playwright starts the real Go backend with
AEGIS_E2E_DATABASE_URL and the server creates its tables.

.PARAMETER DatabaseUrl
Connection URL of the disposable E2E database, for example
postgres://test:test@localhost:5432/aegis_e2e?sslmode=disable.
Defaults to $env:AEGIS_E2E_DATABASE_URL. Never point this at production.

.PARAMETER PsqlPath
Explicit path to psql.exe; overrides the resolution order above.

.EXAMPLE
pwsh scripts/reset-e2e-db.ps1 -WhatIf

Prints the resolved target and the SQL that would run, without touching the
database.

.EXAMPLE
$env:AEGIS_E2E_DATABASE_URL = 'postgres://test:test@localhost:5432/aegis_e2e?sslmode=disable'
pwsh scripts/reset-e2e-db.ps1

Drops and recreates aegis_e2e and prints the target, the SQL and the result.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [string] $DatabaseUrl = $env:AEGIS_E2E_DATABASE_URL,

    [string] $PsqlPath
)

$ErrorActionPreference = 'Stop'

function Resolve-Psql {
    param([string] $Explicit)

    if ($Explicit) {
        if (-not (Test-Path -LiteralPath $Explicit -PathType Leaf)) {
            throw "psql not found at -PsqlPath: $Explicit"
        }
        return (Resolve-Path -LiteralPath $Explicit).ProviderPath
    }

    $fromEnv = $env:AEGIS_PSQL_PATH
    if ($fromEnv) {
        if (-not (Test-Path -LiteralPath $fromEnv -PathType Leaf)) {
            throw "psql not found at AEGIS_PSQL_PATH: $fromEnv"
        }
        return (Resolve-Path -LiteralPath $fromEnv).ProviderPath
    }

    $onPath = Get-Command psql -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($onPath) { return $onPath.Source }

    $fallback = 'D:\AegisFlow-tools\pgsql\pgsql\bin\psql.exe'
    if (Test-Path -LiteralPath $fallback -PathType Leaf) { return $fallback }

    throw 'psql not found; install the PostgreSQL client, set AEGIS_PSQL_PATH, or pass -PsqlPath <path to psql>'
}

# Parses postgres://[user[:password]@]host[:port]/database[?params] into the
# pieces psql needs. Malformed or dangerous targets throw instead of guessing.
function ConvertFrom-DatabaseUrl {
    param([string] $Url)

    if ([string]::IsNullOrWhiteSpace($Url)) {
        throw 'no database URL: pass -DatabaseUrl or set AEGIS_E2E_DATABASE_URL (it must point at a disposable E2E database)'
    }

    try {
        $uri = [Uri] $Url
    } catch {
        throw "invalid database URL (cannot parse): $Url"
    }
    if ($uri.Scheme -ne 'postgres' -and $uri.Scheme -ne 'postgresql') {
        throw "unsupported URL scheme '$($uri.Scheme)'; use postgres:// or postgresql://"
    }
    if (-not $uri.Host) {
        throw 'database URL has no host; expected postgres://user@host:port/database'
    }

    $database = [Uri]::UnescapeDataString($uri.AbsolutePath.TrimStart('/'))
    if (-not $database) {
        throw 'database URL has no database name; expected postgres://user@host:port/database'
    }
    if ($database -notmatch '^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$') {
        throw "refusing database name '$database'; use letters, digits, underscore, dot and hyphen, at most 63 characters"
    }
    if ($database -in @('postgres', 'template0', 'template1')) {
        throw "refusing to reset maintenance database '$database'; point the URL at the disposable E2E database"
    }

    $user = $null
    $password = $null
    if ($uri.UserInfo) {
        $userInfo = $uri.UserInfo.Split(':', 2)
        $user = [Uri]::UnescapeDataString($userInfo[0])
        if ($userInfo.Length -gt 1) {
            $password = [Uri]::UnescapeDataString($userInfo[1])
        }
    }

    $sslMode = $null
    foreach ($pair in $uri.Query.TrimStart('?').Split('&')) {
        if (-not $pair) { continue }
        $keyValue = $pair.Split('=', 2)
        if ($keyValue.Length -eq 2 -and $keyValue[0] -eq 'sslmode') {
            $sslMode = [Uri]::UnescapeDataString($keyValue[1])
        }
    }

    $port = if ($uri.Port -gt 0) { $uri.Port } else { 5432 }
    $displayName = "$($uri.Host):$port/$database"

    [pscustomobject]@{
        Host        = $uri.Host
        Port        = $port
        User        = $user
        Password    = $password
        SslMode     = $sslMode
        Database    = $database
        DisplayName = $displayName
    }
}

# Runs one SQL command against the maintenance database. -w keeps psql from
# prompting for a password interactively (it fails instead). The SQL travels in
# a temporary -f file instead of -c: Windows PowerShell 5.1 mangles embedded
# double quotes in native command arguments, and the quoted database identifier
# must survive verbatim. PGPASSWORD and PGSSLMODE only exist for this call.
function Invoke-Psql {
    param($Connection, [string] $Sql)

    $sqlFile = Join-Path ([System.IO.Path]::GetTempPath()) ('aegisflow-e2e-reset-' + [guid]::NewGuid().ToString('N') + '.sql')
    $previousPassword = $env:PGPASSWORD
    $previousSslMode = $env:PGSSLMODE
    try {
        [System.IO.File]::WriteAllText($sqlFile, $Sql, (New-Object System.Text.UTF8Encoding($false)))
        $arguments = @(
            '-X',
            '-w',
            '--no-psqlrc',
            '-v', 'ON_ERROR_STOP=1',
            '-h', $Connection.Host,
            '-p', "$($Connection.Port)",
            '-d', 'postgres',
            '-f', $sqlFile
        )
        if ($Connection.User) {
            $arguments += @('-U', $Connection.User)
        }

        if ($null -ne $Connection.Password) { $env:PGPASSWORD = $Connection.Password }
        if ($Connection.SslMode) { $env:PGSSLMODE = $Connection.SslMode }
        & $script:PsqlPath @arguments
        if ($LASTEXITCODE -ne 0) {
            throw "psql exited with code $LASTEXITCODE while running: $Sql"
        }
    } finally {
        Remove-Item -LiteralPath $sqlFile -Force -ErrorAction SilentlyContinue
        if ($null -eq $previousPassword) {
            Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
        } else {
            $env:PGPASSWORD = $previousPassword
        }
        if ($null -eq $previousSslMode) {
            Remove-Item Env:PGSSLMODE -ErrorAction SilentlyContinue
        } else {
            $env:PGSSLMODE = $previousSslMode
        }
    }
}

$connection = ConvertFrom-DatabaseUrl -Url $DatabaseUrl

# The allowlist above already rejects quotes and whitespace; doubling any quote
# is the second line of defense for the identifier that goes into the SQL.
$quotedDatabase = '"' + ($connection.Database -replace '"', '""') + '"'
$dropSql = "DROP DATABASE IF EXISTS $quotedDatabase WITH (FORCE)"
$createSql = "CREATE DATABASE $quotedDatabase"

# Validate and show the target before resolving psql: -WhatIf stays usable on a
# machine without a PostgreSQL client installed.
$identity = if ($connection.User) { " (user: $($connection.User))" } else { '' }
Write-Host "target: $($connection.DisplayName)$identity"
Write-Host "sql: $dropSql"
Write-Host "sql: $createSql"

$operation = 'Recreate the disposable E2E database (DROP DATABASE IF EXISTS ... WITH (FORCE); CREATE DATABASE ...)'
if (-not $PSCmdlet.ShouldProcess($connection.DisplayName, $operation)) {
    Write-Host 'reset: nothing was changed (-WhatIf); the SQL above would run without -WhatIf'
    exit 0
}

$script:PsqlPath = Resolve-Psql -Explicit $PsqlPath
Write-Host "psql: $script:PsqlPath"

# DROP/CREATE DATABASE cannot run inside a transaction; psql executes the -f
# file with ON_ERROR_STOP=1, so each statement is its own autocommit command.
# FORCE terminates leftover E2E connections.
Invoke-Psql -Connection $connection -Sql $dropSql
Invoke-Psql -Connection $connection -Sql $createSql

Write-Host "reset: ok: $($connection.DisplayName) was dropped and recreated; point AEGIS_E2E_DATABASE_URL at it and run npm run test:e2e"
