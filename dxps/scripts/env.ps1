# Shared settings for the DxPS local runtime scripts (dot-source this file).
$ErrorActionPreference = 'Stop'
$script:RT      = if ($env:DXPS_RUNTIME) { $env:DXPS_RUNTIME } else { Join-Path $env:USERPROFILE 'dxps-runtime' }
$script:SRC     = Split-Path -Parent $PSScriptRoot
$script:BIN     = Join-Path $RT 'bin'
$script:PGBIN   = Join-Path $RT 'pgsql\bin'
$script:PGDATA  = Join-Path $RT 'data\pg'
$script:KAFKA   = Join-Path $RT 'kafka'
$script:KCONF   = Join-Path $RT 'kafka-config\server.properties'
$script:KDATA   = Join-Path $RT 'data\kafka'
$script:LOGS    = Join-Path $RT 'logs'
$env:DXPS_RUNTIME = $RT

$jdk = Get-ChildItem 'C:\Program Files\Eclipse Adoptium' -Directory -Filter 'jdk-2*' -ErrorAction SilentlyContinue | Sort-Object Name -Descending | Select-Object -First 1
if ($jdk) { $env:JAVA_HOME = $jdk.FullName; $env:Path = "$($jdk.FullName)\bin;$env:Path" }

function Read-Secrets {
    $h = @{}
    Get-Content (Join-Path $RT 'secrets.env') | Where-Object { $_ -match '^[A-Z_]+=' } | ForEach-Object {
        $k, $v = $_ -split '=', 2; $h[$k] = $v
    }
    return $h
}

function Test-Port([int]$port) {
    try { $c = [Net.Sockets.TcpClient]::new(); $c.Connect('127.0.0.1', $port); $c.Close(); return $true } catch { return $false }
}

function Wait-Port([int]$port, [int]$seconds = 90) {
    $deadline = (Get-Date).AddSeconds($seconds)
    while ((Get-Date) -lt $deadline) { if (Test-Port $port) { return $true }; Start-Sleep -Milliseconds 500 }
    throw "port $port did not open within $seconds s"
}

function Invoke-Psql([string]$db, [string]$sql) {
    $s = Read-Secrets
    $env:PGPASSWORD = $s['PG_SUPER_PASSWORD']; $env:PGSSLMODE = 'verify-full'; $env:PGSSLROOTCERT = (Join-Path $RT 'pki\ca.crt')
    try { $sql | & "$PGBIN\psql.exe" -X -q -v ON_ERROR_STOP=1 -h localhost -p 5433 -U postgres -d $db -f - }
    finally { Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue }
    if ($LASTEXITCODE -ne 0) { throw "psql failed ($LASTEXITCODE)" }
}

function Start-Kafka {
    if (Test-Port 9092) { Write-Host 'kafka: already running'; return }
    New-Item -ItemType Directory -Force $LOGS | Out-Null
    $env:KAFKA_HEAP_OPTS = '-Xms512m -Xmx1g'
    $env:LOG_DIR = Join-Path $LOGS 'kafka'
    # No stdout redirection: a redirected child inherits the caller's pipe handles and keeps it open.
    Start-Process -FilePath "$KAFKA\bin\windows\kafka-server-start.bat" -ArgumentList "`"$KCONF`"" -WindowStyle Hidden
    Wait-Port 9092 120 | Out-Null
    Write-Host 'kafka: started on 127.0.0.1:9092'
}

function Start-Pg {
    if (Test-Port 5433) { Write-Host 'postgres: already running'; return }
    New-Item -ItemType Directory -Force $LOGS | Out-Null
    & "$PGBIN\pg_ctl.exe" -D $PGDATA -l (Join-Path $LOGS 'postgres.log') -w start | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'pg_ctl start failed' }
    Write-Host 'postgres: started on localhost:5433 (TLS)'
}
