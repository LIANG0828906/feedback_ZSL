param(
    [string]$Python = "",
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$LoadgenArguments
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot
if (-not $Python) {
    $candidate = Join-Path $repoRoot ".venv\Scripts\python.exe"
    $Python = if (Test-Path -LiteralPath $candidate) { $candidate } else { "python" }
}
$env:PYTHONPATH = Join-Path $repoRoot "src"
& $Python tools\loadgen.py @LoadgenArguments
exit $LASTEXITCODE

