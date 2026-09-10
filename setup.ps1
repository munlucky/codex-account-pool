[CmdletBinding()]
param(
    [string[]]$Profiles,
    [switch]$SkipCodexConfig,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
if ([string]::IsNullOrWhiteSpace($env:APPDATA) -or [string]::IsNullOrWhiteSpace($env:USERPROFILE)) {
    throw 'GPT Codex Router setup currently requires Windows with APPDATA and USERPROFILE available.'
}
$stateRoot = Join-Path $env:APPDATA 'GPTCodexRouter'
$registryPath = Join-Path $stateRoot 'registry.json'
$codexConfigPath = Join-Path $env:USERPROFILE '.codex\config.toml'
$profilePattern = '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'

function Require-Command([string]$Name) {
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command '$Name' was not found in PATH."
    }
}

function Get-NextProfileName([System.Collections.IDictionary]$KnownProfiles) {
    $index = 1
    while ($KnownProfiles.Contains("account-$index")) {
        $index++
    }
    return "account-$index"
}

function Read-ExistingRegistry {
    $known = [ordered]@{}
    $active = $null

    if (Test-Path $registryPath) {
        try {
            $registry = Get-Content -Raw -LiteralPath $registryPath | ConvertFrom-Json
            foreach ($profile in @($registry.profiles)) {
                if ($profile.provider -eq 'codex') {
                    $known[$profile.id] = [ordered]@{
                        id        = [string]$profile.id
                        provider  = 'codex'
                        isolation = 'codex-home'
                    }
                }
            }
            if ($registry.active -and $registry.active.codex) {
                $active = [string]$registry.active.codex
            }
        }
        catch {
            throw "Could not read existing registry at $registryPath. Back it up or fix it before rerunning setup. $($_.Exception.Message)"
        }
    }

    return @($known, $active)
}

function Invoke-CodexChatGPTLogin([string]$ProfileId) {
    if ($ProfileId -notmatch $profilePattern) {
        throw "Invalid profile name '$ProfileId'. Use letters, numbers, dot, underscore, or dash (max 64 chars)."
    }

    $codexHome = Join-Path $stateRoot "profiles\codex\$ProfileId"
    New-Item -ItemType Directory -Force -Path $codexHome | Out-Null

    Write-Host ""
    Write-Host "=== ChatGPT login: $ProfileId ===" -ForegroundColor Cyan
    Write-Host "Complete the official Codex login flow in the browser, then return here."

    $saved = @{}
    foreach ($name in @('CODEX_HOME', 'OPENAI_API_KEY', 'CODEX_API_KEY', 'CODEX_ACCESS_TOKEN')) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }

    try {
        $env:CODEX_HOME = $codexHome
        Remove-Item Env:OPENAI_API_KEY -ErrorAction SilentlyContinue
        Remove-Item Env:CODEX_API_KEY -ErrorAction SilentlyContinue
        Remove-Item Env:CODEX_ACCESS_TOKEN -ErrorAction SilentlyContinue

        & codex -c 'cli_auth_credentials_store="file"' login
        if ($LASTEXITCODE -ne 0) {
            throw "Codex login failed for profile '$ProfileId' (exit code $LASTEXITCODE)."
        }
    }
    finally {
        foreach ($name in $saved.Keys) {
            $value = $saved[$name]
            if ($null -eq $value) {
                [Environment]::SetEnvironmentVariable($name, $null, 'Process')
            }
            else {
                [Environment]::SetEnvironmentVariable($name, $value, 'Process')
            }
        }
    }

    $authPath = Join-Path $codexHome 'auth.json'
    if (-not (Test-Path $authPath)) {
        throw "Codex reported success but $authPath was not created."
    }
}

function Write-Registry([System.Collections.IDictionary]$KnownProfiles, [string]$ActiveProfile) {
    New-Item -ItemType Directory -Force -Path $stateRoot | Out-Null

    $items = @()
    foreach ($entry in $KnownProfiles.GetEnumerator()) {
        $items += [ordered]@{
            id        = [string]$entry.Value.id
            provider  = 'codex'
            isolation = 'codex-home'
        }
    }

    $document = [ordered]@{
        version  = 1
        profiles = $items
        active   = [ordered]@{ codex = $ActiveProfile }
    }
    $json = $document | ConvertTo-Json -Depth 5
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($registryPath, $json + [Environment]::NewLine, $utf8NoBom)
}

function Set-CodexDesktopConfig {
    $configDir = Split-Path -Parent $codexConfigPath
    New-Item -ItemType Directory -Force -Path $configDir | Out-Null

    $content = ''
    if (Test-Path $codexConfigPath) {
        $content = [System.IO.File]::ReadAllText($codexConfigPath)
        $backup = "$codexConfigPath.gpt-codex-router.bak"
        if (-not (Test-Path $backup)) {
            Copy-Item -LiteralPath $codexConfigPath -Destination $backup
            Write-Host "Backed up original Codex config to $backup"
        }
    }

    $keptLines = New-Object System.Collections.Generic.List[string]
    $inRoot = $true
    foreach ($line in ($content -split "\r?\n")) {
        if ($line -match '^\s*\[') {
            $inRoot = $false
        }
        if ($inRoot -and $line -match '^\s*(chatgpt_base_url|openai_base_url)\s*=') {
            continue
        }
        $keptLines.Add($line)
    }
    $preservedContent = ($keptLines -join [Environment]::NewLine).TrimStart([char[]]"`r`n")

    $routerConfig = @(
        'chatgpt_base_url = "http://127.0.0.1:8317/backend-api"',
        'openai_base_url = "http://127.0.0.1:8317/backend-api/codex"',
        ''
    ) -join [Environment]::NewLine

    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($codexConfigPath, $routerConfig + $preservedContent, $utf8NoBom)
    Write-Host "Configured Codex Desktop routing in $codexConfigPath" -ForegroundColor Green
}


function Stop-ExistingRouter {
    Push-Location $repoRoot
    try {
        $previousPreference = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        & docker compose stop gpt-codex-router *> $null
        $ErrorActionPreference = $previousPreference
    }
    finally {
        Pop-Location
    }
}

function Start-Router {
    Push-Location $repoRoot
    try {
        & docker compose up -d --build
        if ($LASTEXITCODE -ne 0) {
            throw "docker compose up failed (exit code $LASTEXITCODE)."
        }

        $healthy = $false
        foreach ($attempt in 1..20) {
            try {
                $response = Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:8317/healthz' -TimeoutSec 2
                if ($response.StatusCode -eq 200 -and $response.Content.Trim() -eq 'ok') {
                    $healthy = $true
                    break
                }
            }
            catch {
                Start-Sleep -Milliseconds 500
            }
        }
        if (-not $healthy) {
            throw 'Container started, but the local health check did not become ready. Run: docker compose logs --tail=100 gpt-codex-router'
        }
    }
    finally {
        Pop-Location
    }
}

Require-Command 'codex'
Require-Command 'docker'
& docker compose version | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Docker Compose v2 is required. Start Docker Desktop and rerun setup.'
}
& docker info --format '{{.ServerVersion}}' | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Docker Desktop is not running or its engine is unavailable.'
}

Stop-ExistingRouter

$registryState = Read-ExistingRegistry
$knownProfiles = $registryState[0]
$activeProfile = $registryState[1]
$requestedProfiles = @()

if ($Profiles -and $Profiles.Count -gt 0) {
    $requestedProfiles = @($Profiles)
    foreach ($profileId in $requestedProfiles) {
        Invoke-CodexChatGPTLogin $profileId
        $knownProfiles[$profileId] = [ordered]@{
            id        = $profileId
            provider  = 'codex'
            isolation = 'codex-home'
        }
        if ([string]::IsNullOrWhiteSpace($activeProfile)) {
            $activeProfile = $profileId
        }
        Write-Registry $knownProfiles $activeProfile
    }
}
else {
    Write-Host 'GPT Codex Router setup' -ForegroundColor Cyan
    Write-Host 'Sign in to each ChatGPT account you want to route. Profile names are assigned automatically.'
    do {
        $profileId = Get-NextProfileName $knownProfiles
        Write-Host "Using local profile name: $profileId" -ForegroundColor DarkGray
        Invoke-CodexChatGPTLogin $profileId
        $knownProfiles[$profileId] = [ordered]@{
            id        = $profileId
            provider  = 'codex'
            isolation = 'codex-home'
        }
        if ([string]::IsNullOrWhiteSpace($activeProfile)) {
            $activeProfile = $profileId
        }
        Write-Registry $knownProfiles $activeProfile
        $another = Read-Host 'Sign in to another ChatGPT account? [y/N]'
    } while ($another -match '^(?i:y|yes)$')
}

if ($knownProfiles.Count -eq 0) {
    throw 'At least one ChatGPT/Codex profile is required.'
}
if ([string]::IsNullOrWhiteSpace($activeProfile) -or -not $knownProfiles.Contains($activeProfile)) {
    $activeProfile = [string]($knownProfiles.Keys | Select-Object -First 1)
}

Write-Registry $knownProfiles $activeProfile
Write-Host "Saved $($knownProfiles.Count) profile(s). Active profile: $activeProfile" -ForegroundColor Green

if (-not $NoStart) {
    Start-Router
    if (-not $SkipCodexConfig) {
        Set-CodexDesktopConfig
    }
    Write-Host ''
    Write-Host 'GPT Codex Router is running at http://127.0.0.1:8317' -ForegroundColor Green
    if (-not $SkipCodexConfig) {
        Write-Host 'Restart Codex Desktop completely before using it.' -ForegroundColor Yellow
    }
    Write-Host 'Logs: docker compose logs -f --tail=100 gpt-codex-router'
}
else {
    Write-Host 'Login setup complete. Start the router later with: docker compose up -d --build'
    if (-not $SkipCodexConfig) {
        Write-Host 'Codex Desktop config was not changed because -NoStart was used.' -ForegroundColor Yellow
    }
}
