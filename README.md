# Input Kernel

跨平台游戏输入层的确定性手柄映射与动作合成内核。项目从空工作区实现，仅使用 Go 1.22+ 标准库，不依赖容器、数据库或网络。

## 交付内容

- 设备描述、轴/按键布局、死区、反向、触发方向、定点精度、事件流、绑定表与真值协议。
- 严格样本加载与结构校验，错误码为 `E_SCHEMA`、`E_PARA`、`E_DIFF`、`E_VER`、`E_STORE`。
- 全整数定点映射管线：量化、死区、反向、分段线性响应曲线、动作状态合成。
- 组合键、长按、双击的确定性整数逻辑时间判定。
- 显式依赖图与缓存键，支持失效范围查询、拓扑波次增量重算和原子缓存替换。
- 增量/全量逐事件比对，首个差异事件与字段定位。
- 逐事件误差传播报告与四项累积上界、潜在判定翻转事件定位。
- 整体原子快照、内容/版本校验、失配时整体作废与全量重放恢复。
- 设备热切换、未映射物理输入清单、最大并发和每工作线程有界队列。
- 固定种子确定性样本生成器和可直接重复执行的 CLI。

## 环境

Windows 64 位：

```powershell
go version
```

要求 Go 1.22 或更高版本。项目没有第三方依赖，可离线构建。

## 复现实验

在工作区根目录执行：

```sh
go test ./...
go run ./cmd/inputkernel generate --seed 20261124 --out samples
go run ./cmd/inputkernel validate --samples samples
go run ./cmd/inputkernel run --samples samples --store snapshots --out artifacts/report.json
```

Windows PowerShell 中命令相同。若 `gofmt` 不在 `PATH`，可使用 `"$(go env GOROOT)/bin/gofmt"`（sh）或 `& "$(go env GOROOT)\bin\gofmt.exe"`（PowerShell）。

`run` 会依次完成：

1. 加载并校验全部样本。
2. 从空缓存全量回放。
3. 对照生成器直接记录的真值验收轴归一值、死区判定、动作状态和触发时刻。
4. 保存快照，重新加载并逐事件比较。
5. 构造版本失效快照，确认返回 `E_VER` 后从空缓存全量恢复。
6. 修改曲线参数触发失效，只重算失效节点，并与相同参数下的全量重放逐事件比较。
7. 输出误差传播报告、未映射输入和线程池统计。

默认种子为 `20261124`。种子会确定性地扰动轴原始值；同一种子生成的 JSON 文件逐字节一致。

## 样本

`samples/manifest.json` 引用四个场景：

| 场景 | 覆盖点 |
| --- | --- |
| `dual_stick` | 双摇杆、组合键、长按、双击、自动连发 |
| `single_stick` | 单摇杆、正向单极扳机、较小定点精度 |
| `trigger_gyro` | 正/负触发方向、轴反向、陀螺仪曲线、长按与双击 |
| `hot_switch` | 运行中设备切换、轴/按键差异、未映射输入、已提交动作保留 |

每个场景文件包含 `devices`、`curves`、`bindings`、`events` 和 `truth`。`truth` 由生成器在构造输入时独立计算并直接写入，生成器不调用内核回放代码，也不能从内核输出反推真值。

关键协议字段：

- `fixed_scale`：每个设备声明的归一化定点比例。
- `deadzone`：归一化定点单位下的死区阈值。
- `direction`：`both`、`positive` 或 `negative`。
- `invert`：轴反向声明。
- `quantization_residual`：精确有理数残差，`num/den`，其中 `den` 为原始轴跨度。
- `truth.axis_samples`：期望归一值、量化误差上界、死区内外判定与边界裕度。
- `truth.actions`：期望动作状态、事件序号和精确触发时刻。
- `truth.unmapped`：热切换后未映射物理输入。

## 定点映射语义

所有核心计算使用 `int64`，不使用浮点。归一化后值域为 `[-fixed_scale, fixed_scale]`，单极轴使用 `[0, fixed_scale]` 或 `[-fixed_scale, 0]`。

处理顺序：

1. 原始轴值按声明范围线性归一化并四舍五入到定点单位。
2. 记录精确量化残差 `exact - quantized`。
3. 应用死区；边界比较为 `abs(value) <= deadzone`。
4. 按 `invert` 翻转值和残差符号。
5. 按分段线性曲线对定点值做整数插值。
6. 由动作绑定输出 `pressed`、`held` 或 `released`。

舍入规则为最近整数，恰好半单位时远离零。逻辑时间、窗口、阈值均为 `int64` 毫秒。

动作语义：

- 普通按钮：按下 `pressed`，自动连发 `held`，释放 `released`。
- 组合键：所有成员在 `window_ms` 内先后进入按下态时，在最后成员按下事件触发。
- 长按：持续到 `threshold_ms` 时产生 `pressed`，`trigger_time_ms = down_time + threshold_ms`。
- 双击：两次完整按下/释放，第二次释放距第一次按下不超过 `window_ms`，并满足 `min_gap_ms`。
- 轴绑定：死区外首个事件 `pressed`，后续 `held`，回到死区时 `released`。

## 依赖图与增量

节点拓扑：

```text
device -> axis --------\
curve ------------------> binding -> timing -> action
```

缓存键为 SHA-256，输入包括：

- 场景完整事件流内容哈希。
- 定点精度参数版本和绑定规则版本。
- 节点标识。
- 节点参数哈希。
- 排序后的上游缓存键。

参数变化时，依赖图提供失效节点清单及原因。增量重算按拓扑层分波，仅调度失效节点；未失效节点直接从旧缓存复制。所有结果先在临时缓存构建，全部成功后才一次性替换运行时缓存和参数，因此失败不会留下半提交状态。

`CompareRuntimesChecked` 同时比较动作输出流和轴样本流，包括状态、触发时刻、归一值、死区判定、量化残差和曲线误差。不一致时返回 `E_DIFF`，`first_diff` 含首个事件序号、流名和差异字段。

## 误差传播报告

报告逐事件记录：

- 轴量化残差与单事件上界 `1` 个定点单位（半单位向上取整）。
- 死区边界裕度 `abs(abs(normalized) - deadzone)`。
- 曲线线性化误差上界：

```text
ceil(curvature_ppm * segment_width^2 / (8 * domain_scale * 1_000_000))
```

- 时序判定边界余量，以及观测事件间隔。

四项累积上界：

- 量化：累加每个已观测轴样本的单事件上界。
- 死区：由累积量化上界推导，因为死区判定位于曲线之前。
- 曲线：累加曲线弦误差和传播的量化上界。
- 时序：对每次已观测时序检查累加 `min(window_ms, event_gap_ms)`。

当累积上界达到或超过对应观测裕度时，报告把该事件列入 `potential_flip_events`，不推断缺失事件。

## 快照与恢复

快照包含：

- 全部节点缓存、节点缓存键、参数哈希和依赖。
- 输入事件内容哈希、配置哈希、缓存内容哈希。
- 定点精度参数版本、绑定规则版本。
- 快照结构版本。

写入先写同目录临时文件，执行 `fsync`，再以原子替换提交；同一 `SnapshotStore` 的写入由互斥锁串行化。Unix 使用 `os.Rename`，Windows 使用 `kernel32!MoveFileExW` 的 `REPLACE_EXISTING | WRITE_THROUGH`。

恢复时先校验整个快照：版本、三类内容哈希、节点集合是否完整、节点参数哈希、依赖和缓存键。任一不匹配即整体拒绝并返回 `E_VER`，不加载任何旧节点；调用方可随后从空缓存全量重放。

## 热切换与并发

`device_switch` 事件在同一逻辑时间轴切换当前设备。切换时：

- 已完成动作输出保持提交状态，不回溯改写。
- 旧设备仍处于活动态的动作在切换时刻确定性释放。
- 新设备未绑定的轴或按键事件逐条写入未映射清单，不静默丢弃。

重放与增量按节点级拓扑波次并行。`MaxWorkers` 限制并发度，`PerWorkerBuffer` 限制每工作线程排队单元数。队列满时调度器阻塞并记录 `per_worker_buffer_full`、任务 ID、工作线程和等待时长。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、布局冲突、重复标识、时间倒序、轴值越界、事件控制未声明等 |
| `E_PARA` | 时间窗越界、最小间隔大于窗口、长按阈值倒置等参数错误 |
| `E_DIFF` | 增量与全量动作/轴样本流不一致 |
| `E_VER` | 快照结构、内容哈希、版本、节点键或节点集合不匹配 |
| `E_STORE` | 本地快照目录或文件 I/O 失败 |

错误 JSON 包含 `field`、`item`、`measured`、`detail` 和可选的 `first_diff`。

## 实际验证结果

在 Go `1.22.12`、Linux amd64 上执行；同时通过 `GOOS=windows GOARCH=amd64 go build ./...` 交叉编译。

```text
dual_stick     truth=true  actions=16  axis=8   incr_equal=true  snapshot=valid
single_stick   truth=true  actions=13  axis=8   incr_equal=true  snapshot=valid
trigger_gyro   truth=true  actions=16  axis=11  incr_equal=true  snapshot=valid
hot_switch     truth=true  actions=20  axis=8   incr_equal=true  snapshot=valid
status=ok
```

验证覆盖：

- 四场景全部动作与轴样本在当前真值容差内通过，实测最大轴归一值偏差为 `0` 个定点单位。
- 每个场景增量重算后的动作流和轴样本流均与全量重放逐事件相等。
- 每个场景快照保存、加载和恢复后逐事件相等。
- 版本失配快照返回 `E_VER`，随后空缓存全量恢复且与源运行结果相等。
- 热切换场景产生 7 条未映射输入记录，已提交动作输出未改写。
- 并发快照写入 8 路并发测试无交错，最终快照完整可验证。
- 单工作线程、容量 1 的队列测试稳定记录阻塞原因 `per_worker_buffer_full`。

完整机器可读结果见 `artifacts/report.json`，样本与快照分别位于 `samples/`、`snapshots/`。

## 目录

```text
cmd/inputkernel/       CLI 入口
internal/model/        协议、错误、严格校验
internal/fixed/        整数定点数学与曲线
internal/kernel/       回放、状态机、依赖图、增量、报告、快照、线程池
internal/samplegen/    确定性样本与独立真值生成
internal/sampleio/     样本加载与内容哈希
internal/app/          验收编排与报告
samples/              固定种子样本
snapshots/            场景快照
artifacts/            机器可读验收报告
```
