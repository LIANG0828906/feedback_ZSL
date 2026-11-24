$ErrorActionPreference = "Stop"
$env:CGO_ENABLED = "1"
wails build -tags wails -skipbindings -s -platform windows/amd64 -clean
