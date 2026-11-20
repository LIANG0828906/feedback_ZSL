param(
    [string]$BuildDir = "$PSScriptRoot\..\build",
    [string]$WorkDir = "$PSScriptRoot\..\regression-out"
)

$ErrorActionPreference = "Stop"
$Root = Resolve-Path "$PSScriptRoot\.."
$Bin = Join-Path $BuildDir "Release\fill_pipeline.exe"
if (-not (Test-Path $Bin)) {
    $Bin = Join-Path $BuildDir "fill_pipeline.exe"
}
if (-not (Test-Path $Bin)) {
    cmake -S $Root -B $BuildDir -DCMAKE_BUILD_TYPE=Release
    cmake --build $BuildDir --config Release
    $Bin = Join-Path $BuildDir "Release\fill_pipeline.exe"
}

if (Test-Path $WorkDir) {
    Remove-Item -Recurse -Force $WorkDir
}
New-Item -ItemType Directory -Path $WorkDir | Out-Null

& $Bin generate-samples --out "$WorkDir\samples" --width 64 --height 64 |
    Out-Null
$manifest = Get-Content "$WorkDir\samples\manifest.json" -Raw |
    ConvertFrom-Json

foreach ($sample in $manifest.samples) {
    $name = [IO.Path]::GetFileNameWithoutExtension($sample.file)
    foreach ($run in @("a", "b")) {
        & $Bin render --input "$WorkDir\samples\$($sample.file)" `
            --out "$WorkDir\runs\$name\$run" | Out-Null
    }
    $a = Get-ChildItem "$WorkDir\runs\$name\a" | Sort-Object Name
    $b = Get-ChildItem "$WorkDir\runs\$name\b" | Sort-Object Name
    foreach ($file in $a) {
        $other = Join-Path $b[0].Directory $file.Name
        if ((Get-FileHash $file.FullName).Hash -ne
            (Get-FileHash $other).Hash) {
            throw "non-deterministic output: $name/$($file.Name)"
        }
    }
    $coverageName = if ($sample.fillRule -eq "nonzero") {
        "coverage_nonzero.pgm"
    } else {
        "coverage_evenodd.pgm"
    }
    $actual = (Get-FileHash "$WorkDir\runs\$name\a\$coverageName").Hash.ToLower()
    if ($actual -ne $sample.baselineCoverageHash) {
        throw "baseline mismatch: $name"
    }
}

foreach ($kind in @("overlap-edge", "degenerate-segment", "zero-area",
                    "hole-direction", "nonfinite")) {
    $out = "$WorkDir\injections\$kind"
    $response = & $Bin render --input "$WorkDir\samples\nested_hole.json" `
        --out $out --inject $kind 2>&1
    if ($LASTEXITCODE -ne 2 -and $LASTEXITCODE -ne 4) {
        throw "injection $kind returned $LASTEXITCODE"
    }
    if (Test-Path $out) {
        throw "injection $kind wrote output before rejection"
    }
    if (($response -join "`n") -notmatch '"injected":true') {
        throw "injection $kind did not report injection marker"
    }
}
$invalidBudgetOut = "$WorkDir\injections\invalid-budget"
$response = & $Bin render --input "$WorkDir\samples\nested_hole.json" `
    --out $invalidBudgetOut --budget "0,0.001,0.01,2" 2>&1
if ($LASTEXITCODE -ne 2 -or
    (($response -join "`n") -notmatch "E_TOLERANCE_INVALID")) {
    throw "invalid tolerance budget was not rejected"
}
if (Test-Path $invalidBudgetOut) {
    throw "invalid tolerance budget wrote output before rejection"
}

& $Bin render --input "$WorkDir\samples\nested_hole.json" `
    --out "$WorkDir\cancel" | Out-Null
$before = (Get-FileHash "$WorkDir\cancel\fill_bitmap.pgm").Hash
$response = & $Bin render --input "$WorkDir\samples\nested_hole.json" `
    --out "$WorkDir\cancel" --cancel-after-blocks 1 2>&1
if ($LASTEXITCODE -ne 4 -or (($response -join "`n") -notmatch "E_CANCELLED")) {
    throw "cancellation did not return E_CANCELLED"
}
$after = (Get-FileHash "$WorkDir\cancel\fill_bitmap.pgm").Hash
if ($before -ne $after) {
    throw "cancelled render replaced published bitmap"
}

& $Bin render --input "$WorkDir\samples\nested_hole.json" `
    --out "$WorkDir\submit-ok" | Out-Null
& $Bin submit --result "$WorkDir\submit-ok\result.json" | Out-Null

& $Bin render --input "$WorkDir\samples\nested_same_direction.json" `
    --out "$WorkDir\submit-degraded" | Out-Null
$response = & $Bin submit --result "$WorkDir\submit-degraded\result.json" 2>&1
if ($LASTEXITCODE -ne 9 -or
    (($response -join "`n") -notmatch "E_DEGRADED_RESULT")) {
    throw "degraded submission was not rejected"
}

Write-Host "regression ok: $($manifest.samples.Count) samples"
