# Terrain Studio Wails Desktop

这是地形材质管线的 Wails v2 桌面诊断壳。核心算法仍在仓库根 Go module 中，本模块通过根目录公共 SDK（`terrainpipeline` 包）调用，不复制烘焙逻辑。

## Windows 10/11

依赖：

- Go 1.22+
- Wails CLI v2.10+（`go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2`）
- WebView2 Runtime（Windows 11 通常内置）

运行：

```powershell
cd desktop
wails dev
```

构建：

```powershell
cd desktop
wails build
```

产物位于 `desktop/build/bin/TerrainStudio.exe`。SQLite 使用纯 Go 的 `modernc.org/sqlite`，不需要 MinGW/CGO。

也可直接做无 CGO 交叉编译检查：

```bash
cd desktop
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
```

## 界面能力

- Canvas 渲染分块法线图与坡度热力图
- 接缝最大角、违反数量与归因计数
- 失效块、重烘/缓存复用状态和原因链
- worker 排队事件、内存预算占用与提交序列
- 编辑块高程、修改材质强度、切换差分算子
- 注入单块失败、逐块重试、导出 PNG/JSON
- 运行根模块的 43 项本地验收并显示失败项

前端是 `frontend/dist/index.html` 中的无 npm 构建 vanilla JS；Wails 直接 embed 该目录。
