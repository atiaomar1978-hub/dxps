# Stop DxPS services, Kafka and PostgreSQL.
. "$PSScriptRoot\env.ps1"
$ErrorActionPreference = 'Continue'
Get-Process gateway, orchestrator, adapter, eventhub, netsim, dashboard -ErrorAction SilentlyContinue | Stop-Process -Force
Get-CimInstance Win32_Process -Filter "Name='java.exe'" | Where-Object { $_.CommandLine -match 'kafka\.Kafka' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force; Write-Host "kafka: stopped pid $($_.ProcessId)" }
if (Test-Path "$PGDATA\postmaster.pid") { & "$PGBIN\pg_ctl.exe" -D $PGDATA -m fast -w stop }
