# Start PostgreSQL and Kafka (after infra-setup.ps1 has run once).
. "$PSScriptRoot\env.ps1"
Start-Pg
Start-Kafka
