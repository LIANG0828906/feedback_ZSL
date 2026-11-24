param(
    [switch]$RunDev
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$Root = Split-Path -Parent $PSScriptRoot
$Artifacts = Join-Path $Root "artifacts"
New-Item -ItemType Directory -Force -Path $Artifacts | Out-Null

function Invoke-Logged {
    param(
        [string]$Name,
        [scriptblock]$Command
    )
    $log = Join-Path $Artifacts ("windows-" + $Name + ".log")
    Write-Host "==> $Name (log: $log)"
    & $Command 2>&1 | Tee-Object -FilePath $log
    if ($LASTEXITCODE -ne 0) {
        throw "$Name failed with exit code $LASTEXITCODE"
    }
}

Push-Location $Root
try {
    Invoke-Logged "go-version" { go version }
    Invoke-Logged "go-env" { go env GOFLAGS GOPROXY GOVERSION }
    Invoke-Logged "go-test" { go test ./... -count=1 }
    Invoke-Logged "go-race" { go test -race ./... -count=1 }
    Invoke-Logged "go-vet" { go vet ./... }
    Invoke-Logged "wails-doctor" { wails doctor }

    Push-Location (Join-Path $Root "desktop")
    try {
        Invoke-Logged "desktop-workbench-test" { go test ./internal/workbench -count=1 }
        Invoke-Logged "wails-build" { wails build -platform windows/amd64 -skipbindings }
        if ($RunDev) {
            Invoke-Logged "wails-dev" { wails dev }
        }
    }
    finally {
        Pop-Location
    }

    Invoke-Logged "demo" { go run ./cmd/hdrproof demo --root artifacts/demo --workers 4 --buffer 2 }
    Write-Host "All Windows checks passed. See artifacts for logs and report.json."
}
finally {
    Pop-Location
}
