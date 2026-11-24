# 游戏关卡声学编辑工作台

面向游戏关卡音频设计的环境声遮蔽与混响区域编辑工作台，使用纯 Go 实现，
覆盖确定性样本生成、定点遮挡/绕射判定、按混响区域合成衰减曲线与声压级、
精确失效的增量重算、SQLite 检查点断点恢复，以及 `net/http` 交互式编辑界面。

## 快速开始

要求 Go 1.22+。

```bash
go mod download
go build ./...
go test ./...

# 生成确定性样本
go run ./cmd/workbench -stage generate

# 三路径一致性 + 真值核对 + 断点恢复，报告写入 artifacts/runs/
go run ./cmd/workbench -stage verify

# 启动交互工作台，浏览器访问 http://127.0.0.1:8080
go run ./cmd/workbench -stage serve
```

Windows 命令行使用等价的 `go build ./...`、`go test ./...` 与 `go run ./cmd/workbench ...`；
SQLite 驱动为纯 Go（`modernc.org/sqlite`），`set CGO_ENABLED=0` 后仍可构建。

## 目录

- `cmd/workbench`：CLI 入口（generate / verify / serve）
- `internal/`：模型、定点几何、声学合成、生成器、加载校验、增量编排、SQLite 存储、HTTP 工作台
- `docs/DESIGN.md`：架构、公式、失效传播、错误码与验证结果
- `data/`：确定性样本与本地 SQLite 数据库（运行时生成）
- `artifacts/runs/`：声学剖面与三路径比对报告（运行时生成）

## 核心保证

- 定点几何精度声明（默认 4 位小数，区间 `[1,8]`），墙体处理顺序无关、结果确定。
- 区域重划/参数编辑只失效受影响对及其下游剖面，其余复用持久化缓存。
- 增量、断点恢复、全量三条路径对同一编辑序列逐采样一致，差异返回 `E_DIFF` 并定位。
- 检查点按听者窗口串行原子写入，版本或内容哈希不匹配时快照整体作废。
- 线程池并行度与每线程缓冲有界，缓冲耗尽阻塞排队并记录背压，不静默丢弃。
