# Performance Expression Workbench

面向游戏配乐与互动媒体制作的多层演奏表情曲线工作台。项目包含确定性样本生成器、领域校验、多层叠加与通道优先级裁决、编辑会话、SQLite 检查点恢复、三路径一致性校验、异常注入、Fyne 时间轴界面及一键验收 CLI。

当前种子 `20261124` 的本地验收结果为 **PASS**：3 个工程、每组 3 段、逐段 16 个采样时刻；真值共 576 个采样点、288 条裁决明细，全部通过；实时编辑、全量重算、断点恢复三条路径的工程哈希与结果哈希完全一致。

## 快速开始

环境要求：

- Go 1.22 或以上。仓库使用 `modernc.org/sqlite@v1.63.0`，该版本声明 Go 1.26，因此 `go.mod` 使用 `go 1.26.0`，Go 的 `GOTOOLCHAIN=auto` 会自动获取所需工具链。
- Fyne `v2.4.5`。
- SQLite 为纯 Go 驱动 `modernc.org/sqlite`，无需系统 SQLite 服务。

```bash
go mod download
go build ./...
go test ./...
```

生成、校验、评估与完整验收：

```bash
go run ./cmd/workspace generate -out samples -seed 20261124
go run ./cmd/workspace validate -project samples/aerial-race.json
go run ./cmd/workspace evaluate -project samples/aerial-race.json -out artifacts/evaluation.json
go run ./cmd/workspace accept \
  -samples samples \
  -db artifacts/expression-checkpoints.db \
  -report artifacts/acceptance.json \
  -markdown artifacts/ACCEPTANCE.md
```

`go run ./cmd/workspace recover -project samples/aerial-race.json -db artifacts/expression-checkpoints.db` 会读取检查点；内容哈希或参数/算法/优先级版本有任一不匹配时，会整体作废快照并按批次操作日志重放。

## 桌面工作台

Fyne 桌面入口使用 `gui` 构建标签，默认构建保留可运行的 CLI 与桌面占位程序，因此没有原生图形依赖时 `go build ./...` 仍可完成。

Linux 需安装 Fyne 原生依赖（X11、GL、Xcursor、Xrandr、Xinerama、Xi、Xxf86vm 及 `pkg-config`）：

```bash
go build -tags gui -o workspace-desktop ./cmd/desktop
./workspace-desktop -project samples/aerial-race.json -db artifacts/ui-workspace.db
```

Windows 使用带 CGO 的 MinGW 工具链：

```powershell
$env:CGO_ENABLED=1
go build -tags gui -o workspace-desktop.exe ./cmd/desktop
.\workspace-desktop.exe -project samples/aerial-race.json -db artifacts\ui-workspace.db
```

界面能力：

- 时间轴按段绘制各层控制点、每层插值曲线、叠加后的已提交曲线和播放头。
- 拖动控制点只写工作副本；橙色叠加曲线保持为已提交状态，提交后刷新。
- 支持任意段内拍位起播、暂停与循环回放；同一采样时刻的取值只由拍位和参数决定。
- 可增删层、提升通道优先级、设置批次号并提交当前段落。
- 可注入 `priority_cycle`、`position_out_of_range`、`batch_conflict`、`commit_failure`，提交时分别走 `E_CYCLE`、`E_POS`、`E_CONFLICT`、`E_COMMIT`，失败段可清除故障后重试。

当前环境已实际完成 Linux `go build -tags gui`，并启动桌面程序 6 秒（进程因 `timeout` 退出，无启动错误）；同时使用本地解包的 MinGW-w64 GCC 13 POSIX 工具链完成 Windows 交叉编译，产出 41 MB 的 `PE32+ x86-64` Fyne 可执行文件。

## 工程结构

| 目录 | 职责 |
|---|---|
| `domain` | 工程、段、通道、层、控制点、优先级、真值、操作、检查点与错误码 |
| `generator` | 固定种子确定性生成 3 组演奏工程及解析真值 |
| `engine` | 结构校验、拓扑优先级、插值叠加、裁决明细、精度报告、差异定位、回放取值 |
| `session` | 工作副本、批量原子提交、提交回滚、故障注入、重试、有界并发池 |
| `checkpoint` | SQLite 串行批次写入、整体版本校验、快照作废、操作日志重放 |
| `workbench` | 检查点恢复及实时/全量/恢复三路径比对 |
| `acceptance` | 一键端到端验收与 JSON/Markdown 报告 |
| `ui` | Fyne 时间轴、控制点拖拽、回放、提交与故障操作 |
| `cmd/workspace` | 默认 CLI |
| `cmd/desktop` | `gui` 标签下的 Fyne 桌面入口 |

## 数据与裁决语义

每个 `Project` 包含 `samples_per_beat` 采样网格与若干 `Segment`。段内每个 `Channel` 有 `target`（`velocity`、`vibrato`、`pitchbend`、`sustain`）和多个 `Layer`。同一通道内所有层按层 ID 排序后逐点求和，避免输入遍历顺序影响浮点结果。

控制点支持 `linear`、`hold`、`smoothstep` 三种插值。曲线在首个控制点前与最后控制点后取端点值。每个采样时刻：

1. 对每个通道叠加全部层；
2. 对同一 `target` 的通道按优先级拓扑排序；
3. 最高优先级通道成为生效来源；
4. 生效来源与每个较低优先级来源值不同时，记录一条裁决明细，包含采样时刻、目标、覆盖通道、被覆盖通道、被覆盖值与声明依据；
5. 输出目标叠加值、生效来源和按稳定顺序排序的裁决序列。

优先级声明为“高 -> 低”有向边。拓扑排序使用通道 ID 作为稳定决胜键；检测到环时返回 `E_CYCLE` 并携带全部涉及通道。

## 错误码

| 错误码 | 触发场景 | 标识信息 |
|---|---|---|
| `E_SCHEMA` | 字段缺失、层 ID 重复、真值来源非法 | 段/通道/层（已知时） |
| `E_POS` | 控制点越界、拍位非严格递增、回放拍位越界 | 段/通道/层/拍位/tick |
| `E_PRIO` | 优先级声明缺失、引用未知通道 | 段与缺失通道列表 |
| `E_CYCLE` | 优先级成环 | 段与环内通道列表 |
| `E_DUP` | 重复提交同一成功批次 | 段与批次号 |
| `E_CONFLICT` | 注入批内冲突、非法批内容 | 段/通道/层 |
| `E_COMMIT` | 注入提交失败或持久化失败 | 段/通道/层与原因 |
| `E_DIFF` | 三路径/自动比对不一致 | 首个不一致段、tick、通道、字段、左右值 |
| `E_PREC` | 真值偏差超容差或来源不匹配 | 段、tick、拍位与偏差报告 |

## 编辑与检查点

- 拖动、增删层、调整优先级只修改工作副本并刷新工作缓存；`ExportCommitted`、`PlaybackAt` 与 `ExportCommittedJSON` 始终读取已提交模型。
- 提交以批次号为单位，可包含一个或多个段。先校验段与候选叠加结果，再写 SQLite 事务；任一步失败，已提交模型和叠加缓存均不变。
- 成功批次按 `seq` 递增写入 `checkpoints`。每行包含段快照、叠加摘要、优先级版本、内容哈希、参数版本、算法版本、操作日志与日志哈希。
- 恢复时逐行重算内容哈希、优先级版本并比较参数/算法版本。任一行失配则整表快照作废，随后从基础工程开始按稳定批次顺序重放操作日志并重建快照。
- 已成功批次号在会话与恢复状态中保留，重复提交返回 `E_DUP`。

## 三路径一致性

`workbench.RunThreePaths` 对同一工程、参数与编辑序列执行：

1. **realtime**：逐次编辑、逐批提交，使用实时缓存。
2. **full-recompute**：从基础工程一次性重放操作并全量求值。
3. **checkpoint-resume**：写入 SQLite，关闭并重开，按检查点恢复后求值。

三者的逐采样值、来源清单、裁决序列、工程内容哈希和结果哈希共同比较。比较按段、tick、目标、字段稳定排序，返回首个不一致位置及左右值。

## 并发边界

`session.Pool` 提供固定 worker 数与每 worker 固定事件缓冲上界；`session.BatchSubmitter` 将该并发边界直接绑定到编辑会话，提交阻塞等待可用缓冲、不丢弃任务，结果按提交序号稳定读取。超界时 `Blocked` 计数增加。SQLite Store 使用单连接、写互斥和事务，保证批次写入不交错。

## 实际验收结果

验收时间：`2026-11-24T04:51:00Z`（种子 `20261124`）。

| 工程 | 段数 | 采样时刻 | 裁决数 | 真值点 | 失败 | 来源不匹配 | 来源匹配率 | 最大绝对误差 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| `aerial-race` | 3 | 48 | 96 | 192 | 0 | 0 | 100% | 1.39e-17 |
| `ember-dialogue` | 3 | 48 | 96 | 192 | 0 | 0 | 100% | 5.55e-17 |
| `tidal-sanctuary` | 3 | 48 | 96 | 192 | 0 | 0 | 100% | 2.78e-17 |

三路径：

| 路径 | 工程 SHA-256 | 结果 SHA-256 | 批次数 |
|---|---|---|---:|
| `realtime` | `4f7b2be7359fe7e7a2940a2e0db5cb369ac7cf7c84a8bfa2d1162ad053c14991` | `8e6678dcefcec83d964f015dd074b27122035b17feb027084a0331196b1cbddf` | 3 |
| `full-recompute` | 同上 | 同上 | 3 |
| `checkpoint-resume` | 同上 | 同上 | 3 |

三组两两比较差异均为 `0`。有效检查点恢复加载 3 个批次；篡改参数版本后整体失效，报告明确列出 3 行版本不匹配，随后重放 3 个批次并得到完全相同的结果。并发验收使用 3 个 worker、每 worker 缓冲 1，提交/完成 9/9，阻塞 3 次，最大并行度 3。

完整机器可读结果见 `artifacts/acceptance.json`，摘要见 `artifacts/ACCEPTANCE.md`。

## 测试

```bash
go test ./...
go test -race ./...
```

覆盖内容包括：字段缺失/越界/优先级缺失/重复层/成环错误码与标识；插值；真值精度与容差单调性；输入顺序与 goroutine 无关性；裁决确定性；`E_DIFF`/`E_PREC` 定位；工作副本隔离；提交失败回滚；重复批次；四类故障与重试；SQLite 顺序写入、内容哈希失效、版本失效与操作重放；三路径一致性；Fyne 时间轴渲染与无头窗口构建。
