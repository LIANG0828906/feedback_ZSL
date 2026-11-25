# QualityBench 技术说明

## 1. 数据流

1. `generator` 用固定种子绘制基准帧，记录图元框、水印框、低对比度纹理和采样点。
2. 生成器顺序执行参考算链，构造采样点期望像素、逐块指标区间和逐块归因真值。
3. `dataset.Load` 对工程/真值做 schema 与范围检查，并核对基准 PNG 内容哈希。
4. `pipeline` 按步骤优先、块内并行执行；每块每步形成独立检查点。
5. `quality` 用最终帧和全部中间帧计算指标与首贡献步骤。
6. `compare` 比较 run 间持久化像素；`verify` 只读块产物重算指标和归因。

真值由生成器参考构造产生，生产执行路径不使用真值，也不把生产输出回写为真值。

## 2. 算子与区域语义

所有算子先复制输入帧，然后仅对作用区域写回。作用区域省略时为整图。

- 噪声索引由 `hash(seed, step_index, x, y, channel)` 产生，不使用全局共享 PRNG，因此并发调度、断点恢复和全量重算无顺序依赖。
- 模糊使用全图坐标与图像边界 clamp，再按强度将原像素与模糊像素混合。
- 块效应在全局 8×8 网格上计算均值，不随分块边界移动。
- 色度偏移在 YCbCr 域修改 Cb/Cr。
- 过曝对目标区域做软截断和轻度去饱和。

块执行虽然只保留目标块，但求值时使用完整帧上下文。这样 `ApplyBlock` 与顺序全图执行逐像素一致。

## 3. 输入域与失效传播

每个 `StepTrace` 记录：

- `input_region`：真正可能读取的支撑域
- `action_region`：声明写回区域
- `output_region`：实际输出域
- 输入帧摘要与输出帧摘要

支撑半径：

- 普通逐像素算子：0
- 模糊：`1 + floor(strength*2)`，上限 3
- 块效应：最多 7 像素，覆盖完整对齐 8×8 单元

编辑后，从最早变化步骤开始维护 `dirty` 块集合：

1. 直接编辑的脏块为旧/新作用区域覆盖的块并集。
2. 后续步骤仅在“脏块存在”且“当前作用区域与该块相交”时重算。
3. 模糊/块效应还会把脏集传播到输入支撑域相交的相邻块。
4. 对作用区域外但上游已变化的块，执行脏块穿透复制。
5. 每个受影响块产生 `{step, block, reason}` 失效记录。
6. 最终只对被像素变化影响的块重算 `EvaluateBlock`。

## 4. 缓存键与回滚

```text
cache_key = SHA256(
  "qb-cache-v1",
  algorithm_version,
  step_index,
  input_pixel_hash,
  canonical(type,strength,region),
  upstream_cache_key
)
```

第 1 步的 `upstream_cache_key` 来自基准块内容哈希；后续步骤严格串联上一步键。算法版本也进入键，因此不同版本中间结果不会混用。

回滚不会拼接旧帧指针，而是切回目标链并重新执行内容寻址缓存路径。测试覆盖多轮“编辑 → 回滚 → 重放”，最终与直接全量构造相同链逐像素一致。

## 5. 质量指标与归因

对每个块计算：

- 噪声方差：亮度与 3×3 局部均值残差的平方均值。
- 边缘清晰度：亮度 Sobel 梯度绝对值均值。
- 色度偏移：相对基准块 Cb/Cr 均值变化的平均值。

归因逐步骤比较指标增量：

```text
score_t =
  0.62 * max(0, noise_t - noise_{t-1}) / (base_noise + eps)
+ 0.34 * max(0, edge_{t-1} - edge_t) / (base_edge + eps)
+ 0.26 * chroma_distance(prev_t, t) / 96
+ 0.22 * abs(luma_t - luma_{t-1}) / 48
```

首贡献步骤是链中第一个满足 `score_t / total_score > attribution_share` 的步骤。无显著贡献时归因到 `none`。

真值区间由生成器参考构造值乘/加声明容差形成；归因真值记录步骤、类别、份额及容差。独立验证器重算后比较区间和份额容差。

## 6. 状态机

块与步骤都使用同一合法迁移集合：

```text
pending    -> processing
processing -> committed | failed
failed     -> pending
committed  -> pending       # 显式失效/重算
```

非法迁移返回 `E_STATE`，携带 `state_before`、`state_after`、步骤和块，且不修改原状态。编辑时受影响块走 `committed → pending → processing → committed`。

## 7. SQLite 检查点

`runs` 表保存 run 元数据、链指纹、工程/真值指纹、算法版本、几何和状态。

`block_steps` 主键：

```text
(run_id, image_id, step, block_x, block_y)
```

每行保存：

- 压缩像素
- `cache_key`
- `input_hash`
- `output_hash`
- `output_digest`
- `chain_fingerprint`
- `algorithm_version`
- 状态、块几何和全局 `commit_seq`

运行完成后还按块写入 `block_manifests`：一条记录包含该块 `step=0..N` 的全部 `cache_key`、`input_hash`、`output_hash`、`output_digest`、链指纹和算法版本。manifest 的内容哈希也参与恢复校验。

存储层使用单连接、互斥和独立事务。执行器先收集并行结果，再按块坐标稳定排序提交；`commit_seq` 由事务内全局最大值递增。

恢复流程：

1. 比较 run 元数据、链指纹、工程/真值指纹、算法版本和几何。
2. 逐行解码像素，验证内容哈希、输出摘要、块网格和版本。
3. 检查步骤连续性。
4. 任一失败：删除该 run/image 的全部块并记录 `run_events`，随后全量重算。

## 8. 三条路径

- `live`：新 run，允许内存/SQLite 缓存命中。
- `resume`：先通过 `--stop-after` 中断，再由 `resume` 加载已完成块继续。
- `full`：新 run，禁止缓存读取，所有块重算。

三条路径最终都写同样的块布局。`compare` 按 `step → block_y → block_x` 比较像素和产物哈希；首个差异构造 `E_DIFF`，包含图像、步骤、块坐标与字段。

## 9. 独立验证器

`internal/verify` 只依赖：

- SQLite 中持久化的 `step=0..N` 分块像素
- 真值 JSON 及其指纹
- 指标/归因定义

它不导入也不调用 `pipeline`、`ops`、缓存或执行器。验证步骤：

1. 校验 run 元数据与真值指纹。
2. 从块产物重建每步完整帧。
3. 从分块产物重算三项指标。
4. 与真值区间比较。
5. 用真值中的步骤类型重算首贡献归因。
6. 校验采样点期望像素。

## 10. 并发与资源

- worker 数由 `--workers` 强制限制。
- 每任务所需全帧上下文缓冲有 `MaxThreadBufferBytes` 上限。
- 共享 `BufferPool` 有总字节预算；预算不足时任务阻塞等待。
- 每次缓冲等待记录步骤、块、原因、请求字节和实际等待时长。
- worker 队列背压也记录等待事件。
- 取消通过 context 传播；暂停在任务安全点阻塞，恢复后继续。

## 11. 诊断页

HTTP 端点：

- `GET /`：单页诊断 UI
- `GET /api/state`：运行、指标、归因、失效、缓存和路径状态
- `GET /api/image?image=...&step=...`：PNG 帧
- `POST /api/control?action=pause|resume|cancel`
- `POST /api/retry?image=...&step=...&x=...&y=...`
- `POST /api/start`

页面主运行完成后自动演示一次“修改强度 → 列出失效 → 回滚”，并可后台执行 `live/resume/full` 比较与独立验证。

