# 设计说明

## 1. 模块划分

| 模块 | 职责 |
|---|---|
| `volrender/binary.py` | 二进制容器编码/解码、校验和 |
| `volrender/scene.py` | 场景模型、矩阵/几何/嵌套/密度校验 |
| `volrender/config.py` | 不可变配置与预算降级序列校验 |
| `volrender/volume.py` | 介质边界事件、分段、自适应步进、散射/阴影 |
| `volrender/render.py` | 分块预检、线程池调度、组装、摘要、接缝真值 |
| `volrender/seams.py` | 整屏参考比较与接缝错误定位 |
| `volrender/incremental.py` | 受影响块计算、内容哈希复用、原子版本发布 |
| `volrender/snapshot.py` | 哈希链快照、原子写入、恢复校验 |
| `volrender/injection.py` | 加载/配置入口五类故障注入 |
| `volrender/pipeline.py` | 双精度预览、误差文件、原子版本目录 |
| `volrender/gui.py` | Tkinter 图形预览 |
| `volrender/samplegen.py` | 确定性样本生成器 |

## 2. 光线与体积积分

1. 对每个像素由 `K^-1` 和相机逆矩阵生成世界空间射线。
2. 对 AABB/球体求交，得到所有介质的进入/离开事件。
3. 事件按 `(t, 离开优先, 介质索引)` 排序，形成活动介质集合恒定的分段。
4. 每个分段使用整屏坐标相位和自适应步长采样密度场。
5. 每个样本计算活动介质的密度加权消光、散射和 HG 相位。
6. 方向光阴影使用固定 `shadow_samples` 个估计算子，光学深度转可见率。
7. 更新：

```text
T_sample = exp(-sigma_t * ds)
color += T * (1 - T_sample) * (sigma_s / sigma_t) * light * phase * visibility
T *= T_sample
```

8. 所有分段结束后加上 `T * background`。

阴影是固定预算估计器，不追求解析精确遮蔽；其采样数在预算预检中固定计入，因此不会破坏预算一致性。

## 3. 调度器

- 先将屏幕按配置切为 `TileSpec`，`index == seq`。
- 按 `seq` 串行执行所有块的预算预检并写轨迹；预检不会写颜色。
- 仅在所有块均通过预算检查后创建 `ThreadPoolExecutor(max_workers=config.max_workers)`。
- 提交顺序按 `seq` 稳定排序。
- 每块先渲染到局部数组，完成后再写入结果映射；异常块不会留下半写内容。
- 预算拒绝发生在任何块启动前，因此整次渲染不会写出颜色发布目录。

轨迹事件包括：

- `schedule_start`
- 每级 `budget_attempt`
- `wait_queue` / `reject_budget` / `accept`
- `submit` / `complete` / `tile_failed`
- 恢复时 `resume_skip`

## 4. 接缝策略

接缝不是通过额外边界像素重叠修补，而是通过全局确定性消除：

- 相位锚定整屏 `(x, y)`。
- 边界事件、分段顺序、密度插值和阴影算子完全由场景参数决定。
- 分块只决定“谁计算这些像素”，不改变像素的计算表达式。
- `render_reference()` 以单块覆盖整屏、按各块实际阶段映射重算真值。

正常结果逐位相等。若外部篡改或实现漂移导致超差，`E_SEAM_UNRESOLVED` 会报告坐标、分块值、真值、颜色/透射率差以及左右上下邻域值。

## 5. 精度路径

`float64` 路径直接累积。

`fixed` 路径使用可配置 `fixed_scale`（默认 `2^20`）的整数 Q 格式：

- 透射率与单步衰减量化到定点。
- 每个颜色通道使用整数乘除累积。
- 背景和透射率同样走定点。

误差上界按每像素累计操作数、光照/背景幅度和量化步长给出保守值：

```text
bound = 8 * (samples_used + 4) * amplitude / fixed_scale
```

该上界写入逐像素 `precision_error_bound.bin`，并与实际颜色及透射率误差一同统计。

## 6. 增量重渲

`Change` 支持：

- `camera`：全部块。
- `light`：全部块，因为所有阴影射线变化。
- `medium`：投影新旧介质包围盒，并沿 `-light * shadow_distance` 扫掠，以覆盖影响阴影的屏幕区域。
- `density`：由块 ID 找到新旧介质，按同样方式求受影响块。
- 未知变化或找不到介质：保守全量重渲并写原因。

未受影响块直接复用旧 `TileResult`，其颜色哈希必须保持不变。受影响集合仍走正常预算、并行、接缝流程；只有成功后整个 `PublishedVersion` 才在短锁内赋值为新对象。查询只会看到完整旧版本或完整新版本。

同一 `scene_id` 使用非阻塞场景锁；重复提交立即返回 `E_BUSY`。

## 7. 快照与恢复

每个已完成块写入：

- tile 几何、`seq`、阶段名/索引、步长
- 预检采样量、实际采样量
- 颜色哈希、透射率哈希
- 原始颜色/透射率载荷的 Base64
- `prev_hash`、`chain_hash`

`chain_hash = SHA256(prev_hash || canonical(entry_metadata))`，首项从 64 个 `0` 开始。快照头包含：

- 场景 ID、场景指纹、密度版本
- 配置摘要、完整预算/降级配置
- 精度模式、分块表、完成块索引、链头

写入使用同目录临时文件、`fsync`、`os.replace`，随后 `fsync` 目录。恢复时：

1. 选择文件名排序最新的 `*.snapshot.json`。
2. 校验版本、场景指纹、密度版本、配置摘要、精度。
3. 校验完成索引排序/唯一、块表长度、逐项必填字段。
4. 逐项重算 `prev_hash`、`chain_hash`、`chain_head`。
5. 校验载荷尺寸、颜色/透射率哈希、预算与预检量。
6. 将已完成 `TileResult` 传给调度器，轨迹记录 `resume_skip`，不会再次提交这些块。

最新快照损坏时直接以 `E_RECOVERY_INVALID` 中止，不静默回退，符合“断链或字段缺失”要求。

## 8. 异常注入

注入只在 `load_inputs()` 入口准备内存中的变异数据，再交给正常加载器复核：

- 损坏密度 offset
- 移除介质条目
- 相机矩阵写入 NaN
- 截断二进制
- 将预算改为 0

复核错误会被映射为五个独立业务码，并附 `injection_point`。CLI 在创建输出目录前调用加载入口，因此不会产生颜色缓冲。
