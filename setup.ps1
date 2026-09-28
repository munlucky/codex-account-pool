[CmdletBinding()]
param(
    [switch]$SkipCodexConfig,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
if ([string]::IsNullOrWhiteSpace($env:APPDATA) -or [string]::IsNullOrWhiteSpace($env:USERPROFILE)) {
    throw 'GPT Codex Router setup currently requires Windows with APPDATA and USERPROFILE available.'
}

$composeEnvPath = Join-Path $repoRoot '.env'
$defaultStateRoot = Join-Path $env:APPDATA 'GPTCodexRouter'
$codexConfigPath = Join-Path $env:USERPROFILE '.codex\config.toml'

function Require-Command([string]$Name) {
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command '$Name' was not found in PATH."
    }
}

function Resolve-StateRoot {
    if (-not (Test-Path -LiteralPath $composeEnvPath -PathType Leaf)) {
        return $defaultStateRoot
    }

    $configured = $null
    foreach ($line in [System.IO.File]::ReadAllLines($composeEnvPath)) {
        if ($line -match '^\s*GPT_CODEX_ROUTER_STATE_ROOT\s*=\s*(.*?)\s*$') {
            $configured = $Matches[1].Trim()
        }
    }
    if ([string]::IsNullOrWhiteSpace($configured)) {
        return $defaultStateRoot
    }

    if (($configured.StartsWith("'") -and $configured.EndsWith("'")) -or
        ($configured.StartsWith('"') -and $configured.EndsWith('"'))) {
        $configured = $configured.Substring(1, $configured.Length - 2)
    }
    $configured = $configured.Replace("\'", "'")
    if ([string]::IsNullOrWhiteSpace($configured)) {
        throw "GPT_CODEX_ROUTER_STATE_ROOT in $composeEnvPath is empty."
    }
    if (-not [System.IO.Path]::IsPathRooted($configured)) {
        $configured = Join-Path $repoRoot $configured
    }
    return [System.IO.Path]::GetFullPath($configured)
}

$stateRoot = Resolve-StateRoot
$clientKeyPath = Join-Path $stateRoot 'client-key'

function Ensure-ClientApiKey {
    New-Item -ItemType Directory -Force -Path $stateRoot | Out-Null
    if (Test-Path -LiteralPath $clientKeyPath -PathType Leaf) {
        $existing = [System.IO.File]::ReadAllText($clientKeyPath).Trim()
        if ($existing.StartsWith('gcr_') -and $existing.Length -ge 32) {
            return $existing
        }
        throw "Existing local API key at $clientKeyPath is invalid. Remove it manually only if you intend to rotate the key."
    }

    $bytes = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $rng.GetBytes($bytes)
    }
    finally {
        $rng.Dispose()
    }
    $encoded = [Convert]::ToBase64String($bytes).TrimEnd([char]'=').Replace('+', '-').Replace('/', '_')
    $key = "gcr_$encoded"
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($clientKeyPath, $key + [Environment]::NewLine, $utf8NoBom)
    return $key
}

function Write-ComposeEnvironment {
    $composePath = ($stateRoot -replace '\\', '/').Replace("'", "\'")
    $lines = New-Object System.Collections.Generic.List[string]
    $lines.Add("GPT_CODEX_ROUTER_STATE_ROOT='$composePath'")

    if (Test-Path -LiteralPath $composeEnvPath -PathType Leaf) {
        foreach ($line in [System.IO.File]::ReadAllLines($composeEnvPath)) {
            if ($line -match '^\s*GPT_CODEX_ROUTER_STATE_ROOT\s*=') {
                continue
            }
            $lines.Add($line)
        }
    }

    while ($lines.Count -gt 1 -and [string]::IsNullOrWhiteSpace($lines[$lines.Count - 1])) {
        $lines.RemoveAt($lines.Count - 1)
    }

    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText(
        $composeEnvPath,
        ($lines -join [Environment]::NewLine) + [Environment]::NewLine,
        $utf8NoBom
    )
}

function Set-CodexDesktopConfig {
    $configDir = Split-Path -Parent $codexConfigPath
    New-Item -ItemType Directory -Force -Path $configDir | Out-Null

    $content = ''
    if (Test-Path -LiteralPath $codexConfigPath -PathType Leaf) {
        $content = [System.IO.File]::ReadAllText($codexConfigPath)
        $backup = "$codexConfigPath.gpt-codex-router.bak"
        if (-not (Test-Path -LiteralPath $backup)) {
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

function Stop-LegacyHostWorkerAt([string]$Root) {
    if ([string]::IsNullOrWhiteSpace($Root)) { return }
    $pidPath = Join-Path $Root 'host-worker.pid'
    if (-not (Test-Path -LiteralPath $pidPath -PathType Leaf)) { return }
    $raw = [System.IO.File]::ReadAllText($pidPath).Trim()
    $pidValue = 0
    if ([int]::TryParse($raw, [ref]$pidValue) -and $pidValue -gt 0) {
        $process = Get-Process -Id $pidValue -ErrorAction SilentlyContinue
        if ($process) {
            try {
                $path = [string]$process.Path
                if ($path -and $path.StartsWith((Join-Path $Root 'host-tools'), [System.StringComparison]::OrdinalIgnoreCase)) {
                    Stop-Process -Id $pidValue -Force -ErrorAction SilentlyContinue
                    Write-Host "Stopped obsolete host worker PID $pidValue" -ForegroundColor DarkGray
                }
            }
            catch {
                # Do not terminate a recycled PID if the executable cannot be verified.
            }
        }
    }
    Remove-Item -LiteralPath $pidPath -Force -ErrorAction SilentlyContinue
}

function Start-Router {
    Push-Location $repoRoot
    try {
        & docker compose up -d --build
        if ($LASTEXITCODE -ne 0) {
            throw "docker compose up failed (exit code $LASTEXITCODE)."
        }
        $healthy = $false
        foreach ($attempt in 1..30) {
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

Require-Command 'docker'
& docker compose version | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Docker Compose v2 is required. Start Docker Desktop and rerun setup.'
}
& docker info --format '{{.ServerVersion}}' | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Docker Desktop is not running or its engine is unavailable.'
}

New-Item -ItemType Directory -Force -Path $stateRoot | Out-Null
$null = Ensure-ClientApiKey
Write-ComposeEnvironment

# Migration cleanup only. New versions do not install or require a host worker.
Stop-LegacyHostWorkerAt $stateRoot
if ($defaultStateRoot -ne $stateRoot) {
    Stop-LegacyHostWorkerAt $defaultStateRoot
}

if ($NoStart) {
    Write-Host "Prepared router state at $stateRoot" -ForegroundColor Green
    Write-Host 'Start later with: docker compose up -d --build'
    return
}

Start-Router
if (-not $SkipCodexConfig) {
    Set-CodexDesktopConfig
}

Write-Host ''
Write-Host 'GPT Codex Router is running at http://127.0.0.1:8317' -ForegroundColor Green
Write-Host "State root: $stateRoot" -ForegroundColor DarkGray
Write-Host 'Account management UI: http://127.0.0.1:8317/admin' -ForegroundColor Green
Write-Host 'Administrator key: docker compose exec -T gpt-codex-router gpt-codex-router admin-key' -ForegroundColor DarkGray
Write-Host 'Add or re-authenticate Codex accounts from /admin using Docker-managed device-code login.' -ForegroundColor Cyan
Write-Host 'OpenAI-compatible base URL: http://127.0.0.1:8317/v1' -ForegroundColor Green
if (-not $SkipCodexConfig) {
    Write-Host 'Restart Codex Desktop completely before using it.' -ForegroundColor Yellow
}
Write-Host 'Logs: docker compose logs -f --tail=100 gpt-codex-router'
