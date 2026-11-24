param(
    [string]$Output = "dist/yuvworkbench.exe"
)

$env:GOOS = "windows"
$env:GOARCH = "amd64"
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Output) | Out-Null
go build -trimpath -o $Output ./cmd/yuvworkbench
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
Write-Output "built $Output"
