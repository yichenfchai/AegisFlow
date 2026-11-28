#Requires -Version 5.1
<#
.SYNOPSIS
Creates a compressed, verifiable PostgreSQL logical backup for AegisFlow.

.DESCRIPTION
Runs pg_dump in custom format (--no-owner --no-privileges so the archive can be
restored into a different role/database), writes a SHA-256 sidecar file and, by
default, verifies that the archive is readable with "pg_restore --list". The
verification prints the archived table count, TOC entry count, archive database
name, dump size and hash; a failure exits non-zero.

The password is never a parameter. libpq reads PGPASSWORD / PGPASSFILE / pgpass
or uses a peer/trust connection; pass a password with
    $env:PGPASSWORD = '...'
in the current PowerShell process only. The script never prints that variable.

pg_dump is resolved from -PgDumpPath, then $env:AEGIS_PG_DUMP_PATH, then
D:\AegisFlow-tools\pgsql\pgsql\bin\pg_dump.exe, then PATH. pg_restore (needed
only for the default verification pass) is resolved from -PgRestorePath, then
$env:AEGIS_PG_RESTORE_PATH, then next to the resolved pg_dump, then the same
local fallback directory, then PATH.

.PARAMETER Database
Source database name (required). Only the name, not a connection URL.

.PARAMETER OutputDir
Directory for the .dump and .dump.sha256 files. Defaults to the current
directory. The directory is created when missing.

.PARAMETER SkipVerify
Skip the "pg_restore --list" readability check. The SHA-256 sidecar is always
written; skipping only omits the archive read-back check.

.PARAMETER SslMode
Optional libpq sslmode (disable, allow, prefer, require, verify-ca,
verify-full). When omitted an inherited $env:PGSSLMODE is left untouched.

.EXAMPLE
$env:PGPASSWORD = Get-Content C:\secrets\pg.txt
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/backup-postgres.ps1 `
  -Database aegisflow -Host db.internal -Port 5432 -User aegis_backup -OutputDir D:\backups

.EXAMPLE
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ops/backup-postgres.ps1 `
  -Database aegis_ops_scratch -OutputDir D:\backups -SkipVerify
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $Database,

    [Alias('Host')] [string] $DatabaseHost = '127.0.0.1',

    [ValidateRange(1, 65535)] [int] $Port = 5433,

    [string] $User = 'postgres',

    [string] $OutputDir,

    [switch] $Verify,

    [switch] $SkipVerify,

    [string] $PgDumpPath,

    [string] $PgRestorePath,

    [string] $SslMode
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'ops-common.ps1')

function Invoke-BackupMain {
    if ($Verify -and $SkipVerify) {
        throw '-Verify and -SkipVerify are mutually exclusive: verification is on by default, use -SkipVerify to turn it off'
    }
    if (-not $OutputDir) { $OutputDir = (Get-Location).ProviderPath }
    Assert-OpsDatabaseName -Name $Database -ParameterName 'Database'
    Assert-OpsConnectionSettings -DatabaseHost $DatabaseHost -Port $Port -UserName $User

    Set-OpsPgEnvironment -SslMode $SslMode -AppName 'aegisflow-backup'

    Write-OpsStage 'backup target'
    Write-OpsLog "database=$Database host=${DatabaseHost}:$Port user=$User"
    $passwordState = if ($env:PGPASSWORD) { 'PGPASSWORD is set for this process (value never printed)' }
        elseif ($env:PGPASSFILE) { "PGPASSFILE is set ($($env:PGPASSFILE))" }
        else { 'no PGPASSWORD/PGPASSFILE in the environment; relying on pgpass/peer/trust' }
    Write-OpsLog $passwordState

    $resolvedOutputDir = Resolve-OpsPath -Path $OutputDir
    if (-not (Test-Path -LiteralPath $resolvedOutputDir -PathType Container)) {
        New-Item -ItemType Directory -Path $resolvedOutputDir -Force | Out-Null
        Write-OpsLog "created output directory $resolvedOutputDir"
    }

    $timestamp = Get-OpsLocalTimestamp
    $dumpFile = Join-Path $resolvedOutputDir ($Database + '-' + $timestamp + '.dump')
    if (Test-Path -LiteralPath $dumpFile) { throw "refusing to overwrite an existing backup: $dumpFile" }
    $hashFile = $dumpFile + '.sha256'

    $pgDump = Resolve-OpsTool -ToolName 'pg_dump' -ExplicitPath $PgDumpPath -EnvironmentVariable 'AEGIS_PG_DUMP_PATH'
    Write-OpsLog "pg_dump: $pgDump ($(Get-OpsToolVersion -ToolPath $pgDump))"
    $pgRestore = $null
    if (-not $SkipVerify) {
        $pgRestore = Resolve-OpsTool -ToolName 'pg_restore' -ExplicitPath $PgRestorePath -EnvironmentVariable 'AEGIS_PG_RESTORE_PATH' -SiblingOf $pgDump
        Write-OpsLog "pg_restore: $pgRestore ($(Get-OpsToolVersion -ToolPath $pgRestore))"
    }

    Write-OpsStage 'pg_dump'
    $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
    $arguments = @(
        '--format=custom',
        '--compress=9',
        '--no-owner',
        '--no-privileges',
        '--no-password',
        '-h', $DatabaseHost,
        '-p', [string]$Port,
        '-U', $User,
        '-d', $Database,
        ('--file=' + $dumpFile)
    )
    try {
        Invoke-OpsNative -ToolPath $pgDump -Arguments $arguments
        $dumpExit = $LASTEXITCODE
    }
    catch {
        Remove-OpsFile -Path $dumpFile
        throw "pg_dump could not be started: $($_.Exception.Message)"
    }
    $stopwatch.Stop()
    if ($dumpExit -ne 0) {
        Remove-OpsFile -Path $dumpFile
        throw "pg_dump exited with code $dumpExit; the partial dump was removed, no backup was produced"
    }
    if (-not (Test-Path -LiteralPath $dumpFile -PathType Leaf)) {
        throw "pg_dump reported success but '$dumpFile' does not exist"
    }

    $size = (Get-Item -LiteralPath $dumpFile).Length
    if ($size -le 0) {
        Remove-OpsFile -Path $dumpFile
        throw 'pg_dump produced an empty file; treating it as a failed backup'
    }
    $sha256 = Get-OpsSha256 -Path $dumpFile
    $sidecar = $sha256 + '  ' + (Split-Path -Leaf $dumpFile) + "`n"
    Write-OpsTextFile -Path $hashFile -Text $sidecar

    Write-OpsStage 'backup result'
    Write-OpsLog ("file:     " + $dumpFile)
    Write-OpsLog ("size:     " + (Format-OpsByteSize -Bytes $size))
    Write-OpsLog ("sha256:   " + $sha256)
    Write-OpsLog ("checksum: " + $hashFile)
    Write-OpsLog ("duration: " + $stopwatch.Elapsed.TotalSeconds.ToString('0.0') + "s")

    if ($SkipVerify) {
        Write-OpsWarn 'archive verification was skipped (-SkipVerify); run pg_restore --list on the dump before relying on it'
        Write-OpsLog 'backup: OK (unverified)'
        return
    }

    Write-OpsStage 'verify archive (pg_restore --list)'
    $listLines = & $pgRestore --list $dumpFile
    $listExit = $LASTEXITCODE
    if ($listExit -ne 0) {
        throw "pg_restore --list exited with code $listExit; '$dumpFile' is not a readable archive"
    }
    $archiveDatabase = $null
    $tocEntries = $null
    $dumpVersion = $null
    foreach ($line in $listLines) {
        if ($line -match '^\;\s+dbname:\s+(\S+)\s*$') { $archiveDatabase = $matches[1] }
        elseif ($line -match '^\;\s+TOC Entries:\s+(\d+)') { $tocEntries = [int]$matches[1] }
        elseif ($line -match '^\;\s+Dumped by pg_dump version:\s+(\S+)') { $dumpVersion = $matches[1] }
    }
    $tableLines = @($listLines | Where-Object { $_ -match '^[0-9]+; [0-9]+ [0-9]+ TABLE ' -and $_ -notmatch ' TABLE DATA ' })
    $dataLines = @($listLines | Where-Object { $_ -match ' TABLE DATA ' })
    $tableCount = $tableLines.Count
    $dataCount = $dataLines.Count

    if ($null -eq $archiveDatabase) {
        throw 'could not read the archive database name from pg_restore --list; the archive header is damaged'
    }
    if ($archiveDatabase -ne $Database) {
        throw "archive database '$archiveDatabase' does not match the requested database '$Database'"
    }
    if ($null -eq $tocEntries) {
        throw 'could not read the TOC entry count from pg_restore --list; the archive header is damaged'
    }
    if ($tableCount -le 0) {
        throw 'the archive lists no tables; refusing to report a healthy backup'
    }

    Write-OpsLog ("archive db:  " + $archiveDatabase)
    Write-OpsLog ("tables:      " + $tableCount + ' table definitions, ' + $dataCount + ' table-data sections')
    Write-OpsLog ("toc entries: " + $tocEntries)
    if ($dumpVersion) { Write-OpsLog ("dump version: " + $dumpVersion) }
    Write-OpsLog 'backup: OK'

    Write-OpsStage 'record this in the drill log'
    Write-OpsLog 'Store the dump and its .sha256 sidecar encrypted and off-host; the archive contains password hashes,'
    Write-OpsLog 'refresh-token hashes, audit rows and sealed credential ciphertext (secrets/deployment_targets).'
}

try {
    Invoke-BackupMain
}
catch {
    Write-OpsError ('backup failed: ' + $_.Exception.Message)
    exit 1
}
exit 0
