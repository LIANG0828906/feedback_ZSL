# 量化色差取证工程说明

本仓库在空工作区中初始化，Go module 为 `quantforensics`，只依赖 Go 标准库。SQLite 检查点通过 cgo 直接调用环境已有的 `libsqlite3.so.0`，没有引入第三方 Go module。

## 快速运行

```bash
go test ./...
go test -race ./...
go vet ./...
go run ./cmd/forensics diagnose
go run ./cmd/forensics serve --addr 127.0.0.1:8088
```

诊断命令默认写入：

- `artifacts/diagnostics/index.html`：本地诊断界面。
- `artifacts/diagnostics/diagnostic-report.json`：结构化诊断结果。
- `artifacts/diagnostics/three-path-baseline.json`：完整三路径结果。
- `artifacts/diagnostics/screenshots/*.png`：标准库导出的三张固定种子截图。
- `artifacts/diagnostics/project.forensics.v1.json`、`project.forensics.v2.json`、`project.forensics.v3.json`：三版本样例工程。
- `artifacts/checkpoints/*.sqlite`：真实 SQLite 检查点库。

## 取证分层

统一内部模型是 `Project`，核心字段见 `model.go`。遇到色差争议时按以下顺序判定：

1. **块坐标换算级**：绝对像素坐标与块序号换算出现非零余数、溢出或矩形/内容哈希不一致，归类为 `block_coordinate_conversion`。
2. **调色板量化级**：坐标余数为零，但真值索引与实际索引不同；或索引相同而调色板颜色/增益重建仍有偏差，归类为 `palette_quantization`。
3. **位深量化级**：索引一致且逐分量偏差落在声明位深（例如 RGB565）的整型误差包络内，归类为 `bit_depth_quantization`。

归类器为 `ClassifyDiscrepancy`，RGB565 误差包络由 `MaxBitDepthDeviation` 对 0..255 全量整数枚举得到，不使用浮点拟合。

## 三版本 Schema

| 版本 | 标识 | 字段风格 | 调色板 | 块坐标 | 刻度 | 抖动 |
| --- | --- | --- | --- | --- | --- | --- |
| v1 | `forensics.v1` | `camelCase`，如 `projectId`、`rect.left` | `palette.constants` 扁平常量表 | `rect.left/top` 绝对像素 | `quantPermille`、`gainPermille` 千分比 | 无字段，升级补默认 `0` |
| v2 | `forensics.v2` | `snake_case` | 顶层 `palette_entries` 扁平表 | `block_col/block_row` 序号 | `*_permyriad` 万分比 | 仅全局 `dither_permyriad` |
| v3 | `forensics.v3` | `camelCase` | `paletteTree.families.swatches` 分组树 | `col/row` 序号 + `grid` 推导版本 | `*Permyriad` 万分比 | 全局 + `dither.perBlock[]` 逐块参数 |

加载入口：

- `LoadProject`：按 `schema`、`schema_version`、`schemaVersion` 识别版本。
- 未知版本返回 `E_SCHEMA`，字段路径类似 `$.schema`。
- 必填字段缺失或类型错误返回带 JSON 路径的 `*Error`，例如 `$.projectId`、`$.palette.constants[0].rgb`。
- `Project.SourceSchema` 保留来源版本，`Project.Traces` 保存重命名、默认值、刻度和坐标转换痕迹。

导出入口：

- `ExportProject`
- `DegradeProject`

降级规则：

- v3 分组树导出到 v1/v2：生成 `E_LOSSY` 清单，处置为 `flatten_groups_preserve_entry_values`。
- v3 逐块抖动导出到 v2/v1：逐块生成 `per_block_dither` 损失，处置为继承全局值或重置为默认 `0`。
- v1 不支持任何抖动字段：全局抖动也生成损失并归零。
- 定点刻度不能整除时记录 PPM 残差并按声明的 `floor` 方向降级。
- 导出的 JSON 均由对应版本加载器读回；测试将读回模型与 `DegradeProject` 的降级模型做语义哈希比较。

## 整型换算

内部刻度单位为 PPM（百万分之一）：

- 千分比 `v` → `v * 1000 PPM`
- 万分比 `v` → `v * 100 PPM`

坐标规则：

- 绝对坐标 → 块序号：`floor((abs - origin) / tile)`，余数是块内像素偏移。
- 块序号 → 绝对坐标：`origin + index * tile`。

所有乘法、加法、减法均经过 `checkedMulNonNegative`、`checkedAdd`、`checkedSub`；溢出错误码为 `E_OVF`，`Error.Op1`、`Error.Op2` 保留两个操作数。每次除法生成 `Residual`，记录字段路径、输入、除数、商、输出、余数、单位和取舍方向。

样本中的刻度和对齐坐标均选择可精确往返的值，因此高版本 → 低版本 → 高版本后调色板颜色/增益和块坐标完全一致，重复转换的痕迹哈希也一致。非整除值仍保留确定性截断残差，但会按降级规则产生精度损失。

## 确定性样本与量化

`GenerateSampleSet(DefaultSeed)` 固定生成：

- 3 张 `32×32` 截图。
- 每张 `4×4 = 16` 个 `8×8` 块，共 48 块。
- 前景色块、稠密渐变、半透明描边、嵌入式噪声和混合块。
- 每块真值索引、逐像素真值误差、期望主索引和块级期望色差。
- 面板脏块记录，覆盖调色板、几何和抖动编辑路径。

量化核心是纯函数 `QuantizeBlock`：

- 支持 `weighted_rgb`、`euclidean_rgb`、`max_component` 三种整数距离度量。
- 平局稳定选择较小索引。
- 用块像素 SHA-256 内容哈希、算法版本、参数版本和抖动量派生本地 SplitMix64 种子。
- 抖动偏移按像素/通道随机派生，不使用 `math/rand` 全局源。
- 块内使用分母 16 的整数 Floyd–Steinberg 误差分摊。
- 输出索引块、原始像素到所选调色板的逐像素误差、实际选色工作像素和抖动偏移。

因此结果只依赖块内容与参数，不依赖完成顺序、worker 数或批次。

## 重建与色差

`ReconstructBlock` 由索引和调色板恢复像素；`EvaluateBlock` 计算：

- R/G/B 逐分量最大绝对偏差。
- 加权 RGB 距离的最大整数平方根色差。
- 声明容差下的通过/不通过结论。
- 失败时的块 ID、绝对坐标、分量名、偏差值和所用容差。

容差只影响判定，不参与实测偏差计算；收紧容差不会减小测量值。诊断面板分别汇总 `dense_gradient` 与 `translucent_stroke` 的结论。

## 状态机与 SQLite 检查点

`StateMachine` 为每个块的 `load`、`quantize`、`reconstruct`、`verify` 四阶段维护：

`pending → running → committed/failed`

失败可 `failed → running` 重试；图失效可 `committed/failed → pending`。面板窗口使用同一组合法状态。非法迁移返回 `E_STATE`，并保持原状态。

`SQLiteStore.CommitStage` 使用：

```sql
BEGIN IMMEDIATE;
INSERT INTO stage_checkpoints (...);
COMMIT;
```

Go 互斥锁串行化进程内写入，SQLite 事务保证原子性。每行保存：

- 工程、块、阶段、状态和稳定顺序键。
- 输出摘要。
- 阶段缓存键。
- 块内容哈希。
- 参数版本和算法版本。
- 可恢复的阶段输出 JSON。
- 提交时间。

诊断库可用系统 `python3` 的 `sqlite3` 模块直接核验，不是自定义扩展名。

## 依赖图与精确失效

`BuildDependencyGraph` 显式建立：

- `palette:entry:<i>` → `index:<block>`
- `geometry:partition` → `geometry:block:<id>` → `load:<block>` → `index:<block>`
- `dither:global` → `dither:block:<id>` → `index:<block>`
- `index:<block>` → `recon:<block>` → `verify:<block>`

缓存键由 `ComputeCacheKey` 生成，输入包含：

- 块内容哈希。
- 调色板版本。
- 坐标推导版本。
- 度量、容差、刻度、抖动和算法/参数版本。
- 上游节点的实际缓存键。

调色板编辑使用 `AnalyzePaletteEdit` 做语义影响判定：如果旧索引没有使用被改条目，则用检查点中的实际工作像素比较“新条目距离”和“旧选中条目距离”，并按平局索引规则判断是否可能改选。只有真正会变化的块进入失效清单。几何和逐块抖动编辑直接比较对应参数。未失效块从 SQLite/内存检查点恢复，不重新量化。

## 调度预算与三路径

`Engine.Run` 使用固定 worker pool 和有界 block channel：

- `Workers` 限制最大并行度。
- `PixelBufferPerWorker` 是每个 worker 的像素缓冲上限；块超过上限返回 `E_BUDGET`。
- `MaxPendingBlocks` 是待处理块队列预算。
- 队列满时提交方阻塞，记录 `pending_block_queue_budget_exhausted`、块 ID 和等待纳秒，不丢弃任务。

三条验证路径由 `RunThreePathVerification` 执行：

1. `realtime`：4 worker 实时跑并提交 192 个阶段。
2. `breakpoint-recovery`：先提交前 6 个阶段后关闭 SQLite，再重新打开库恢复。
3. `full-recompute`：1 worker 全量重算。

`CompareRunReports` 逐块、逐像素比较索引、误差、重建像素、最大分量偏差、加权色差和通过结论；发现差异时给出块坐标与字段路径，例如 `quant.indices[17]` 或 `reconstruction.pixels[9].G`。提交序列还比较块、阶段、顺序键和缓存键。

## 测试索引

- `forensics_test.go`：schema 往返/损失、错误路径、定点溢出、容差、状态机、精确调色板失效、故障重试。
- `bitdepth_test.go`：位深整型包络和三级归类。
- `extra_test.go`：可配置距离度量和首个不一致定位。
- `smoke_test.go`：SQLite 三路径一致性冒烟。

实际验证输出见 `docs/VALIDATION.md`。
