# Show which DxPS components are listening.
. "$PSScriptRoot\env.ps1"
$c = [ordered]@{ postgres = 5433; kafka = 9092; netsim = 9199; adapter = 9202; orchestrator = 9201; eventhub = 9203; gateway = 8443; dashboard = 8088 }
foreach ($k in $c.Keys) { '{0,-13} {1,-5} port {2}' -f $k, $(if (Test-Port $c[$k]) { 'up' } else { 'down' }), $c[$k] }
