$ErrorActionPreference = "Stop"

$WailsVersion = if ($env:WAILS_VERSION) { $env:WAILS_VERSION } else { "v2.10.2" }

Write-Host "== Go =="
go version
$currentGoos = (go env GOOS)
Write-Host "Current GOOS: $currentGoos"
if ($currentGoos -ne "windows") {
    Write-Warning "Target host is Windows, but current GOOS is $currentGoos. Cross-build will still be attempted."
}

Write-Host "== Deterministic sample =="
go run ./cmd/samplegen -seed 20261124 -out data/scenario.json

Write-Host "== Tests =="
go test ./...

Write-Host "== Acceptance =="
go run ./cmd/verify -scenario data/scenario.json -db artifacts/acceptance.db -out artifacts/acceptance-report.json

Write-Host "== Wails =="
go install github.com/wailsapp/wails/v2/cmd/wails@$WailsVersion
$wailsCommand = Get-Command wails -ErrorAction SilentlyContinue
if ($null -eq $wailsCommand) {
    $wailsExe = Join-Path (go env GOPATH) "bin/wails.exe"
} else {
    $wailsExe = $wailsCommand.Source
}
& $wailsExe doctor

Push-Location desktop
try {
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    & $wailsExe build -platform windows/amd64 -clean
    $exe = Join-Path (Get-Location) "build/bin/tactical-workbench.exe"
    if (-not (Test-Path $exe)) {
        throw "Wails build did not produce $exe"
    }
    Get-Item $exe | Format-List FullName, Length, LastWriteTime
}
finally {
    Pop-Location
}
