# PanoWorkbench 技术说明

## 1. 设计目标

工作台围绕“可复现、可在任意提交边界重启、三条路径像素一致”设计。算法实现不是一次性脚本：

- 所有随机过程显式定种
- 图像对提交顺序固定
- RANSAC 使用自实现确定性流程
- OpenCV 工作线程数固定为 1
- 检查点整快照原子替换，不混用不同参数版本的部分结果
- 全景融合顺序固定，金字塔运算逐层确定

## 2. 数据与真值

生成器先造一张高纹理合成主场景，再为每个视图定义：

```text
H_i : 图像 i 局部坐标 -> 基准图像坐标
```

局部图像由主场景经过 `H_i^-1` 采样得到，并施加固定种子生成的增益/偏置。基准图 `H_0 = I`。

理论画布通过所有图像四角在基准坐标系中的并集计算：

```text
origin_x = floor(min_x)
origin_y = floor(min_y)
width    = ceil(max_x) - origin_x
height   = ceil(max_y) - origin_y
```

管线使用估计的全局单应计算同一几何量；若与真值画布差不超过配置的 `canvas_reconcile_px`，将输出尺寸对齐到理论尺寸，并在报告中标记 `canvas_reconciled`，否则返回 `E_HOMO`。

## 3. 特征与匹配

- 检测器：SIFT
- 可配置：`nfeatures`、`nOctaveLayers`、`contrastThreshold`、`edgeThreshold`
- 匹配：双向 `kNN(k=2)`
- 比率测试：正反两个方向都要通过
- 互最近邻：正向最近邻与反向最近邻必须指向彼此

匹配数低于 `min_matches` 时抛出 `E_MATCH`。该异常被图像对边界捕获，状态置为 `failed`，不会以空匹配继续。

## 4. 确定性 RANSAC

每个图像对使用以下内容的 SHA-256 派生 RNG种子：

```text
pair_id | dataset_content_hash | consensus_threshold | max_iterations
```

为避免“收紧阈值后重新拟合导致残差统计反弹”，RANSAC 共识模型使用独立的
`consensus_threshold`（默认 5 px）生成并固定；用户配置的
`reproj_threshold` 只用于最终内点判定。对固定模型而言，阈值收紧只会从
内点集合中移除高残差样本，因此内点数和残差中位数单调不增。

流程：

1. 从匹配中无放回抽取 4 对点。
2. 优先用 OpenCV C++ 最小样本求解；退化样本被跳过。
3. 使用正向重投影误差寻找内点。
4. 以“内点数优先，其次内点残差中位数”为模型排序。
5. 用最终内点做归一化 DLT 重拟合。
6. 计算内点率、残差中位数、P95、参数协方差条件数。
7. 与真值比较图像四角最大位移，记录 `truth_corner_error` 与 `truth_ok`。

协方差条件数来自重投影方程雅可比：

```text
sigma^2 = sum(r_i^2) / (2N - 8)
Cov(H)  = sigma^2 * pinv(J^T J)
condition = cond(Cov(H))
```

检查点只保存至多 96 个确定性内点样本用于热力图，统计值仍来自全部内点。

## 5. 全局配准图

每个已提交图像对提供一条边：

```text
H_a_to_b : 图像 a -> 图像 b
```

全局配准从基准图出发做最短边数 BFS。到达节点 `v` 时：

```text
H_v_to_base = H_parent_to_base @ H_v_to_parent
```

直接基准图边优先，因为它们通常是精度最高、漂移最小的路径。某些非必要边失败时，只要 BFS 仍覆盖全部图像，全景可继续；否则全景状态进入 `failed`。

## 6. 规模策略

工作量指标：

```text
work_units_mp = image_count * average_megapixels
```

自动规则：

```text
full       if image_count <= small_max_images
           or work_units_mp <= work_unit_threshold_mp
restricted otherwise
```

全对模式选择全部 `N(N-1)/2` 个图像对。

受限模式：

1. 将每张图缩到最长边 180 像素，提取 180 个 ORB 描述子。
2. 对所有图像对做 Hamming 距离交叉匹配，距离 ≤72 的匹配数作为重叠分数。
3. 强制加入所有基准图边和相邻采集序号边。
4. 按重叠分数补充候选，每图最多 `restricted_neighbor_links` 条额外连接。
5. 候选总数上限为 `min(max_restricted_pairs, 2N)`。

报告中的 `reason`、`actual_feature_budget`、`candidate_pair_count`、`coarse_screen_used` 与 `coarse_scores_considered` 使取舍可解释。

选择的模式、实际预算和候选集合均进入 `plan_fingerprint`，再与完整参数和算法版本一起生成 `parameter_version`。

## 7. 曝光、缝合与融合

### 曝光

以基准图重叠区为参考，对每张图做稳健线性拟合：

```text
base ≈ gain * image + offset
```

为避免图像二次重采样造成的对比度衰减偏置，基准参考会先映射到当前图像
坐标系再映射回画布，使两侧经过相同的插值路径；拟合使用非饱和的 BGR
三通道样本，单通道裁剪不会污染灰度关系。

先最小二乘，再按残差 MAD 迭代剔除离群像素。增益或偏置超出配置范围返回 `E_PARA`；严重溢出返回 `E_CLIP`。估算模型记录每个图像的重叠像素数、拟合 RMS、被裁像素数与最大溢出量。

### 缝合线

按插入顺序把基准图放到全景，其余图像逐个加入：

1. 求当前覆盖与新图覆盖的重叠区域。
2. 构造颜色差 + 梯度差代价图。
3. 重叠区较宽时，用逐行动态规划求竖向最小代价路径；否则求横向路径。
4. 新图拥有路径一侧的像素；另一侧保留既有归属。
5. 若空间连通性导致动态规划无有限路径，使用确定性直线回退，保证新图不会悄悄消失。

### 多分辨率融合

- 图像金字塔与归属掩码金字塔同时下采样。
- 每层累加 `Laplacian(image) * Gaussian(mask)` 与权重。
- 从最粗层向上重建并归一化。
- 融合后，对原始覆盖计数为 1 的像素强制复制曝光校正后的单图值，保证非重叠区不被融合改写。
- 接缝梯度 p95 超过配置门限时，全景合成失败并返回 `E_CLIP`。

## 8. 状态机

图像对：

```text
pending -> registering -> committed
                       -> failed -> registering (retry)
committed -> (terminal)
```

全景：

```text
pending -> compositing -> committed
                       -> failed -> compositing (retry)
committed -> (terminal)
```

非法迁移抛出 `E_STATE`，对象状态保持原值。`PairResult` 为 frozen dataclass；一旦图像对进入 `committed`，后续没有合法迁移可重估其单应。

## 9. 检查点与强制终止

检查点是一个完整 JSON 快照，包含：

- schema/algorithm/parameter 版本
- 数据集内容哈希
- 策略计划指纹
- 序列号
- 按 `pair_id` 排序的已提交图像对
- 已完成全景摘要

提交流程：

1. 主线程按固定图像对顺序取得 worker 结果。
2. 线程锁 + 文件 advisory lock 串行化写入。
3. 写临时文件，`fsync`，`os.replace` 原子替换。
4. POSIX 下同步目录元数据。

重启验证：

1. 顶层 schema、算法、参数、数据集哈希、计划指纹任一不匹配，整个快照作废。
2. 每个图像对再校验内容哈希、参数版本、算法版本、数值有限性与内点计数一致性。
3. 任一图像对失败，全部已恢复对清空并全量重算，不允许部分混用。

强制终止语义：

- 每次运行开始清除旧的全景/报告产物。
- 立即写 `run_state.json = running` 和 `report.json status=running`。
- 只有正常完成才写 `completed`。
- 强制终止后最多只存在 `running` 状态与已提交检查点，不会把旧全景报告错标为本次完成。

## 10. 三路径一致性

三条路径：

1. 实时逐对提交并完成融合
2. 暂停在提交边界，新建管线实例从磁盘恢复后续接
3. `resume=False` 忽略检查点，全量重算

一致性依赖：

- 同一数据集内容哈希
- 同一参数/计划版本
- 同一 RANSAC 种子
- 同一提交顺序
- 单线程 OpenCV 运算
- 固定排序的融合顺序
- 所有运行路径中不存在失败图像对

比较器同时读取：

- `panorama.png` 像素
- `report.json` 规范化内容
- `owner.npz` 像素归属

发现差异时返回：

- 首个差异像素 `(x, y)`
- 两侧 BGR 值
- `owner.npz` 中该像素所属图像
- 报告中该图像的 `registration_pair_id`

## 11. 并发边界

- 图像对匹配/RANSAC 可在线程池并行，最大线程数由 `max_workers` 限制。
- 检查点只在主线程、按规范顺序写入；worker 不直接写盘。
- 融合缓存使用 LRU，最多缓存 `max_cached_images` 张画布尺寸图像。
- 同时在途图像对数量由 `max_buffered_pairs` 限制。
- 内部单对异常被转换为该对 `failed`，不污染其他结果。

## 12. 复现实验

```bash
pytest -q
python -m panoworkbench verify --dataset data/samples/large \
  --output-root artifacts/verification-large
python -m panoworkbench compare-modes --dataset data/samples/large \
  --output-root artifacts/mode-comparison-large --mode-tolerance 3
```

实测结果见根目录 `README.md` 与 `artifacts/*/verification.json`、`artifacts/mode-comparison-large/mode_comparison.json`。
