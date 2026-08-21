[CmdletBinding()]
param(
    [ValidateSet('amd64', 'arm64')]
    [string]$Architecture = 'amd64',

    [string]$OutputDirectory = '',

    [switch]$SkipTests,

    [switch]$KeepExisting
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

$RepositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $RepositoryRoot ("dist\windows-{0}" -f $Architecture)
} elseif (-not [System.IO.Path]::IsPathRooted($OutputDirectory)) {
    $OutputDirectory = Join-Path $RepositoryRoot $OutputDirectory
}

$Go = Get-Command go -ErrorAction Stop
Write-Host ("Using {0}" -f (& $Go.Source version))

if ((Test-Path $OutputDirectory) -and -not $KeepExisting) {
    Remove-Item -Recurse -Force $OutputDirectory
}
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null

$OldGOOS = $env:GOOS
$OldGOARCH = $env:GOARCH
$OldCGO = $env:CGO_ENABLED

Push-Location $RepositoryRoot
try {
    if (-not $SkipTests) {
        Write-Host 'Running Go tests with the repository module graph...'
        Remove-Item Env:GOOS -ErrorAction SilentlyContinue
        Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
        $env:CGO_ENABLED = '0'
        & $Go.Source test ./...
        if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
    }

    $env:GOOS = 'windows'
    $env:GOARCH = $Architecture
    $env:CGO_ENABLED = '0'

    $Executable = Join-Path $OutputDirectory 'wavecontrol.exe'
    Write-Host ("Building {0}..." -f $Executable)
    & $Go.Source build -trimpath -o $Executable ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }

    Copy-Item -Recurse -Force (Join-Path $RepositoryRoot 'web') $OutputDirectory
    Copy-Item -Recurse -Force (Join-Path $RepositoryRoot 'migrations') $OutputDirectory
    Copy-Item -Force (Join-Path $RepositoryRoot 'schema.sql') $OutputDirectory
    Copy-Item -Force (Join-Path $RepositoryRoot 'README.md') $OutputDirectory
    Copy-Item -Force (Join-Path $RepositoryRoot 'docs\WINDOWS_HOST.md') $OutputDirectory
    Copy-Item -Force (Join-Path $RepositoryRoot 'docs\ALERTING.md') $OutputDirectory
    Copy-Item -Force (Join-Path $RepositoryRoot 'docs\SYSMON_WEB_ALERTER.md') $OutputDirectory
    Copy-Item -Force (Join-Path $PSScriptRoot 'run-wavecontrol.ps1') $OutputDirectory
    Copy-Item -Force (Join-Path $PSScriptRoot 'wavecontrol.env.example') $OutputDirectory

    New-Item -ItemType Directory -Force -Path (Join-Path $OutputDirectory 'firmware') | Out-Null
    New-Item -ItemType Directory -Force -Path (Join-Path $OutputDirectory 'backups') | Out-Null

    $Hash = (Get-FileHash -Algorithm SHA256 $Executable).Hash.ToLowerInvariant()
    Set-Content -Encoding ASCII -Path (Join-Path $OutputDirectory 'wavecontrol.exe.sha256') -Value ("{0}  wavecontrol.exe" -f $Hash)

    Write-Host ''
    Write-Host 'Windows package created:' -ForegroundColor Green
    Write-Host $OutputDirectory
    Write-Host 'Copy wavecontrol.env.example to wavecontrol.env, fill in the required values, then run:'
    Write-Host '.\run-wavecontrol.ps1'
}
finally {
    Pop-Location
    $env:GOOS = $OldGOOS
    $env:GOARCH = $OldGOARCH
    $env:CGO_ENABLED = $OldCGO
}
