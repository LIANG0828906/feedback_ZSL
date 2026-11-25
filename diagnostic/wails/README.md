# QKernel Wails 诊断壳

这个独立 Go module 复用根 module 的 `qkernel/diag` 本地 Handler 与内嵌
`diag/web/index.html`，不加载 CDN、外网字体或远程脚本。

在 Windows 上安装 Wails CLI 与 WebView2 后：

```powershell
cd diagnostic\wails
go mod tidy
wails build
.\build\bin\qkernel-diagnostic.exe -root ..\..\demo-output
```

开发模式：

```powershell
wails dev -- -root ..\..\demo-output
```

也可以不使用桌面壳，直接在仓库根运行：

```bash
go run ./cmd/qkernel serve --root demo-output --addr 127.0.0.1:8787
```
