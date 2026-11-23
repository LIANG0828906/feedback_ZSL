# 战术编队协同寻路与决策工作台

面向战术决策题材游戏的本地确定性工作台。仓库包含领域模型、样本生成器、定点化搜索/平滑/推演核心、增量缓存与失效传播、SQLite 检查点、窗口调度器、验收 CLI，以及无需 Node 的 Wails 桌面诊断界面。

## 快速开始

环境要求：

- Go 1.22+
- Windows 桌面构建另需 Wails CLI、WebView2 与 Windows CGO/打包环境
- SQLite 使用 `modernc.org/sqlite` 纯 Go 驱动，不依赖数据库服务

```bash
# 1. 生成固定种子的本地样本
go run ./cmd/samplegen -seed 20261124 -out data/scenario.json

# 2. 运行全部测试
go test ./...

# 3. 跑增量 / 断点恢复 / 全量三路径验收
go run ./cmd/verify \
  -scenario data/scenario.json \
  -db artifacts/acceptance.db \
  -out artifacts/acceptance-report.json
```

验收成功时 `artifacts/acceptance-report.json` 中会包含：

- `incremental_vs_restore = equal`
- `incremental_vs_full = equal`
- `restore_vs_full = equal`
- 失效成员、tick 区间与原因
- 路径缓存命中/重算、区间复用
- 并行窗口峰值、提交顺序与串行结果比对

## Wails 桌面应用

桌面壳位于独立嵌套模块 `desktop/`，因此根模块测试不会引入 WebKit/GTK 依赖。

```powershell
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
cd desktop
wails doctor
wails build -platform windows/amd64 -clean
```

构建结果：`desktop/build/bin/tactical-workbench.exe`。

应用启动时会：

- 默认用固定种子在内存中生成样本，无需外部文件或网络。
- 将 SQLite 检查点写入 `%AppData%\tactical-workbench\workbench.db`。
- 可用 `TACTICAL_SCENARIO` 覆盖场景 JSON 路径，用 `TACTICAL_DB` 覆盖数据库路径。

前端位于 `desktop/frontend/dist`，为原生 HTML/CSS/JS，并通过 `embed.FS` 打包；没有 Node/npm 构建步骤。界面支持：

- 代价底图、不可通行格、区域封锁
- 成员路径、拐点网格/世界坐标、队形槽位
- 逐成员 tick 轨迹、队形偏差与重试
- 格子代价编辑、封锁区域切换
- 失效传播、缓存命中、重算耗时和检查点状态

## 实际环境验证

当前执行环境为 Linux/amd64，`go env GOOS` 实际为 `linux`，与任务说明中的 Windows 目标不同。已完成：

- Go `1.22.12` 下 `go test ./...` 全通过。
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build` 成功。
- Wails CLI `v2.10.2` 的 `wails doctor` 成功扫描；当前容器缺少 `libwebkit2gtk`，因此未在本机启动 GUI。
- `wails build -platform windows/amd64 -clean` 成功产出 PE32+ Windows 可执行文件。
- Wails CLI `@latest`（v2.17.1）要求 Go 1.26，与任务指定的 Go 1.22 不兼容；项目固定使用 Go 1.22 可用的 `v2.10.2`。

详细记录见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。

## 命令

| 命令 | 用途 |
|---|---|
| `go run ./cmd/samplegen` | 生成确定性战术地图、编队、指令与真值 |
| `go test ./...` | 结构校验、搜索、平滑、推演、增量、检查点、调度器测试 |
| `go run ./cmd/verify` | 三路径一致性、失效清单与资源预算验收 |
| `make windows-build` | 交叉编译 Windows 桌面程序（Wails） |

## 错误码

| Code | 含义 |
|---|---|
| `E_SCHEMA` | 字段缺失、类型/有限性、重复成员、非法代价、越界、槽位越界等 |
| `E_HEUR` | 启发式高估、权重非法、搜索溢出/预算耗尽 |
| `E_FORM` | 槽位容差或成员最大间距不满足，携带成员对与当前间距 |
| `E_PARA` | 步长或转向速率越界 |
| `E_TRACE` | tick 轨迹越出折线、跳段、瞬转或 tick 预算耗尽 |
| `E_DIFF` | 增量与全量首差异，携带成员、tick 与数据项 |

实现细节、定点约定、依赖传播与并发边界见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 仓库结构

```text
cmd/samplegen/          确定性样本生成入口
cmd/verify/             三路径验收入口
internal/sample/        固定种子样本与真值生成
pkg/tactical/           校验、搜索、平滑、推演、依赖、缓存、SQLite、调度
pkg/workbench/          桌面/CLI 共用的操作服务
desktop/                Wails v2 桌面壳与静态前端
data/scenario.json      已生成样本
artifacts/              验收报告与本地运行产物
```
