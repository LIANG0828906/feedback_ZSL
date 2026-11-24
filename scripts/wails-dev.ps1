$ErrorActionPreference = "Stop"
$env:CGO_ENABLED = "1"
wails dev -tags wails -skipbindings -s
