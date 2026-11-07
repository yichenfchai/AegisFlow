#requires -Version 5.1
<#
.SYNOPSIS
Validates a PR body/comment file as strict UTF-8, and optionally publishes it
the same way `--body-file` does.

.DESCRIPTION
Implements the "PR 正文与评论编码" rule in AGENTS.md. The text must be UTF-8
without a BOM and must not carry the traces of the two accidents that produced
garbled PR descriptions in this repository:

  * a lossy conversion to an ASCII/ANSI code page, which turns every Chinese
    character into '?',
  * an escape interpreter (`echo -e`, `printf %b`, a shell string literal)
    turning `\a`, `\b`, `\f`, `\r` into control characters, which silently
    eats the first letter of words such as `backend` or `npm`.

Those are exactly the checks CI runs on `pull_request` events, so a body that
passes here passes CI.

With -Publish the file is sent with the equivalent of `--body-file`: `gh` is
preferred; otherwise the file content is sent as UTF-8 over the REST API.
After publishing, the stored body is read back and compared with the file, so a
transport that re-encodes the text fails loudly instead of publishing mojibake.

.PARAMETER Path
Markdown file that is (or will be) the PR body or comment.

.PARAMETER Publish
Publish Path with -PullRequest or -Create. Requires `gh` or GH_TOKEN/GITHUB_TOKEN.

.PARAMETER PullRequest
PR number whose body is replaced.

.PARAMETER Create
Create a new PR; requires -Head, -Title and a token or gh.

.PARAMETER Repository
owner/name. Defaults to GITHUB_REPOSITORY, then the origin remote.

.PARAMETER AllowQuestionRun
Skip the '??' heuristic. Use only for a body that legitimately quotes code with
the TypeScript nullish operator; fenced code blocks are already ignored.

.EXAMPLE
pwsh scripts/pr-text.ps1 docs/pr-body.md

.EXAMPLE
pwsh scripts/pr-text.ps1 docs/pr-body.md -Publish -PullRequest 44
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory, Position = 0)]
    [string] $Path,

    [switch] $Publish,

    [int] $PullRequest,

    [switch] $Create,

    [string] $Base = 'main',

    [string] $Head,

    [string] $Title,

    [string] $Repository,

    [switch] $AllowQuestionRun
)

$ErrorActionPreference = 'Stop'

# Windows terminals default to the OEM code page (936 here), which is what
# turned the PR text into '?'. Set the console to UTF-8 before doing anything.
function Initialize-Utf8Console {
    try { chcp 65001 > $null } catch { }
    try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
    try { $script:OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
}

Initialize-Utf8Console

# A BOM or an invalid byte sequence means the file left the editor in the wrong
# encoding, so both are hard errors rather than something to repair silently.
function Read-PrTextFile {
    param([string] $FilePath)

    if (-not (Test-Path -LiteralPath $FilePath -PathType Leaf)) {
        throw "file not found: $FilePath"
    }
    $bytes = [System.IO.File]::ReadAllBytes($FilePath)
    if ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF) {
        throw "${FilePath}: starts with a UTF-8 BOM; save the file as UTF-8 without BOM"
    }
    $utf8 = New-Object System.Text.UTF8Encoding($false, $true)
    try {
        return $utf8.GetString($bytes)
    } catch {
        throw "${FilePath}: not valid UTF-8 (invalid byte sequence); save the file as UTF-8"
    }
}

# Returns errors and warnings with 1-based line numbers. Fenced code blocks are
# skipped for the '??' and backslash heuristics: `a ?? b` and escaped quotes in
# a snippet are legitimate, while prose is where the corruption shows up.
function Test-PrText {
    param([string] $Text, [switch] $SkipQuestionRun)

    $errors = New-Object System.Collections.Generic.List[string]
    $warnings = New-Object System.Collections.Generic.List[string]

    if ($Text.IndexOf([char] 0xFEFF) -ge 0) {
        $errors.Add('U+FEFF (BOM) inside the text; save the file as UTF-8 without BOM')
    }
    if ($Text.IndexOf([char] 0xFFFD) -ge 0) {
        $errors.Add('U+FFFD replacement character; that is the trace of a lossy encoding conversion')
    }

    $control = [regex] '[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]'
    $lines = ($Text -replace "`r`n", "`n") -split "`n"
    $inFence = $false
    for ($index = 0; $index -lt $lines.Length; $index++) {
        $line = $lines[$index]
        $lineNumber = $index + 1
        if ($line -match '^\s*(```|~~~)') {
            $inFence = -not $inFence
        }

        foreach ($match in $control.Matches($line)) {
            $code = [int] [char] $match.Value
            $errors.Add(('line {0}: control character 0x{1:X2}; a shell probably interpreted a backslash escape (\a \b \f \r)' -f $lineNumber, $code))
        }
        if (-not $SkipQuestionRun -and -not $inFence) {
            # Inline code is skipped too: `a ?? b` is a legitimate TypeScript
            # operator, not the trace of a broken conversion.
            $prose = [regex]::Replace($line, '`[^`]*`', '')
            $run = [regex]::Match($prose, '\?{2,}')
            if ($run.Success) {
                $errors.Add(('line {0}: run of {1} "?" characters; Chinese text was replaced during an ASCII conversion' -f $lineNumber, $run.Length))
            }
        }
        if (-not $inFence -and [regex]::Replace($line, '`[^`]*`', '').Contains('\')) {
            $warnings.Add(('line {0}: backslash found; write paths with "/" in PR text' -f $lineNumber))
        }
    }

    [pscustomobject]@{
        Text     = $Text
        Errors   = $errors
        Warnings = $warnings
    }
}

function Write-PrTextReport {
    param($Result, [string] $FilePath)

    foreach ($warning in $Result.Warnings) {
        Write-Host "warn: ${FilePath}: $warning" -ForegroundColor Yellow
    }
    foreach ($errorText in $Result.Errors) {
        Write-Host "error: ${FilePath}: $errorText" -ForegroundColor Red
    }
    if ($Result.Errors.Count -gt 0) {
        Write-Host ''
        Write-Host 'Fix the file and send it again with --body-file (see AGENTS.md, PR text encoding rule).'
        return 1
    }
    Write-Host "ok: ${FilePath}: valid UTF-8, no control characters, no '??' runs ($($Result.Text.Length) characters)"
    return 0
}

function Get-GitHubToken {
    if ($env:GH_TOKEN) { return $env:GH_TOKEN }
    if ($env:GITHUB_TOKEN) { return $env:GITHUB_TOKEN }
    return $null
}

function Get-RepositorySlug {
    param([string] $Explicit)

    if ($Explicit) { return $Explicit }
    if ($env:GITHUB_REPOSITORY) { return $env:GITHUB_REPOSITORY }
    $url = git remote get-url origin 2>$null
    if ($url -and $url -match 'github\.com[:/](?<slug>[^/]+/[^/.]+?)(\.git)?$') {
        return $Matches['slug']
    }
    throw 'cannot determine the repository; pass -Repository owner/name'
}

# JSON is only the transport envelope: the payload is always re-encoded to UTF-8
# bytes so the body cannot depend on the console code page.
function Invoke-GitHubApi {
    param(
        [string] $Method,
        [string] $Uri,
        $Payload,
        [string] $Token
    )

    $headers = @{
        Authorization = "Bearer $Token"
        'User-Agent'  = 'aegisflow-pr-text'
        Accept        = 'application/vnd.github+json'
    }
    if ($null -eq $Payload) {
        return Invoke-RestMethod -Method $Method -Uri $Uri -Headers $headers
    }
    $json = $Payload | ConvertTo-Json -Depth 5 -Compress
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    return Invoke-RestMethod -Method $Method -Uri $Uri -Headers $headers -Body $bytes -ContentType 'application/json; charset=utf-8'
}

$text = Read-PrTextFile -FilePath $Path
$result = Test-PrText -Text $text -SkipQuestionRun:$AllowQuestionRun
$status = Write-PrTextReport -Result $result -FilePath $Path
if ($status -ne 0) { exit 1 }

if (-not $Publish) { exit 0 }

$token = Get-GitHubToken
$slug = Get-RepositorySlug -Explicit $Repository
$gh = Get-Command gh -ErrorAction SilentlyContinue

if ($Create) {
    if (-not $Head -or -not $Title) { throw '-Create needs -Head and -Title' }
    if ($gh) {
        & gh pr create --repo $slug --base $Base --head $Head --title $Title --body-file $Path
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    } else {
        if (-not $token) { throw 'gh is not installed and no GH_TOKEN/GITHUB_TOKEN is set' }
        $created = Invoke-GitHubApi -Method Post -Uri "https://api.github.com/repos/$slug/pulls" -Token $token -Payload @{
            title = $Title; head = $Head; base = $Base; body = $text
        }
        Write-Host "created PR #$($created.number): $($created.html_url)"
    }
    exit 0
}

if ($PullRequest -le 0) { throw '-Publish needs -PullRequest <number> or -Create' }
if ($gh) {
    & gh pr edit $PullRequest --repo $slug --body-file $Path
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} else {
    if (-not $token) { throw 'gh is not installed and no GH_TOKEN/GITHUB_TOKEN is set' }
    Invoke-GitHubApi -Method Patch -Uri "https://api.github.com/repos/$slug/pulls/$PullRequest" -Token $token -Payload @{ body = $text } | Out-Null
}

# Round-trip check: whatever GitHub stored must match the file. This catches a
# transport that re-encodes the body even though the local file was healthy.
if ($token) {
    $stored = (Invoke-GitHubApi -Method Get -Uri "https://api.github.com/repos/$slug/pulls/$PullRequest" -Token $token).body
    $expected = ($text -replace "`r`n", "`n")
    $actual = ($stored -replace "`r`n", "`n")
    if ($actual -ne $expected) {
        throw 'GitHub stored a body that differs from the file (was it re-encoded?); publish again with a UTF-8 transport'
    }
    Write-Host "verified: PR #$PullRequest body matches $Path after line-ending normalization"
}
