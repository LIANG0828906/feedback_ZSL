$ErrorActionPreference = "Stop"

Write-Host "== Go =="
go version
go env GOPATH GOROOT GOOS GOARCH CGO_ENABLED

Write-Host "`n== Wails =="
wails version
wails doctor

Write-Host "`n== WebView2 runtime =="
$webview = Get-ItemProperty `
  -Path "HKLM:\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" `
  -ErrorAction SilentlyContinue
if ($null -eq $webview) {
  $webview = Get-ItemProperty `
    -Path "HKCU:\SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" `
    -ErrorAction SilentlyContinue
}
if ($null -eq $webview) {
  Write-Warning "WebView2 runtime not found. Install Microsoft Edge WebView2 Evergreen Runtime."
} else {
  Write-Host "WebView2" $webview.pv
}

Write-Host "`nEnvironment check complete."
