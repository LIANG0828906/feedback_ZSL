$ErrorActionPreference = "Stop"
$env:CGO_ENABLED = "0"

go build -o bin/maskpipe.exe ./cmd/maskpipe
./bin/maskpipe.exe generate -root samples/local-seed -seed 424242
./bin/maskpipe.exe validate -sample samples/local-seed -scene blocks
./bin/maskpipe.exe run -scene blocks -params configs/params.example.json -db reports/example-blocks.db -out reports/example-blocks.json -mode full -preview
./bin/maskpipe.exe threeway -scene blocks -out reports/threeway-blocks
./bin/maskpipe.exe verify -db reports/threeway-blocks/dbs/full.db -manifest samples/local-seed/manifest.json -scene blocks -out reports/threeway-blocks/verify-cli.json
Write-Host "Demo passed. Open http://127.0.0.1:8088 after running scripts/serve-demo.ps1"
