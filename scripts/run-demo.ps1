$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $Root

$env:GOMAXPROCS = "2"
$env:GOTOOLCHAIN = "local"
$GoBin = "C:\Program Files\Go\bin"
if (Test-Path $GoBin) {
    $env:Path = "$GoBin;$env:Path"
}

New-Item -ItemType Directory -Force -Path bin, artifacts | Out-Null

go test ./...
if ($LASTEXITCODE -ne 0) { throw "go test failed" }
go build -o bin\gateway.exe ./cmd/gateway
if ($LASTEXITCODE -ne 0) { throw "gateway build failed" }
go build -o bin\upstream.exe ./cmd/upstream
if ($LASTEXITCODE -ne 0) { throw "upstream build failed" }
go build -o bin\loadgen.exe ./cmd/loadgen
if ($LASTEXITCODE -ne 0) { throw "loadgen build failed" }

$upstreamOut = Join-Path $Root "artifacts\upstream.out.log"
$upstreamErr = Join-Path $Root "artifacts\upstream.err.log"
$gatewayOut = Join-Path $Root "artifacts\gateway.out.log"
$gatewayErr = Join-Path $Root "artifacts\gateway.err.log"

$upstream = Start-Process -FilePath ".\bin\upstream.exe" `
    -ArgumentList @("-h1", "127.0.0.1:18082", "-h2", "127.0.0.1:18083", "-h2c", "127.0.0.1:18084") `
    -RedirectStandardOutput $upstreamOut -RedirectStandardError $upstreamErr -PassThru
$gateway = Start-Process -FilePath ".\bin\gateway.exe" `
    -ArgumentList @("-config", "configs\gateway.json") `
    -RedirectStandardOutput $gatewayOut -RedirectStandardError $gatewayErr -PassThru

try {
    $ready = $false
    for ($i = 0; $i -lt 50; $i++) {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:19091/healthz" -TimeoutSec 1
            if ($response.StatusCode -eq 200) {
                $ready = $true
                break
            }
        } catch {
        }
        Start-Sleep -Milliseconds 100
    }
    if (-not $ready) { throw "gateway did not become ready" }

    & curl.exe --noproxy "*" -fsS http://127.0.0.1:18082/__control/reset | Out-Null
    & curl.exe --noproxy "*" -kfsS https://127.0.0.1:18083/__control/reset | Out-Null
    & curl.exe --noproxy "*" -fsS http://127.0.0.1:18084/__control/reset | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "upstream reset failed" }

    & .\bin\loadgen.exe -config configs\scenario.json -output artifacts\scenario-report.json
    if ($LASTEXITCODE -ne 0) { throw "scenario failed" }
} finally {
    if ($gateway -and -not $gateway.HasExited) { Stop-Process -Id $gateway.Id -Force }
    if ($upstream -and -not $upstream.HasExited) { Stop-Process -Id $upstream.Id -Force }
}
