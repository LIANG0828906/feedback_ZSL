# 观战投屏首版设计与验收口径

本实现只依赖 Go 标准库，模块名为 `mirrorcast`。没有引入第三方 SQLite 驱动，因此按任务允许的等价方案，使用目录内原子 JSON 检查点实现“先受控屏状态、后全局时刻序号”的提交语义。

## 1. 目录与职责

- `internal/bizerror`：稳定业务码 `E_*` 与可定位上下文。
- `internal/safemath`：声明为 64 位的整数运算、溢出检查、四舍五入除法和 int32 饱和转换。
- `internal/session`：内部统一模型、三版无关的几何/色度/预算纯函数、工程校验。
- `internal/projectio`：v1/v2/v3 DTO、装载器、写出器、千像素面积反解和语义冲突判定。
- `internal/generator`：固定种子的确定性工程、临界用例和真值构造。
- `internal/engine`：逻辑时钟、加入/退出、拓扑、预算重平衡、恢复状态和几何缓存。
- `internal/checkpoint`：两阶段文件检查点、SHA-256 校验和分步下发日志。
- `internal/incremental`：编辑候选构造、状态分叉、脏区间、缓存复用与失效来源。
- `internal/verify`：三版等价、真值重放、精度报告和样本目录复核。
- `internal/acceptance`：可被 `go test` 与 CLI 复用的端到端验收。
- `cmd/mirrorcast`：`generate`、`verify`、`acceptance` 命令。

## 2. 三版 Schema 映射

三版 wire schema 分别为：

- `mirrorcast.project/v1`：snake_case、扁平 `screens_flat`、像素宽高、千分比缩放、`PLANAR_420/422/444` 枚举。
- `mirrorcast.project/v2`：camelCase、按主机分组的 `hosts[].screens`、`areaKiloPixels + aspectWidth/Height`、万分比缩放、`chromaBitsPerPixel` 派生比特数。
- `mirrorcast.project/v3`：canonical 嵌套组、像素宽高与 `equivalentAreaKiloPixels` 交叉校验、万分比、平面枚举和派生比特数同时给出。

装载后统一为 `session.Project`，并按屏幕 ID 排序、按逻辑时刻稳定排序事件。因此主机分组或 JSON 文档顺序不会改变受控屏集合或镜像区域顺序。

千像素面积口径为：

```text
areaKiloPixels = width * height / 1000
```

生成器只选择宽高乘积可被 1000 整除的分辨率。v2 反解使用：

```text
factor^2 = areaKiloPixels * 1000 / (aspectWidth * aspectHeight)
width = aspectWidth * factor
height = aspectHeight * factor
```

若不能整除或不是完全平方数，返回 `E_RESOLUTION_AREA_INVALID`。v3 的面积与像素宽高不一致时返回 `E_RESOLUTION_CONFLICT`；枚举与比特数指向不同色度格式时返回 `E_SCHEMA_CONFLICT`。

## 3. 几何与色度口径

所有长度、面积、预算中间量均使用有符号 int64，并通过 `internal/safemath` 检查乘法/加法溢出。

缩放内部统一为万分比：

```text
scalePermyriad = scalePermille * 10
```

声明降档刻度为：

```text
10000, 8750, 7500, 6250, 5000, 3750, 2500
```

即 100%、87.5%、75%、62.5%、50%、37.5%、25%。

先按缩放档得到目标盒：

```text
boxWidth  = round_half_up(screenWidth  * scale / 10000)
boxHeight = round_half_up(screenHeight * scale / 10000)
```

再按主控区域宽高比 fit 到目标盒。宽源以宽为基准，否则以高为基准；另一维同样使用 round-half-up。目标矩形在受控屏内居中：

```text
x = floor((screenWidth  - outputWidth)  / 2)
y = floor((screenHeight - outputHeight) / 2)
```

奇数余量落在右/下边缘。

色度平面口径：

| 格式 | 平均比特/像素 | 最小亮度采样单元 | 色度平面宽 | 色度平面高 |
| --- | ---: | --- | --- | --- |
| `444` | 24 | `1x1` | `ceil(W/1)` | `ceil(H/1)` |
| `422` | 16 | `2x1` | `ceil(W/2)` | `H` |
| `420` | 12 | `2x2` | `ceil(W/2)` | `ceil(H/2)` |

缩放为零返回 `E_SCALE_ZERO`；缩放小于零或大于 10000 返回 `E_SCALE_OUT_OF_RANGE`。输出宽高小于对应最小采样单元返回 `E_RESOLUTION_TOO_SMALL`。

预算以 kbps 为单位，帧率固定声明为 60fps，位深 8bit：

```text
frameBits      = 8 * (W*H + 2*planeWidth*planeHeight)
costKbps       = ceil(frameBits * 60 / 1000)
```

## 4. 加入、退出、拓扑和降档

同一逻辑时刻内的事件按工程中的稳定顺序处理；屏幕和候选排序使用 `(priority asc, screen_id asc)`。`priority` 越小越先被降档或在最低画质仍超预算时移出。

加入判定：

1. 请求档费用不超过当前剩余预算：`admitted`，约束 `C-BUDGET-TOTAL`。
2. 否则从声明刻度中寻找不低于该屏最低档且费用可放入剩余预算的最高档：`downgraded`，记录原因 `INSUFFICIENT_BUDGET_HEADROOM_FOR_REQUESTED_SCALE`、预算余量和差额。
3. 最低档仍无法放入：`rejected`，稳定码 `E_ADMISSION_REJECTED`，约束 `C-MIN-QUALITY`。

退出删除唯一分配并释放预算，不重算、不升级其他屏幕。预算事件导致总预算不足时，按固定优先级逐档降到相邻声明刻度；所有屏都到最低档仍超预算时，按同一稳定顺序逐出并记录 `C-MIN-QUALITY` 与触发来源。

拓扑快照是包含全部已知屏幕的完整树，版本必须大于已提交版本。拓扑变更只更新拓扑版本、主机归属和在投分配上的拓扑版本标签，不改变几何或预算。

## 5. 检查点与续投

检查点目录包含：

- `screens-<tick>-<hash>.json`：受控屏分配、拓扑和可选分步进度。
- `checkpoint.json`：全局提交时刻、拓扑版本、预算汇总、屏幕状态文件名和 SHA-256。
- `dispatch-journal.json`：分步下发的幂等完成记录。

提交顺序：

1. 写新的屏幕状态临时文件，`sync`，原子 rename。
2. 最后写全局 `checkpoint.json` 临时文件，`sync`，原子 rename。

如果第一步后崩溃，旧全局文件仍指向旧屏幕状态，重启得到上一一致提交点；如果全局文件被截断或 JSON 损坏，返回 `E_CHECKPOINT_TRUNCATED`；哈希不符返回 `E_CHECKPOINT_CHECKSUM_MISMATCH`；格式/session 版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`，调用方应丢弃检查点并整段重算。

恢复时会重新计算分配费用、汇总预算并检查屏幕 ID 唯一，因此同一受控屏不会从检查点恢复出两份预算占用。

分步日志记录 `prepare_source`、`map_planes`、`send_regions`、`settle_budget` 等步骤；重启后已完成步骤返回命中，不重复执行。

## 6. 增量失效

`incremental.Evaluator` 保存：

- 当前工程；
- 跨版本/重算复用的几何键缓存；
- 每个已提交逻辑时刻的 `engine.State`；
- 当前完整 `TickResult`。

缩放、色度、预算或事件编辑会构造候选工程并从 `dirtyTick-1` 的状态分叉。早于脏点的 `TickResult` 直接深比较复用；脏点之后的预算/拓扑结算重算。未改变的几何键在共享 `MappingCache` 中命中；因预算重平衡而改变档位的屏幕会沿依赖链重算。

每份 `Report` 都包含：

- `dirty_from_tick`、`reused_ticks`、`recomputed_ticks`；
- 缓存命中/未命中数；
- 每个 tick/screen 的 `Provenance`，说明 `reused`、`geometry_recomputed`、`geometry_cache_reused_settlement_recomputed` 以及具体编辑来源。

## 7. 常用稳定业务码

| 场景 | 业务码 |
| --- | --- |
+| 不支持的 schema | `E_SCHEMA_UNSUPPORTED` |
| 语义字段冲突 | `E_SCHEMA_CONFLICT` |
| 像素/面积冲突 | `E_RESOLUTION_CONFLICT` / `E_RESOLUTION_AREA_INVALID` |
| 负值/零分辨率 | `E_RESOLUTION_NEGATIVE` / `E_RESOLUTION_ZERO` |
| 缩放为零 | `E_SCALE_ZERO` |
| 缩放越界 | `E_SCALE_OUT_OF_RANGE` |
| 色度枚举或比特数非法 | `E_CHROMA_ENUM_INVALID` / `E_CHROMA_BITS_INVALID` |
| 输出小于采样单元 | `E_RESOLUTION_TOO_SMALL` |
| 线程/缓冲预算越界 | `E_THREAD_OUT_OF_RANGE` / `E_BUFFER_BUDGET_OUT_OF_RANGE` |
| 整数溢出 | `E_OVERFLOW` |
| 加入被预算拒绝 | `E_ADMISSION_REJECTED` |
| 检查点截断/校验失败/版本不符 | `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CHECKSUM_MISMATCH` / `E_CHECKPOINT_VERSION_MISMATCH` |

错误对象还携带 `path`、`screen_id`、`tick`、`constraint` 和 `details`，便于定位注入样本。

## 8. 命令

```bash
go test -count=1 ./...
go run ./cmd/mirrorcast generate --seed 1118333214 --out samples/generated
go run ./cmd/mirrorcast verify --dir samples/generated
go run ./cmd/mirrorcast acceptance --work artifacts/acceptance
```

固定种子也可用十六进制 `0x42A8691E`，十进制为 `1118333214`。同一 seed 连续生成的目录已用 `diff -r` 验证逐字节一致。
