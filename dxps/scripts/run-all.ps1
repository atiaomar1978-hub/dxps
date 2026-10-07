# Build and start the whole DxPS stack: PostgreSQL, Kafka, simulators, adapters, orchestrator,
# event hub, gateway and the Mosaic dashboard (http://127.0.0.1:8088).
param([switch]$NoBuild)
. "$PSScriptRoot\env.ps1"

Start-Pg
Start-Kafka

$services = 'netsim', 'adapter', 'orchestrator', 'eventhub', 'gateway', 'dashboard'
if (-not $NoBuild) {
    Push-Location $SRC
    try {
        foreach ($s in $services + 'dxpsctl') {
            go build -trimpath -o (Join-Path $BIN "$s.exe") "./cmd/$s"
            if ($LASTEXITCODE -ne 0) { throw "build $s failed" }
        }
    } finally { Pop-Location }
    Write-Host "built: $($services -join ', ')"
}

& (Join-Path $BIN 'dxpsctl.exe') migrate | Out-Null
& (Join-Path $BIN 'dxpsctl.exe') topics | Out-Null
& (Join-Path $BIN 'dxpsctl.exe') seed | Out-Null

$logDir = Join-Path $LOGS 'svc'
New-Item -ItemType Directory -Force $logDir | Out-Null
$env:DXPS_LOG_DIR = $logDir
Get-Process $services -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 500

$ports = @{ netsim = 9199; adapter = 9202; orchestrator = 9201; eventhub = 9203; gateway = 8443; dashboard = 8088 }
foreach ($s in $services) {
    Start-Process -FilePath (Join-Path $BIN "$s.exe") -WorkingDirectory $RT -WindowStyle Hidden
    Wait-Port $ports[$s] 60 | Out-Null
    Write-Host ("{0,-13} up on port {1}" -f $s, $ports[$s])
}
Write-Host ''
Write-Host 'DxPS Mosaic:  http://127.0.0.1:8088'
Write-Host 'TMF641 API:   https://localhost:8443/tmf-api/serviceOrdering/v5/serviceOrder'
Write-Host "Logs:         $logDir"
