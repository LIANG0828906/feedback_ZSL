$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
function Assert-LastExit([string] $Step) {
    if ($LASTEXITCODE -ne 0) { throw "$Step failed with exit code $LASTEXITCODE" }
}

$Run = Join-Path $Root ".validation"
$Bin = Join-Path $Root "bin\spectralforge.exe"
New-Item -ItemType Directory -Force (Join-Path $Root "bin") | Out-Null
if (Test-Path $Run) { Remove-Item -Recurse -Force $Run }
New-Item -ItemType Directory -Force $Run | Out-Null

go build -o $Bin ./cmd/spectralforge
Assert-LastExit "go build"
go test ./...
Assert-LastExit "go test"

& $Bin sample --dir "$Run\sample" --seed 20261125
Assert-LastExit "sample"
& $Bin validate --sample "$Run\sample"
Assert-LastExit "validate"

& $Bin render --sample "$Run\sample" --checkpoints "$Run\cp-full" --out "$Run\out-full" --mode full | Set-Content "$Run\full.json"
Assert-LastExit "full render"
& $Bin render --sample "$Run\sample" --checkpoints "$Run\cp-realtime" --out "$Run\out-realtime" --mode realtime | Set-Content "$Run\realtime.json"
Assert-LastExit "realtime render"
& $Bin render --sample "$Run\sample" --checkpoints "$Run\cp-resume" --out "$Run\out-resume" --mode realtime --stop-after 5 | Set-Content "$Run\pause.json"
Assert-LastExit "paused render"
& $Bin render --sample "$Run\sample" --checkpoints "$Run\cp-resume" --out "$Run\out-resume" --mode resume | Set-Content "$Run\resume.json"
Assert-LastExit "resume"

& $Bin verify --sample "$Run\sample" --checkpoints "$Run\cp-full" --out "$Run\out-full" | Set-Content "$Run\verify.json"
Assert-LastExit "verify"
& $Bin compare --a "$Run\out-full" --b "$Run\out-realtime" --a-checkpoints "$Run\cp-full" --b-checkpoints "$Run\cp-realtime" | Set-Content "$Run\compare-realtime.json"
Assert-LastExit "compare realtime"
& $Bin compare --a "$Run\out-full" --b "$Run\out-resume" --a-checkpoints "$Run\cp-full" --b-checkpoints "$Run\cp-resume" | Set-Content "$Run\compare-resume.json"
Assert-LastExit "compare resume"

Get-FileHash "$Run\out-full\frame.png", "$Run\out-realtime\frame.png", "$Run\out-resume\frame.png" -Algorithm SHA256
Write-Host "Validation artifacts: $Run"
