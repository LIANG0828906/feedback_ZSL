param(
    [string]$Session = "artifacts/demo_session",
    [string]$RunRoot = "artifacts/runs/verified"
)

$ErrorActionPreference = "Stop"
$env:PYTHONPATH = "src"
& .\.venv\Scripts\python.exe -m audio_sync generate --out $Session
& .\.venv\Scripts\python.exe -m audio_sync validate --session $Session
& .\.venv\Scripts\python.exe -m audio_sync verify `
    --session $Session `
    --checkpoint-root "artifacts/runs/three-path-demo" `
    --out $RunRoot
& .\.venv\Scripts\python.exe -m audio_sync gui --session $Session

