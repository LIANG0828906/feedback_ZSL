# 语义着色一致性评估工作台交付说明

## 1. 交付范围

本仓库从空工作区搭建了零外部素材、可重复演示的 Go 工作台。固定种子生成 `64×48`、4 通道、`16px` 分块样本，共 `4×3=12` 个块和 5 个语义类别：`background`、`solid_coral`、`solid_teal`、`gradient_band`、`texture_grid`。

参考贴图包含两个已知色块、一条渐变带和一个内嵌纹理区域；重建贴图经过确定性的逐块量化和通道重排。真值包含 RLE 掩码、面积、整型中心坐标、采样点期望色差和整图解析聚合期望值，真值指纹写入 SQLite 元数据。

SQLite 驱动使用纯 Go 的 `modernc.org/sqlite`，依赖已放入 `vendor/`。运行时不需要外部图像、HTTP CDN、Docker 或网络依赖；可使用 `GOPROXY=off go build -mod=vendor ./...` 离线构建。

## 2. Windows 使用方式

要求原生 Windows 安装 Go 1.22 或更高版本。在 PowerShell 中：

```powershell
go version
go env GOOS
.\scripts\preflight.ps1
.\scripts\demo.ps1
```

也可以手动执行：

```powershell
go build ./...
go test ./...
go run ./cmd/ssw demo
go run ./cmd/ssw verify --db data\checkpoints\resume.sqlite --truth artifacts\truth.json
go run ./cmd/ssw serve --state artifacts\dashboard-state.json --addr 127.0.0.1:18080
```

浏览器打开 `http://127.0.0.1:18080/`。离线模式先设置 `$env:GOPROXY='off'`，再使用 `go build -mod=vendor ./...`。

在非 Windows 主机验证 Windows 目标：

```bash
GOOS=windows GOARCH=amd64 go build -mod=vendor -o artifacts/bin/ssw-windows-amd64.exe ./cmd/ssw
```

## 3. 代码地图

- `cmd/ssw/main.go`：`demo`、`generate`、`verify`、`serve`、`preflight` 命令。
- `internal/model`：清单、真值、块记录、区域指标、误差界和哈希模型。
- `internal/apierr`：结构化错误码、像素坐标、块坐标和字段名。
- `internal/sample`：确定性样本、RLE 真值、加载校验和异常定位。
- `internal/colormetric`：Q8 定点色差、窗口均值、整型 Newton 平方根和截断记录。
- `internal/eval`：纯函数分块评估、累加器合并、区域指标、阈值判定和扩散路径。
- `internal/bounds`：由量化档位、位深、分块尺寸和定点截断位推导误差界。
- `internal/runner`：资源预算、worker 池、有界队列、排队等待、状态机和稳定提交。
- `internal/checkpoint`：SQLite schema、原子事务、身份/版本校验和整体快照作废。
- `internal/verify`：独立验证器，不导入评估、调度、样本生成或边界推导主路径。
- `internal/pipeline`：实时、断点恢复、全量重算三路径编排和比较。
- `internal/diagnostics`：八类异常注入链路。
- `internal/dashboard`：回环 HTTP API 与无 CDN 单页诊断界面。

## 4. 度量与去重口径

- 色差支持 `rgb_l2_q8` 和 `rgb_l1_q8`，默认 `rgb_l2_q8`。
- 定点精度为 Q8，`model.FixedScale=256`；色差平方为 Q16。
- 平方根使用纯整数 Newton 迭代向下取整，不使用浮点累加。
- 区域均值为 `SumDistanceQ8 / Area`，标准差由 `SumDistanceQ8` 与 `SumSquaredQ16` 合并计算。
- 掩码边界跃变：对跨语义标签的右/下相邻边，计算两侧窗口色差得分之差的绝对值；每条边只归属于左/上像素所在区域和块。
- 面积只统计块核心像素，重叠边带仅补齐窗口和邻边，不重复计入面积。测试断言所有区域面积之和恰为 `Width*Height`。
- `RequiredBand = WindowRadius + 1`；推荐值随窗口半径和 `TileSize/16` 调整。当前样本 `radius=1`、`tile=16`，推荐边带为 2。

截断位置记录在 `PixelDistanceQ8`、`WindowDistanceQ8`、`StdFromSums` 的返回标记，以及 `BlockRecord.Truncations`、`RegionMetrics.Truncations` 中。

## 5. 调度、状态机和检查点

资源配置包括：

- `parallelism`：合法范围 `[1,64]`；
- `queue_budget`：合法范围 `[1,4096]`；
- `thread_pixel_buffer_limit`：必须容纳 `(TileSize+2*Band)^2` 像素的重叠缓冲。

配置越界返回 `E_PARA`，`field` 指出具体配置项。队列满时提交者阻塞，`QueueWait` 记录块序号、`queue_budget_exhausted` 原因、等待纳秒和入队前深度。

块状态迁移为：

```text
pending -> processing -> committed
processing -> failed
failed -> pending -> processing
processing -> pending
```

非法迁移返回 `E_STATE` 并保持原状态。单个块注入失败时进入 `failed`，可重试；稳定 committer 只按块序提交连续前缀，已提交块不会被污染。

SQLite 包含 `snapshot_meta` 和 `block_checkpoints` 两张表。每个块在一个事务中写入；`Store` 互斥锁、`SetMaxOpenConns(1)` 和单 committer 保证写入串行、顺序稳定。重启时任一哈希或版本不匹配会删除旧块表并重建，禁止部分混用。

## 6. 三路径与独立验证

`pipeline.RunThreePaths` 分别运行：

1. `realtime.sqlite`：有界队列实时提交；
2. `resume.sqlite`：先提交 3 块后关闭，再打开同版本库续跑；
3. `full.sqlite`：显式 reset 后一次性全量重算。

比较使用 `reflect.DeepEqual` 逐项检查 `RegionMetrics`、`ErrorBound` 和 `CommitSequence`。

独立验证命令：

```powershell
go run ./cmd/ssw verify --db data\checkpoints\resume.sqlite --truth artifacts\truth.json
```

验证器从 `checkpoint_json` 读取原始累加器，自行完成除法、标准差、边界均值、传播最大值和误差界推导，并以真值 RLE 逐块核对面积。发现篡改时返回首个不一致项，包含 `region_id`、`block_index`、整型块坐标和 `field`，CLI 退出码为 2。

## 7. 异常注入与面板

HTTP 接口包括 `/api/health`、`/api/state`、`/api/inject?kind=...` 和 `/api/inject-all`。页面展示块状态矩阵、逐区域均值/真值曲线、偏差曲线、误差界对比、预算峰值、排队明细、误差扩散路径和三路径结果。所有 CSS/JS 均在内嵌 `index.html`，不访问 CDN 或外网。

| 注入 | 错误码 | 定位 |
| --- | --- | --- |
| 贴图字节截断 | `E_DECODE` | 字段名加像素坐标 |
| 宽高/整除/通道矛盾 | `E_SCHEMA` | 参数字段 |
| 量化档位或预算越界 | `E_PARA` | `quant_gear`、`parallelism` 等 |
| 标签值超出枚举 | `E_LABEL` | `labels` 加首个坏像素 |
| 单块内部失败 | `E_STAGE` | 块坐标、可重试 |
| 非法状态迁移 | `E_STATE` | 块坐标、from/to |
| 边带不足 | `E_BAND` | 块坐标、`boundary_jump_q8` |
| 实测超过误差界 | `E_BOUND` | 区域 ID 和四项实测值 |

## 8. 实际验证结果

完整日志见 `artifacts/validation-results.txt`。最近一次结果为：

- `GOPROXY=off go test -mod=vendor ./... -count=1`：全部包 `ok`。
- `GOPROXY=off go vet -mod=vendor ./...`：通过。
- `GOPROXY=off go build -mod=vendor ./...`：通过。
- `GOOS=windows GOARCH=amd64 go build -mod=vendor ...`：通过，生成约 14 MB `ssw-windows-amd64.exe`。
- demo：三路径一致 `true`，5 个区域，提交序列 `[0 1 2 3 4 5 6 7 8 9 10 11]`，独立验证 `ok=true mismatches=0`。
- 八类注入全部返回预期错误码且 `ok=true`。

当前验证主机是 Linux/WSL，因此主机原生 `go env GOOS=linux`；但 `GOOS=windows go env GOOS` 输出 `windows`，并已完成 Windows/amd64 交叉编译。在原生 Windows PowerShell 运行 `scripts/preflight.ps1` 时，`go env GOOS` 会直接显示 `windows`。
