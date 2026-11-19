[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Push-Location $Root
try {
    $env:GOTOOLCHAIN = "local"
    $env:CGO_ENABLED = "0"
    Write-Host "Running Go tests..."
    & go test ./...
    if ($LASTEXITCODE -ne 0) {
        throw "go test failed"
    }

    $BuildDir = Join-Path $Root "build-win"
    New-Item -ItemType Directory -Force -Path $BuildDir | Out-Null
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $targets = @(
        @{ Name = "coordinator"; Package = "./cmd/coordinator" },
        @{ Name = "worker"; Package = "./cmd/worker" },
        @{ Name = "taskgen"; Package = "./cmd/taskgen" },
        @{ Name = "verify"; Package = "./cmd/verify" },
        @{ Name = "probe"; Package = "./cmd/probe" }
    )
    foreach ($target in $targets) {
        $output = Join-Path $BuildDir ($target.Name + ".exe")
        Write-Host "Building $output..."
        & go build -trimpath -ldflags="-s -w" -o $output $target.Package
        if ($LASTEXITCODE -ne 0) {
            throw "build failed: $($target.Package)"
        }
    }
    Write-Host "Windows binaries are in $BuildDir"
}
finally {
    Pop-Location
}
