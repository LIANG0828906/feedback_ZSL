@echo off
setlocal
if not exist samples\manifest.json (
  python -m haptic_studio generate --out samples
)
python -m haptic_studio gui --samples samples --runtime runtime
endlocal

