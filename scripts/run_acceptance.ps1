param(
    [string]$OutDir = ""
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Build = if ($env:BUILD_DIR) { $env:BUILD_DIR } else { Join-Path $Root "build" }
if ([string]::IsNullOrWhiteSpace($OutDir)) {
    $OutDir = Join-Path $Root "artifacts\acceptance"
}

cmake -S $Root -B $Build -DCMAKE_BUILD_TYPE=Release
cmake --build $Build --config Release -j 4
ctest --test-dir $Build -C Release --output-on-failure

if (Test-Path $OutDir) {
    Remove-Item -Recurse -Force $OutDir
}
New-Item -ItemType Directory -Path $OutDir -Force | Out-Null

$Exe = Join-Path $Build "chunkpipe.exe"
if (-not (Test-Path $Exe)) {
    $Exe = Join-Path $Build "Release\chunkpipe.exe"
}
if (-not (Test-Path $Exe)) {
    throw "chunkpipe executable was not found"
}

function Invoke-ChunkPipe {
    param(
        [string[]]$Arguments,
        [string]$LogPath
    )
    & $Exe @Arguments *> $LogPath
    if ($LASTEXITCODE -ne 0) {
        throw "chunkpipe failed with exit code $LASTEXITCODE; see $LogPath"
    }
}

Invoke-ChunkPipe -Arguments @("generate", "--out", (Join-Path $OutDir "samples")) `
    -LogPath (Join-Path $OutDir "generate.log")
Invoke-ChunkPipe -Arguments @("run", "--archive",
    (Join-Path $OutDir "samples\scene_v1.arc"), "--out",
    (Join-Path $OutDir "run-v1"), "--workers", "3", "--delay-us", "150") `
    -LogPath (Join-Path $OutDir "run-v1.log")
Invoke-ChunkPipe -Arguments @("run", "--archive",
    (Join-Path $OutDir "samples\scene_v2.arc"), "--out",
    (Join-Path $OutDir "run-v2"), "--workers", "3", "--delay-us", "150") `
    -LogPath (Join-Path $OutDir "run-v2.log")
Invoke-ChunkPipe -Arguments @("run", "--archive",
    (Join-Path $OutDir "samples\scene_v3.arc"), "--out",
    (Join-Path $OutDir "run-v3"), "--workers", "3", "--delay-us", "150") `
    -LogPath (Join-Path $OutDir "run-v3.log")
Invoke-ChunkPipe -Arguments @("compare", "--dir", (Join-Path $OutDir "samples"),
    "--out", (Join-Path $OutDir "compare")) `
    -LogPath (Join-Path $OutDir "compare.log")
Invoke-ChunkPipe -Arguments @("compare", "--dir", (Join-Path $OutDir "samples"),
    "--out", (Join-Path $OutDir "compare-partial"), "--max-loaded", "3") `
    -LogPath (Join-Path $OutDir "compare-partial.log")
Invoke-ChunkPipe -Arguments @("determinism", "--archive",
    (Join-Path $OutDir "samples\scene_v3.arc"), "--out",
    (Join-Path $OutDir "determinism")) `
    -LogPath (Join-Path $OutDir "determinism.log")

& $Exe export --archive (Join-Path $OutDir "samples\scene_v3.arc") `
    --out (Join-Path $OutDir "scene_v3_to_v2.arc") --to-version 2 `
    *> (Join-Path $OutDir "export-loss.log")
if ($LASTEXITCODE -ne 3) {
    throw "expected E_VERSION_LOSS exit status 3, got $LASTEXITCODE"
}

& $Exe run --archive (Join-Path $OutDir "samples\scene_v3.arc") `
    --out (Join-Path $OutDir "crash") --workers 3 --delay-us 100 --crash-after 3 `
    *> (Join-Path $OutDir "crash-first.log")
if ($LASTEXITCODE -ne 70) {
    throw "expected forced termination status 70, got $LASTEXITCODE"
}
$Before = @(Get-ChildItem (Join-Path $OutDir "crash\recovery") -Filter "*.snap").Count
if ($Before -ne 3) {
    throw "expected 3 committed recovery chunks, got $Before"
}

Invoke-ChunkPipe -Arguments @("recover", "--archive",
    (Join-Path $OutDir "samples\scene_v3.arc"), "--out",
    (Join-Path $OutDir "crash"), "--workers", "3", "--delay-us", "100") `
    -LogPath (Join-Path $OutDir "crash-recover.log")
$After = @(Get-ChildItem (Join-Path $OutDir "crash\recovery") -Filter "*.snap").Count
if ($After -ne 9) {
    throw "expected 9 committed recovery chunks, got $After"
}

$Determinism = Get-Content (Join-Path $OutDir "determinism\determinism.json") |
    ConvertFrom-Json
if (-not $Determinism.deterministic) {
    throw "determinism check failed"
}
$Diffs = Get-Content (Join-Path $OutDir "compare\diff_report.json") |
    ConvertFrom-Json
if (@($Diffs.differences).Count -eq 0) {
    throw "expected attributed visibility differences"
}
$PartialDiffs = Get-Content (Join-Path $OutDir "compare-partial\diff_report.json") |
    ConvertFrom-Json
$Causes = @($PartialDiffs.differences | ForEach-Object { $_.cause })
if ($Causes -notcontains "load_order") {
    throw "expected load_order attribution in partial comparison"
}
$MetadataCauses = @($Diffs.metadata_differences | ForEach-Object { $_.cause })
if ($MetadataCauses -notcontains "priority_derivation") {
    throw "expected priority_derivation attribution"
}
$ErrorReport = Get-Content (Join-Path $OutDir "error.json") | ConvertFrom-Json
if ($ErrorReport.code -ne "E_VERSION_LOSS") {
    throw "expected E_VERSION_LOSS report"
}

$NormalLog = Get-Content (Join-Path $OutDir "run-v3.log") -Raw
$RecoverLog = Get-Content (Join-Path $OutDir "crash-recover.log") -Raw
$NormalHash = [regex]::Match($NormalLog, "content_hash=([0-9a-f]+)").Groups[1].Value
$RecoverHash = [regex]::Match($RecoverLog, "content_hash=([0-9a-f]+)").Groups[1].Value
if ($NormalHash -ne $RecoverHash) {
    throw "normal and recovered v3 snapshot hashes differ"
}

Write-Host "acceptance: PASS"
Write-Host "output: $OutDir"
Write-Host "v3 snapshot hash: $NormalHash"
Write-Host "recovery commits: $Before -> $After"

