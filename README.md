# uvenhance

确定性的 YUV 4:2:0 图像滤波增强实现，用于替换美术组的多阶段手调脚本。项目完全使用 Go，SQLite 通过纯 Go 驱动 `modernc.org/sqlite` 访问，不依赖 Docker、GPU、外部账号或网络服务。

## 快速开始

要求 Go 1.22 或以上版本。Windows amd64 下：

```powershell
go version
go mod download
go test ./...
go build -o bin\uvtool.exe .\cmd\uvtool
```

Linux/macOS 下：

```bash
go test ./...
go build -o bin/uvtool ./cmd/uvtool
```

仓库已提交固定种子样本。重生成并校验真值：

```bash
./scripts/generate_samples.sh
# Windows: .\scripts\generate_samples.ps1
```

单次处理：

```bash
bin/uvtool run \
  -sample samples/sample_01 \
  -params configs/default_params.json \
  -db work/cache.db \
  -out work/run
```

三路径自动比对：

```bash
./scripts/verify_three_paths.sh
# 或单独指定：
bin/uvtool verify -sample samples/sample_01 -edits configs/fixed_edits.json \
  -workdir work/verify -cancel-step 2 -cancel-after-stages 1 -workers 2
```

取消、重启与全量重算的字节级比对：

```bash
./scripts/cancel_restart.sh
# Windows: .\scripts\cancel_restart.ps1
```

实际运行结果见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。

## 仓库结构

| 路径 | 内容 |
| --- | --- |
| `internal/yuv` | 样本加载、元数据/平面校验、参数范围、滤波、4:2:0 上采样、交织、真值校验 |
| `internal/cache` | 阶段定义、依赖图、缓存键、SQLite 版本化快照 |
| `internal/engine` | 失效判定、拓扑调度、线程池与缓冲边界、取消恢复、三路径比对、热力图、误差报告 |
| `internal/samplegen` | 固定种子 YUV 样本与解析真值生成器 |
| `cmd/uvtool` | CLI |
| `samples/sample_01` | 320x240 样本，带色块、渐变带、低对比度细节和真值 |
| `samples/sample_02` | 1608x904 样本 |
| `configs` | 默认参数、固定四步编辑序列、仅修改 U 增益的参数 |
| `scripts` | Linux/macOS 与 Windows PowerShell 的生成、三路径、取消重启脚本 |

## 样本格式

每个样本目录包含 `metadata.json`、`Y.plane`、`U.plane`、`V.plane`。

- 亮度为 `width x height`，色度为 `width/2 x height/2`。
- 位深支持 8/10/12/16；8 位每样本 1 字节，更高位深使用 2 字节小端。
- 元数据声明量化范围、色度对齐（`center` 或 `top_left`）和上采样规则（`bilinear` 或 `nearest`）。
- 每个平面单独声明尺寸和位深，便于检测三平面不一致。
- 真值包括色块中心坐标及期望 Y/U/V、亮度/色度采样点、4:2:0 上采样后的交织像素。
- `input_hashes` 覆盖平面几何、位深、量化范围和样本内容；加载时校验。

加载错误契约：

- 必需元数据字段缺失或 JSON 非法：`E_META`，`field` 指出缺失字段。
- 尺寸/位深与 4:2:0 声明冲突、平面文件长度或内容哈希不符：`E_PLANE`，同时给出 `plane`、`expected`、`observed`。
- 亮度宽高必须为偶数；色度尺寸必须严格为亮度一半；三平面位深必须一致。

## 滤波链

固定阶段与提交顺序：

1. `luma.denoise`
2. `luma.sharpen`
3. `luma.local_contrast`
4. `chroma_u.band_smooth`
5. `chroma_u.gain`
6. `chroma_v.band_smooth`
7. `chroma_v.gain`
8. `interleave`

所有滤波使用确定性整数/定点运算，保持平面分辨率和位深不变。`interleave` 按元数据声明的对齐与插值阶数生成全分辨率 YUV444 交织结果。

参数范围：

| 字段 | 范围 |
| --- | --- |
| `luma.denoise_strength` | `[0,1]` |
| `luma.sharpen_amount` | `[0,2]` |
| `luma.sharpen_threshold` | `[0,0.5]`（量化范围归一化） |
| `luma.contrast_strength` | `[0,1]` |
| `luma.contrast_radius` | `[1,8]` |
| `chroma_u.smooth_sigma` / `chroma_v.smooth_sigma` | `[0.1,4]` |
| `chroma_u.gain` / `chroma_v.gain` | `[0.5,1.5]` |
| `chroma_u.offset` / `chroma_v.offset` | `[-0.25,0.25]`（量化范围归一化） |

越界返回 `E_PARA`，并指出具体字段。

### 色度对齐与上采样

以 Q8 定点坐标计算：

- `center`：色度样本位于 2x2 亮度块中心，`sxQ8 = 64*x - 32`。
- `top_left`：色度样本对应 2x2 块左上亮度像素，`sxQ8 = 64*x`。
- 双线性先水平、再垂直，每一步使用整数四舍五入；`nearest` 为 0 阶。
- 边缘坐标夹取到有效样本范围。

## 缓存键与失效传播

阶段键为以下字段的 SHA-256：

```text
key_version + input_content_hash + stage_id + parameter_digest + upstream_cache_keys
```

- 根阶段输入哈希来自原始 Y、U 或 V 平面。
- 链内阶段输入哈希来自直接上游阶段的输出摘要和上游缓存键。
- `interleave` 输入哈希由三条父链输出摘要构成，上游键也显式记录三个父阶段。
- 每个阶段只使用自己的参数子集，因此修改 U 增益不会使 Y 链或 V 链失效。
- 失效清单包含平面名、阶段名和原因：`parameter_changed`、`input_changed`、`upstream_changed`、`cache_missing`、`snapshot_rejected` 或 `cache_key_mismatch`。

同进程编辑使用 `Engine.RunLive`，输入平面改变时可只重算对应平面链和交织。新进程重启使用 `Engine.Run`，必须通过整快照输入哈希和版本校验，不允许跨快照拼接旧阶段。

## SQLite 快照

使用 `modernc.org/sqlite`，单连接（`SetMaxOpenConns(1)`）加写互斥锁，保证事务不交错。每次阶段完成都在一个事务中：

1. 写入新的整体快照版本标识；
2. 写入当前全部阶段摘要、缓存键、参数摘要、对齐方式、提交序号和输出二进制；
3. 写入三平面输入内容哈希；
4. 删除旧快照；
5. 提交。

快照清单摘要覆盖流水线算法版本、参数/对齐版本、输入哈希、完整标志和全部阶段行。`PipelineVersion` 必须随阶段算法语义变化递增；它同时进入缓存键命名和整快照摘要，避免升级后复用旧算法输出。加载时会校验：

- `parameter_version`、`alignment_version`、对齐方式、上采样规则；
- Y/U/V 三平面内容哈希；
- 阶段数量、阶段元数据、输出载荷长度与输出摘要；
- 整体清单摘要。

任一项不符会删除整个快照并全量重算，缓存命中数必须为 0。`parameter_version` 表示参数模式版本；具体参数值进入阶段缓存键，因此同版本下的单参数编辑仍可增量复用。

## 取消与恢复边界

- `run -cancel-after-stages N` 在提交第 N 个新重算阶段后保存部分快照并返回 `E_CANCELLED`。
- 取消时只写 `partial_run.json`，其中 `complete=false`；不会生成完整 `run_report.json`。
- 重启从版本和全部输入哈希匹配的单一快照继续，已提交阶段直接命中缓存。
- 若发生进程级强制终止，SQLite 事务保证“提交了哪些阶段”是原子边界；未提交事务回滚。此时可重启续算，但强制终止进程本身不会生成完整报告。
- 三路径比对验证最终图像、8 个中间阶段、增量/恢复的历史提交序列和最终快照阶段顺序。

## 一致性比对与差异定位

`verify` 执行：

1. 实时增量路径：同一进程应用完整编辑序列，使用 `RunLive`。
2. 断点恢复路径：在指定步骤提交指定阶段数后优雅取消，再用新 `Engine` 重启完成。
3. 全量路径：空缓存下以最终参数一次性重算。

逐阶段比较平面/交织像素；不一致返回 `E_DIFF`，包含首个不一致像素 `x`、`y` 和所属阶段。最终输出同时校验声明宽高、位深与量化范围。

## 差异热力图

原图和增强图均转成全分辨率 YUV444 后计算：

```text
norm = (|dY| + |dU| + |dV|) / (3 * quant_range)
heat = clamp((norm - norm_min) / (norm_max - norm_min)) * 255
```

输出为确定性 PGM。`-heat-min`、`-heat-max` 写入统计；重复生成同一输入逐字节一致。

`enhanced.yuv444p` 与 `original.yuv444p` 为无文件头的交织 YUV444 原始数据：每个像素依次 `Y,U,V`；8 位每样本 1 字节，更高位深每样本 2 字节小端。

## 数值误差上界

`error_report.json` 逐色块记录：

- 色度上采样插值误差；
- 亮度锐化对比度残差；
- 色度增益量化误差；
- 交织取整残差；
- 四项之和的保守累积上界及 Y/U/V 通道界。

上界不使用真值偏差，也不使用邻域差异；邻域差异仅作为诊断字段。公式：

```text
interp     = full_range * 0.5^(order+1)
sharpen    = quant_step + |sharpen_amount| * full_range
gain_quant = quant_step * (1 + max(|U.gain|,|V.gain|))
             + max(|U.offset|,|V.offset|) * full_range
interleave = quant_step * (1 + order)
cumulative = interp + sharpen + gain_quant + interleave
```

其中 `quant_step = quant_range / (2^bit_depth - 1)`。归一化累积界在最终单通道偏差不可能超过满量程时截断为 `1.0`。该界是参数化保守估计，不是统计置信区间。

## 并发与资源边界

- 最大并行度默认 2，硬上限 16。
- 调度器按依赖就绪情况并行运行 Y/U/V 分支阶段。
- 每阶段按输入/输出平面估算缓冲需求；每工作线程有独立缓冲上限。
- 全局缓冲耗尽的阶段会阻塞排队，并记录 `buffer_exhausted_waiting` 事件；不会被静默丢弃。
- 单个阶段超过每线程上限返回 `E_RESOURCE`，并记录 `stage_exceeds_per_worker_buffer_cap`。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_META` | 元数据缺失/非法 |
| `E_PLANE` | 平面尺寸、位深、内容或路径不符 |
| `E_PARA` | 参数越界 |
| `E_DIFF` | 路径或阶段像素不一致 |
| `E_CACHE` | 缓存/快照结构错误 |
| `E_CANCELLED` | 在已持久化阶段边界取消 |
| `E_RESOURCE` | 并行度或缓冲硬限制 |
| `E_IO` | 文件或 SQLite I/O 错误 |
| `E_INVARIANT` | 内部一致性检查失败 |
