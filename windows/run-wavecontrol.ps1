[CmdletBinding()]
param(
    [string]$EnvironmentFile = '',
    [string]$ListenAddress = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

$Root = $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($EnvironmentFile)) {
    $EnvironmentFile = Join-Path $Root 'wavecontrol.env'
} elseif (-not [System.IO.Path]::IsPathRooted($EnvironmentFile)) {
    $EnvironmentFile = Join-Path $Root $EnvironmentFile
}

if (-not (Test-Path -LiteralPath $EnvironmentFile -PathType Leaf)) {
    $Template = Join-Path $Root 'wavecontrol.env.example'
    if (Test-Path -LiteralPath $Template -PathType Leaf) {
        Copy-Item -LiteralPath $Template -Destination $EnvironmentFile
        throw "Created $EnvironmentFile from the sample. Edit it before starting WaveControl."
    }
    throw "Environment file not found: $EnvironmentFile"
}

foreach ($RawLine in Get-Content -LiteralPath $EnvironmentFile) {
    $Line = $RawLine.Trim()
    if ($Line.Length -eq 0 -or $Line.StartsWith('#')) { continue }
    $Separator = $Line.IndexOf('=')
    if ($Separator -lt 1) { throw "Invalid environment line: $RawLine" }
    $Name = $Line.Substring(0, $Separator).Trim()
    $Value = $Line.Substring($Separator + 1).Trim()
    if ($Name -notmatch '^[A-Za-z_][A-Za-z0-9_]*$') { throw "Invalid environment variable name: $Name" }
    if ($Value.Length -ge 2) {
        $First = $Value[0]
        $Last = $Value[$Value.Length - 1]
        if (($First -eq "'" -and $Last -eq "'") -or ($First -eq '"' -and $Last -eq '"')) {
            $Value = $Value.Substring(1, $Value.Length - 2)
        }
    }
    [Environment]::SetEnvironmentVariable($Name, $Value, 'Process')
}

$Required = @('WAVECONTROL_DSN', 'WAVECONTROL_JWT_SECRET', 'WAVECONTROL_DATA_KEY')
foreach ($Name in $Required) {
    $Value = [Environment]::GetEnvironmentVariable($Name, 'Process')
    if ([string]::IsNullOrWhiteSpace($Value)) { throw "$Name is required in $EnvironmentFile" }
    if ($Value -match 'CHANGE_ME|REPLACE_WITH') { throw "$Name still contains a sample placeholder in $EnvironmentFile" }
}
if ($env:WAVECONTROL_JWT_SECRET.Length -lt 32) {
    throw 'WAVECONTROL_JWT_SECRET must be at least 32 characters.'
}

$DataKeyValid = $false
if ($env:WAVECONTROL_DATA_KEY -match '^[0-9A-Fa-f]{64}$') {
    $DataKeyValid = $true
} else {
    try {
        $DecodedDataKey = [Convert]::FromBase64String($env:WAVECONTROL_DATA_KEY)
        $DataKeyValid = ($DecodedDataKey.Length -eq 32)
    } catch {
        $DataKeyValid = $false
    }
}
if (-not $DataKeyValid) {
    throw 'WAVECONTROL_DATA_KEY must be exactly 32 bytes encoded as base64 or 64 hexadecimal characters.'
}

$Executable = Join-Path $Root 'wavecontrol.exe'
$WebRoot = Join-Path $Root 'web'
if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) { throw "Executable not found: $Executable" }
if (-not (Test-Path -LiteralPath (Join-Path $WebRoot 'index.html') -PathType Leaf)) { throw "Web assets not found under: $WebRoot" }

New-Item -ItemType Directory -Force -Path (Join-Path $Root 'firmware') | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $Root 'backups') | Out-Null

$Arguments = @('-d', '-workdir', $Root, '-webroot', $WebRoot)
if (-not [string]::IsNullOrWhiteSpace($ListenAddress)) {
    $Arguments += @('-addr', $ListenAddress)
}

Write-Host "Starting WaveControl from $Root"
& $Executable @Arguments
exit $LASTEXITCODE
