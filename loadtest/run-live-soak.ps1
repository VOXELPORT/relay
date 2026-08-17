param(
    [int]$Players = 150,
    [string]$Duration = "3h",
    [int]$PayloadBytes = 1024,
    [string]$Interval = "1s",
    [string]$ConnectRamp = "2m",
    [string]$ProgressEvery = "30s",
    [string]$WebSocketUrl = "wss://play.voxelport.in/",
    [string]$PlayerHost = "play.voxelport.in"
)

$ErrorActionPreference = "Stop"

if (-not $env:LIVE_RELAY_TOKEN) {
    throw "Set LIVE_RELAY_TOKEN to a temporary relay token before running this script."
}

$stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$log = Join-Path $PSScriptRoot "live-soak-$stamp.log"

& "$PSScriptRoot\live-soak.exe" `
    -ws $WebSocketUrl `
    -player-host $PlayerHost `
    -players $Players `
    -duration $Duration `
    -payload-bytes $PayloadBytes `
    -interval $Interval `
    -connect-ramp $ConnectRamp `
    -progress-every $ProgressEvery 2>&1 |
    Tee-Object -FilePath $log

Write-Host "Log saved to $log"
