# YUV 4:2:0 图像滤波增强工作台

本项目是一套面向游戏贴图与互动媒体素材的本地图像处理工作台：

- 按固定种子生成多分辨率、8/10/16 位深的确定性 YUV 4:2:0 样本及解析真值。
- 严格校验亮度/色度平面尺寸、位深、量化范围、字段完整性和文件大小。
- 在亮度链执行去噪、锐化、局部对比度增强；在 U/V 链执行带宽受限平滑和增益校正。
- 按显式依赖图和跨平面缓存键做增量重算；未失效阶段直接复用。
- 使用 `modernc.org/sqlite` 纯 Go 驱动持久化版本化阶段快照，阶段写入串行且原子。
- 支持主动暂停、上下文取消、重启恢复，以及增量/恢复/全量三路径逐像素比对。
- 输出增强 YUV 4:4:4、PNG、差异热力图、阶段耗时、重算次数、缓存命中和缓冲等待诊断。
- 全流程纯 Go，无 CGO、Docker 或容器依赖，可交叉编译到 Windows amd64。

## 快速开始

要求 Go 1.21 或更高版本。当前验证环境使用 `go1.22.12`。

```bash
go mod download
go test -race ./...
go build -o bin/yuvworkbench ./cmd/yuvworkbench

./bin/yuvworkbench generate -out samples -seed 20261124
./bin/yuvworkbench inspect -manifest samples/manifest.json \
  -image game_tile_64x48_8bit_rgb

./bin/yuvworkbench run -manifest samples/manifest.json \
  -image game_tile_64x48_8bit_rgb \
  -db artifacts/run/workbench.db \
  -out artifacts/run \
  -mode incremental

./bin/yuvworkbench verify -manifest samples/manifest.json \
  -out artifacts/verify -seed 20261124
```

Windows PowerShell 构建：

```powershell
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -o dist/yuvworkbench.exe ./cmd/yuvworkbench
```

进程成功退出码为 `0`，业务错误为 `2`，取消为 `130`，验收失败为 `3`。业务错误码始终以 `E_xxx` 出现在 JSON 或 stderr 中。

## 目录

```text
cmd/yuvworkbench       CLI：generate / inspect / run / verify
yuv/                   清单、样本生成、严格平面加载、上采样、PNG
filters/               参数校验、分块滤波、并行度与缓冲预算
workbench/             依赖图、缓存键、调度器、SQLite 快照、比对、热力图、验收
samples/               固定种子生成的样本清单和原始平面
artifacts/verify/      实际验收输出、SQLite 快照和报告
```

## 样本与清单

`generate` 默认生成三张样本：

| 样本 | 分辨率 | 位深 | 量化 | 色度对齐 | 上采样 |
| --- | --- | --- | --- | --- | --- |
| `game_tile_64x48_8bit_rgb` | 64x48 | 8 | full | left | bilinear |
| `vfx_96x64_10bit_video` | 96x64 | 10 | video | center | bilinear |
| `ui_128x72_16bit_full` | 128x72 | 16 | full | left | nearest |

每个平面分别存为 raw 文件。8 位平面每样本 1 字节；10/12/16 位平面使用 little-endian 每样本 2 字节。亮度平面尺寸等于图像尺寸，U/V 平面严格为亮度的一半。清单同时包含图像格式、平面路径与尺寸、色块矩形/中心、中心期望 YUV 值，以及若干采样点的期望 YUV 值和对应色度坐标。

加载时会检查：

- JSON 必需字段、支持的 schema、位深、量化范围、色度对齐和上采样规则。
- 亮度和色度尺寸必须为偶数，U/V 必须分别为亮度的 `1/2`。
- 每平面 `channels == 1`，所有平面位深与图像一致。
- raw 文件字节数必须精确等于 `width * height * bytesPerSample`。
- `full` 范围必须为 `0..(2^depth-1)`；`video` 范围必须为 `16..240` 的位深缩放值。

任何字段缺失、位深/量化不一致、尺寸或文件大小不匹配都会返回 `E_PLANE`，并带 `field`、`plane`、`expected` 和 `actual`。

## 滤波链

| 阶段 | 参数 | 允许范围 | 说明 |
| --- | --- | --- | --- |
| `Y:denoise` | `radius`, `strength` | `1..8`, `0..1` | 半径内盒式邻域与中心值混合 |
| `Y:sharpen` | `radius`, `amount` | `1..8`, `0..2` | 相对邻域均值的非锐化掩蔽 |
| `Y:contrast` | `window`, `amount` | 奇数 `3..31`, `0..2` | 局部均值对比度增强 |
| `U/V:smooth` | `radius`, `strength` | `1..8`, `0..1` | 带宽受限盒式平滑 |
| `U/V:gain` | `gain`, `offset` | `0.25..4`, `-0.5..0.5` | 围绕平面中值缩放；offset 为码值跨度的比例 |
| `COMPOSITE:interleave` | 对齐与上采样规则 | 清单声明 | 生成全分辨率 YUV 4:4:4，尺寸严格不变 |
| `heatmap` | `normalization` | `(0,262140]` | 差异归一化，独立于滤波缓存 |

所有阶段只读取输入平面，按行分块计算；每个像素独立且采用确定性四舍五入，因此改变 `chunkRows`、工作线程数或调度顺序不会改变字节结果。任意参数越界返回 `E_PARA` 和字段名。

## 缓存键与依赖图

规范拓扑及提交序固定为：

```text
0 Y:denoise -> 1 Y:sharpen -> 2 Y:contrast -----\
3 U:smooth  -> 4 U:gain ------------------------+-> 7 COMPOSITE:interleave
5 V:smooth  -> 6 V:gain ------------------------/
```

阶段缓存键为 SHA-256，输入包括：

1. 该链源平面内容哈希；
2. 平面名与阶段标识；
3. 该阶段参数规范 JSON；
4. 色度对齐方式与上采样规则；
5. 色度/算法版本；
6. 所有直接上游缓存键。

交织节点的显式依赖为 `Y:contrast`、`U:gain`、`V:gain`。参数或输入变化时，只有该节点及其下游闭包被标记失效。失效清单包含 `plane`、`stage`、`node` 和原因，例如 `parameter-changed`、`input-changed`、`missing`、`upstream-cache-key-changed:Y:sharpen`。

Y、U、V 链可由最多 `maxParallel` 个工作线程并发计算，但提交严格按上面的固定序号串行执行，因此并发度不改变阶段提交序列。

## SQLite 快照与恢复

依赖固定为 `modernc.org/sqlite v1.34.5`，无 CGO。

SQLite 使用单连接、WAL、`synchronous=FULL`、写互斥锁和逐阶段事务：

- `run_meta` 保存整体 `run_id`、schema/算法/参数/色度版本、输入哈希、清单哈希和完成标记。
- `stages` 保存固定序号、节点、缓存键、输入哈希、参数哈希、参数版本、色度版本、上游键、输出摘要、尺寸/位深/量化元数据和输出 BLOB。
- 每个阶段提交是独立事务；同一时刻只有一个写者，且提交序固定，不会交错。
- 新版本启动时在单个事务中清除旧阶段并写入新整体版本，禁止混用旧阶段。

重启加载会比较 schema、算法、参数、色度、输入内容和清单哈希。任一版本或内容哈希不匹配时整个快照作废并全量重算；摘要损坏同样整体拒识。版本匹配但 `complete = 0` 时，仅加载已提交阶段继续。

同一进程内修改阶段参数时，未失效阶段仍在内存缓存中直接复用，并以新版本原子写入新快照；进程重启后若参数版本已经不同，则按严格整体版本规则全量重算。这同时满足“参数变化只重算下游”和“重启禁止部分混用旧版本”两项约束。

## 暂停、取消与一致性

- `-pause-after N`：已提交 `N` 个重算阶段后停止；DB 中只保留已提交前缀，并标记 `complete = 0`。
- Go API 的 `RunOptions.Pause` 通道可随时请求暂停；CLI 的 `Ctrl+C`/中断信号触发取消。
- Go 上下文取消：返回 `E_CANCEL`，已完成并提交的阶段保留。
- 重启恢复：新引擎只读取版本匹配的已提交阶段，未提交节点按 `missing` 失效。
- 增量、恢复、全量路径使用同一确定性滤波实现和固定提交序。

`CompareRuns` 会比较最终 YUV 4:4:4 图像和每个中间单通道平面。首个差异会返回 `E_DIFF` 或 `E_PATH`，包含节点、平面、阶段、`(x,y)` 和左右像素值。

## 热力图与诊断

热力图仅接收原图、增强图和归一化值，逐像素取 Y/U/V 最大绝对差，经过固定黑-蓝-青-黄-红色带映射生成 PNG。相同输入和归一化值逐字节一致；非法归一化返回 `E_PARA`。

报告中按阶段记录：

- `durationMs`
- `recomputed`、`cacheHit`、`committed`、`persisted`
- `failed` 与错误
- `bufferWaitCount` 和带阶段/线程/平面原因的缓冲等待记录

统计区分 `full`、`incremental`、`resume` 和 `retry`。

## 资源边界

- `-max-parallel`：最多并行工作线程数。
- `-max-buffer`：全局临时平面缓冲字节预算。
- `-per-worker-buffer`：单线程单块缓冲上限，必须不大于全局预算。
- `-chunk-rows`：每次处理的最大行数。

工作线程在进入分块计算前申请预算，预算耗尽时阻塞排队并记录原因；上下文取消会终止等待。实际输出平面保持一份，临时缓冲按分块申请/释放。

## 实际验证结果

执行：

```bash
go test -race ./...
GOOS=windows GOARCH=amd64 go build -trimpath -o /tmp/yuvworkbench.exe ./cmd/yuvworkbench
./bin/yuvworkbench verify -manifest samples/manifest.json -out artifacts/verify -seed 20261124
```

结果：

- `go test -race ./...`：全部通过。
- Windows amd64 交叉编译：生成有效 PE32+ 控制台可执行文件。
- 验收 15 项全部通过，报告位于 `artifacts/verify/report.json`。
- 增量路径：失效 4/8 阶段，重算 4、命中 4；与空缓存全量路径的最终图像和全部中间平面逐像素一致。
- 暂停路径：提交 3 个亮度前缀后暂停；重启后重算 5、命中 3，与增量/全量路径一致。
- 版本识别：篡改整体参数版本后，8 个阶段全部失效，重算 8、命中 0。
- 故障重试：注入 `Y:sharpen` 失败不污染其他缓存；重试重算 3、命中 5，输出与无故障全量路径一致。
- 三路径最终 raw 的 SHA-256 相同：`0a940ef5bf1afc129f9b01333a6747304414a3e5883aa19f5a6a09f658f2eb1b`。
- 热力图重复生成逐像素一致；非法归一化返回 `E_PARA`。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_PLANE` | 清单字段、平面尺寸、位深、量化或文件大小非法 |
| `E_PARA` | 滤波/热力图参数越界 |
| `E_DIFF` | 增量与全量图像或中间平面不一致 |
| `E_PATH` | 增量/恢复/全量三路径不一致 |
| `E_SNAPSHOT` | SQLite 快照读写或提交失败 |
| `E_STAGE` | 单阶段执行失败，可重试 |
| `E_CANCEL` | 主动取消 |
| `E_RESOURCE` | 并行度/缓冲预算配置或分配失败 |
| `E_VERIFY` | 验收检查未全部通过 |
| `E_IO` | 文件或数据库 I/O 失败 |
