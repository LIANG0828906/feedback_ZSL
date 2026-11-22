# 技术设计

## 1. 设计目标

系统将“分割结果”与“规模策略、块阶段提交、版本身份”绑定成一个可诊断工作流。核心约束如下：

1. 相同图像、参数、预算、算法版本必须得到相同掩码与相同规模判定。
2. 精细与降采样路径的差异必须可测量、可解释、可复现。
3. 只有已原子持久化且版本完全匹配的块阶段可以恢复。
4. 几何统计与轮廓必须基于最终全分辨率标签掩码，不允许从降采样中间量反推。

## 2. 样本与真值

`samplegen.py` 使用 `numpy.random.default_rng(seed)`，每个样本的随机序列只依赖样本 seed。解析图元只使用椭圆、矩形、多边形，按固定 z 序栅格化：

1. 先生成解析背景与低幅噪声。
2. 逐图元生成布尔掩码，写入颜色和 `uint16` 标签。
3. 后绘制图元覆盖先绘制图元，因此真值统计反映“可见区域”。
4. 对噪声场景在指定标签内嵌入局部散斑。
5. 从最终像素标签计算重心、面积、外接框，同时保留图元解析参数。

每个样本目录包含：

```text
manifest.json
image.npy
truth_mask.npy
```

`manifest.json` 中声明 `width`、`height`、`channels`、`dtype`、`region_count`、每区域统计、文件哈希、场景、seed 与推荐参数。

## 3. 加载校验

`validation.py` 的检查顺序：

1. 清单存在且可解析，否则 `E_DECODE`，`details.file` 指向清单。
2. 图像和真值文件存在且 `np.load(..., allow_pickle=False)` 成功，否则 `E_DECODE`。
3. 图像 `ndim / dtype / width / height / channels` 与声明一致，否则 `E_META`，`field` 指向具体字段。
4. 真值掩码为二维整数数组且尺寸等于图像，否则 `E_META`。
5. 内容哈希一致，否则 `E_DECODE`。
6. 真值标签集合、区域数量、清单区域列表与区域面积一致，否则 `E_META`。

错误对象统一为 `BusinessError(code, message, field, details)`。

## 4. 区域生长

### 4.1 特征

- 颜色距离：像素 RGB 与种子 RGB 的欧氏距离。
- 梯度：RGB 三通道中心差分的逐像素最大幅值。
- 背景连通域：从图像边界开始，以局部相邻颜色差进行边缘感知洪泛，用于排除渐变背景候选种子。
- 自动种子：在非背景、与边界中位颜色差异足够且低梯度的网格样本上做颜色量化聚类；按簇像素数稳定排序，`expected_regions` 存在时取前 N 个稳定颜色簇。

### 4.2 生长判定

对每个种子：

```text
color_ok  = distance(pixel, seed_color) <= color_tolerance
grad_ok   = gradient(pixel) <= gradient_threshold
confidence = 0.72 * (1 - dist / tolerance)
           + 0.28 * (1 - gradient / gradient_threshold)
accepted  = color_ok AND grad_ok AND confidence >= confidence_threshold
```

使用固定邻居顺序 `上、左、右、下` 的 BFS，并只写入尚未分配的标签。判定本身不依赖访问路径，因此结果不依赖线程标识或调度顺序。

### 4.3 后处理

对每个标签独立执行：

1. 二值开运算（腐蚀后膨胀）去除细小毛刺。
2. 二值闭运算（膨胀后腐蚀）填补小孔。
3. 小于 `min_area` 的区域被丢弃。

颜色容差、梯度阈值、置信度阈值、结构元素尺寸与最小面积均有范围校验，越界抛出 `E_PARA` 并给出字段。

## 5. 降采样路径

降采样使用固定块的逐通道中位数，避免均值把边界颜色混入目标。生长在 `1/factor` 分辨率完成，标签用确定性最近邻映射回原尺寸，再执行全分辨率形态学清理。

因此：

- 生长阶段受控于降采样后的像素数。
- 最终掩码、几何统计、轮廓均在原尺寸最终标签上计算。
- 双路径对照不会从降采样标签直接反推面积、重心或外接框。

## 6. 几何统计、轮廓与匹配

`geometry.py`：

- 区域面积：`count_nonzero(labels == region_id)`。
- 重心：区域内所有前景像素坐标的算术平均。
- 外接框：闭区间边界转换后的 `x, y, w, h`。
- 轮廓：Moore 邻域边界跟踪，输出首尾相同的闭合折线。
- 真值对照：对真值/预测区域构建 `1 - mask_IoU` 成本矩阵，使用矩形匈牙利算法求全局最优一一匹配。
- 数量不一致：立即抛出 `E_REGION`，同时返回：

```json
{
  "extra_regions": [{"region_id": 9, "area": 42}],
  "missing_regions": [{"region_id": 1, "area": 820}]
}
```

尺度一致性匹配同样使用最优匹配。逐区域计算：

- 重心欧氏偏差（px）
- 面积相对误差
- 外接矩形 IoU

超过 `ScaleTolerance` 时抛出 `E_SCALE`，`details.violations` 包含区域编号和三项实测值。

## 7. 规模策略

`budget.py` 使用固定、可审计的估算模型：

```text
精细时间 ≈ pixels × 7.8e-6 × (1 + 0.025 × (regions - 1))
精细内存 ≈ pixels × 48 bytes
降采样时间 ≈ ceil(pixels / f²) × 8.2e-6 × overhead + pixels × 1.6e-6
降采样内存 ≈ (pixels × 7 bytes + ceil(pixels / f²) × 54 bytes)
```

判定按固定顺序测试 `f = 1, 2, 4, 8, 16`，选择第一个同时满足时间与内存预算的倍率。`ModeDecision` 记录：

- `mode`
- `downsample_factor`
- `block_size`
- 预测时间/内存
- 每个候选倍率及是否满足预算
- 人可读判定依据
- 降级原因与影响范围

若 16× 仍不满足预算，返回 `E_BUDGET`，不会静默超预算运行。

## 8. 块状态机

每块有三个阶段：`growth → cleanup → statistics`；每阶段四态：

```text
pending → processing → committed
                     ↘ failed → processing (retry)
```

其他迁移均返回 `E_STATE`，状态保持不变。清理阶段开始前强制要求生长阶段已提交，统计阶段同理。

`BlockStateMachine` 提供：

- `progress()`：已提交阶段数 / 总阶段数。
- `snapshot()`：用于 UI 的块阶段矩阵与失效标记。
- `invalidate_uncommitted()`：冻结已提交阶段，返回仍有未提交阶段的块清单。
- `failed_keys()`：失败阶段列表。

## 9. 检查点与恢复

### 9.1 身份字段

每个快照和每个阶段文件均绑定：

- `sample_id`
- `image_hash`
- `parameter_version`
- `algorithm_version`
- `budget_version`
- `mode`
- `factor`
- `block_size`
- `block_count`

`parameter_version` 是参数与算法版本的规范化 JSON SHA-256。快照目录以 `sample_id` 的哈希命名，因此同一样本更换图像内容仍能发现 `image_hash` 不匹配。

### 9.2 原子写入

每次块阶段提交：

1. 检查阶段载荷不超过单块缓冲上限。
2. 计算载荷 SHA-256。
3. 获取线程锁与 `fcntl` 文件锁。
4. 在同目录写临时 `.npz`，包含载荷、哈希、身份字段、块、阶段、序列号。
5. `flush + fsync` 后 `os.replace`。
6. `fsync` 快照目录。
7. 扫描并重验全部阶段文件，原子更新 `index.json`。

管道按块行优先、阶段固定顺序提交，因此序列号与块序/阶段序稳定。

### 9.3 恢复规则

- 快照元数据任一身份字段不匹配：删除整个快照并重建。
- 任一阶段文件损坏、载荷哈希错误、身份字段错误、块阶段链断裂或重复：整个快照作废。
- 完整快照：直接拼装全分辨率生长/清理标签，重新从最终掩码计算统计与报告。
- 部分快照：重算以验证版本匹配，只恢复与重算载荷逐字节一致的阶段；不一致则整体作废。
- 用户取消或进程被强制终止：只保留已持久化阶段。重启后可从这些阶段继续，但不承诺存在完整报告。

## 10. 并行资源边界与失败隔离

- `ThreadPoolExecutor(max_workers=budget.max_workers)` 并行计算每个块的阶段载荷。
- 提交循环按行优先、阶段固定顺序串行推进。
- 检查点写入由共享存储对象内的锁与文件锁串行化。
- 单块阶段异常只将该块该阶段置为 `failed`，后续阶段不提交；其他块继续。
- 重试时已提交阶段从检查点恢复并冻结，不因失败块被污染。

## 11. 桌面诊断

PyQt5 工作台在独立 `QThread` 中运行核心流水线，主线程仅处理绘制与状态显示。展示内容：

- 原图
- 生长初始标签掩码
- 最终标签掩码叠加
- 真值对照的逐区域偏差热力图
- 块 × 阶段状态矩阵
- 自动/强制模式、倍率、块大小、预测与判定依据

支持块行选择、故障注入、重试、暂停、取消，以及导出掩码、初始掩码、叠加图、热力图和 JSON 报告。
