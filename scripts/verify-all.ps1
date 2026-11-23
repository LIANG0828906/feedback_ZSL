$ErrorActionPreference = "Stop"

function Assert-GoExit {
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }
}

go version
Assert-GoExit
go test ./...
Assert-GoExit
go vet ./...
Assert-GoExit

go run .\cmd\gaitretarget generate --seed 42 --out .\data
Assert-GoExit
go run .\cmd\gaitretarget validate --data .\data
Assert-GoExit
go run .\cmd\gaitretarget run --mode full --data .\data --source src-walk-001 --target target-compact-01 --out .\artifacts\full-result.json --mapping-out .\artifacts\mapping.json
Assert-GoExit
go run .\cmd\gaitretarget verify --data .\data --source src-walk-001 --target target-compact-01 --store .\artifacts\verify-main --interrupt-after 2 --out .\artifacts\verification-report.json
Assert-GoExit

$pairs = @(
    @("src-walk-001", "target-compact-01"),
    @("src-walk-001", "target-human-02"),
    @("src-walk-001", "target-stylized-03"),
    @("src-walk-002", "target-compact-01-s2"),
    @("src-walk-002", "target-human-02-s2"),
    @("src-walk-002", "target-stylized-03-s2")
)

foreach ($pair in $pairs) {
    $source = $pair[0]
    $target = $pair[1]
    $store = Join-Path ".\artifacts" ("verify-{0}-{1}" -f $source, $target)
    go run .\cmd\gaitretarget verify --data .\data --source $source --target $target --store $store --interrupt-after 1
    Assert-GoExit
}
