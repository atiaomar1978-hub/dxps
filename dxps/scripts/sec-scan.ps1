# Static security scans: govulncheck (known CVEs reachable from DxPS code) and gosec (insecure patterns).
# Reports land in <runtime>\results. G404 is excluded: math/rand is used only for retry jitter, simulator latency
# and demo traffic mix; every secret (tokens, CSRF nonces, certificate serials, keys) uses crypto/rand.
. "$PSScriptRoot\env.ps1"
$ErrorActionPreference = 'Continue'
$out = Join-Path $RT 'results'
New-Item -ItemType Directory -Force $out | Out-Null
$gobin = Join-Path (go env GOPATH) 'bin'
foreach ($tool in 'govulncheck', 'gosec') {
    if (-not (Test-Path "$gobin\$tool.exe")) { throw "$tool not installed (go install golang.org/x/vuln/cmd/govulncheck@latest / github.com/securego/gosec/v2/cmd/gosec@latest)" }
}
Push-Location $SRC
try {
    & "$gobin\govulncheck.exe" -show verbose ./... *> (Join-Path $out 'govulncheck.txt')
    $vulnExit = $LASTEXITCODE
    & "$gobin\gosec.exe" -quiet -fmt json -out (Join-Path $out 'gosec.json') -exclude=G404 ./... *> $null
} finally { Pop-Location }

$g = Get-Content -Raw (Join-Path $out 'gosec.json') | ConvertFrom-Json
$issues = @($g.Issues)
$bySev = $issues | Group-Object severity | ForEach-Object { "$($_.Name)=$($_.Count)" }
$res = [ordered]@{
    generatedAt = (Get-Date).ToUniversalTime().ToString('o')
    govulncheck = if ($vulnExit -eq 0) { 'No vulnerabilities found' } else { "vulnerabilities reported (exit $vulnExit)" }
    gosec       = [ordered]@{ files = $g.Stats.files; lines = $g.Stats.lines; nosec = $g.Stats.nosec; found = $issues.Count
        high = @($issues | Where-Object severity -eq 'HIGH').Count; medium = @($issues | Where-Object severity -eq 'MEDIUM').Count
        low = @($issues | Where-Object severity -eq 'LOW').Count
        issues = @($issues | ForEach-Object { [ordered]@{ rule = $_.rule_id; severity = $_.severity; file = ($_.file -replace '.*\\dxps\\', ''); line = $_.line; details = $_.details } }) }
}
$res | ConvertTo-Json -Depth 5 | Set-Content -Encoding utf8 (Join-Path $out 'security-scan.json')
"govulncheck: $($res.govulncheck)"
"gosec: $($g.Stats.files) files, $($g.Stats.lines) lines, $($issues.Count) findings ($($bySev -join ', ')), $($g.Stats.nosec) justified #nosec"
$issues | Where-Object severity -ne 'LOW' | ForEach-Object { '  {0} {1} {2}:{3} {4}' -f $_.rule_id, $_.severity, ($_.file -replace '.*\\dxps\\', ''), $_.line, $_.details }
if ($vulnExit -ne 0 -or @($issues | Where-Object severity -ne 'LOW').Count -gt 0) { exit 1 }
