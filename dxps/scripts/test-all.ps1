# Runs the full DxPS test suite (unit + integration against the local Kafka/PostgreSQL) and publishes
# <runtime>\results\tests.json for the Mosaic Tests tile, plus the raw go test -json stream and coverage profile.
param([string]$Run = '', [int]$Parallel = 2)
. "$PSScriptRoot\env.ps1"
$ErrorActionPreference = 'Continue'

$out = Join-Path $RT 'results'
New-Item -ItemType Directory -Force $out | Out-Null
$raw = Join-Path $out 'tests-raw.jsonl'
$prof = Join-Path $out 'coverage.out'

Push-Location $SRC
try {
    $goArgs = @('test', '-json', '-count=1', "-p=$Parallel", '-timeout=20m', '-covermode=set',
        '-coverpkg=./internal/...', "-coverprofile=$prof")
    if ($Run) { $goArgs += "-run=$Run" }
    $goArgs += './...'
    $t0 = Get-Date
    & go @goArgs 2>$null | Set-Content -Encoding utf8 $raw
    $elapsed = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)
    & go run ./cmd/dxpsctl testreport -json $raw -cover $prof -out (Join-Path $out 'tests.json') -elapsed $elapsed
    if ($LASTEXITCODE -ne 0) { exit 1 }
} finally { Pop-Location }
