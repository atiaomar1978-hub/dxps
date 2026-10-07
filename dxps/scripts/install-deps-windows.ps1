# Installs the DxPS prerequisites on Windows 10/11 (x64):
#   Go and Eclipse Temurin JRE 21 via winget (if missing), and into the DxPS runtime directory:
#   PostgreSQL 18 binaries (EDB zip) and Apache Kafka 4 (Apache release, SHA-512 verified). Re-running is safe.
param([string]$KafkaVersion = '4.3.1', [string]$PgVersion = '18.6-1')
. "$PSScriptRoot\env.ps1"
$dl = Join-Path $RT 'dl'
New-Item -ItemType Directory -Force $RT, $dl | Out-Null

function Have($cmd) { [bool](Get-Command $cmd -ErrorAction SilentlyContinue) }

if (-not (Have 'go')) {
    Write-Host '== Go (winget GoLang.Go)'
    winget install --id GoLang.Go -e --accept-source-agreements --accept-package-agreements
    Write-Host 'Open a new terminal afterwards so go is on PATH.'
}
if (-not $jdk -and -not (Have 'java')) {
    Write-Host '== Java 21 (winget EclipseAdoptium.Temurin.21.JRE)'
    winget install --id EclipseAdoptium.Temurin.21.JRE -e --accept-source-agreements --accept-package-agreements
}

if (-not (Test-Path "$PGBIN\initdb.exe")) {
    Write-Host "== PostgreSQL $PgVersion binaries"
    $zip = Join-Path $dl 'pg.zip'
    curl.exe -fSL -o $zip "https://get.enterprisedb.com/postgresql/postgresql-$PgVersion-windows-x64-binaries.zip"
    if ($LASTEXITCODE) { throw 'PostgreSQL download failed' }
    Expand-Archive -Path $zip -DestinationPath $RT -Force   # creates <runtime>\pgsql
}
& "$PGBIN\postgres.exe" --version

if (-not (Test-Path "$KAFKA\bin\windows\kafka-server-start.bat")) {
    Write-Host "== Apache Kafka $KafkaVersion"
    $name = "kafka_2.13-$KafkaVersion"
    $tgz = Join-Path $dl "$name.tgz"
    $url = $null
    foreach ($base in 'https://downloads.apache.org/kafka', 'https://archive.apache.org/dist/kafka') {
        curl.exe -fsSL -o "$tgz.sha512" "$base/$KafkaVersion/$name.tgz.sha512"
        if ($LASTEXITCODE -eq 0) { $url = "$base/$KafkaVersion/$name.tgz"; break }
    }
    if (-not $url) { throw "Kafka $KafkaVersion not found on the Apache mirrors" }
    curl.exe -fSL -o $tgz $url
    $want = ((Get-Content -Raw "$tgz.sha512") -replace '^[^:]*:', '' -replace '\s', '').ToLower()
    $got = (Get-FileHash -Algorithm SHA512 $tgz).Hash.ToLower()
    if ($want -ne $got) { throw 'Kafka download checksum mismatch' }
    tar.exe -xzf $tgz -C $RT
    Move-Item (Join-Path $RT $name) $KAFKA
    Write-Host "kafka: installed $KafkaVersion (sha512 verified)"
}
Write-Host 'prerequisites ready - next: scripts\infra-setup.ps1'
