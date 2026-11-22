# 技术说明

## 1. 数据与坐标约定

生成器对无噪声基准图 `B(x,y)` 应用 Fourier 平移，得到第 `t` 帧：

```text
C_t(x,y) = B(x - dx_t, y - dy_t)
I_t = C_t + N(0, sigma_t)
```

`(dx_t, dy_t)` 是“移动帧内容相对参考帧”的位移，也是配准模块返回的符号约定。将移动帧校正到参考几何时应用反向平移 `(-dx_t, -dy_t)`。

参考帧可配置。若参考帧为 `r`，配准偏移表示 `shift_t - shift_r`，质量比较使用平移后的基准图 `B(x - dx_r, y - dy_r)`，避免改变参考帧后错误比较。

每帧像素最终统一为 C-contiguous `float32`。检查点保存的也是这一表示，恢复时不重新量化，因此不同路径可以逐字节比较。

## 2. 确定性样本

固定随机种子驱动以下内容：

1. 几何特征的轻微位置抖动、尺寸和亮度；
2. 每帧亚像素位移；
3. 每帧噪声基底与高斯噪声；
4. 每帧浮点数据。

基准图包含三个高斯亮斑和两条高斯截面线段。边缘与特征之间保留足够裕量，使 Fourier 循环平移不会把特征卷到图像另一侧。生成结果保存帧文件 SHA-256 和无噪声基准图 SHA-256，使“同种子同数据”可直接验证。

## 3. 加载校验

`load_sequence()` 严格检查：

- 必需元数据字段；
- `height`、`width`、`frame_count`、`reference_frame` 的范围；
- 帧文件数量与哈希数量；
- 真值偏移和噪声基底数量、数值有限性；
- 特征参数与每帧特征中心；
- 每帧 shape、dtype 和文件 SHA-256；
- 基准图 shape 与哈希；
- 帧路径不得逃逸序列根目录。

错误定位约定：

| 条件 | 错误码 | `field` 示例 |
|---|---|---|
| 元数据字段缺失/非法/NaN | `E_META` | `manifest.frame_count`, `truth.shifts` |
| 帧文件缺失、无法解码或哈希不符 | `E_DECODE` | `frame_file`, `frame_sha256` |

`frame` 尽量填具体帧号；序列级错误为 `None`。

## 4. 亚像素配准

实现位于 `registration.py`。

1. 对参考帧和移动帧应用相同窗函数：`none`、`hann`、`hamming` 或 `blackman`。
2. 去均值并计算互功率谱 `F_moving * conj(F_reference)`。
3. 使用幅度幂权重 `|R|^0.8`，避免纯相位相关放大低信噪比高频。
4. 逆 FFT 找到整数相关峰，并转换为有符号环绕位移。
5. 在峰附近用局部矩阵乘 DFT 计算 `upsample_factor` 倍精度样本。
6. 对局部 DFT 峰的横纵切片做抛物线细化，消除离散网格偏差。
7. 若任一轴位移绝对值超过 `max_shift`，抛出 `E_REG`，`details.actual_displacement` 给出实际估计。

验收中大规模组最大误差为 0.2049 px。精度阶梯 `1×/2×/4×/6×` 的平均残差单调下降。

## 5. 噪声估计与去噪

### 5.1 分块噪声

每个块内拼接横纵一阶差分：

```text
d = [I(y+1,x)-I(y,x), I(y,x+1)-I(y,x)]
sigma_block = 1.4826 * MAD(d) / sqrt(2)
```

对全图块噪声取第 20 百分位作为噪声基底，避免特征边缘主导估计。块中位数的一阶分位数作为背景。

### 5.2 自适应强度

去噪强度由测量到的 `sigma` 决定。Gaussian 滤波器空间宽度：

```text
scale = clip(0.88 + 0.045 * sigma, 0.55, 2.0)
spatial_sigma = clip(strength * scale, 0.35, 2.4)
```

Median 滤波器在噪声较高时自动扩大半径，半径仍受配置上限约束。Guided 滤波器的 epsilon 随噪声和强度变化。

### 5.3 质量指标

- **残留噪声标准差**：在排除特征邻域的背景像素上，对 `denoised - clean_reference` 取中位数和 MAD，抑制少量特征偏置。
- **特征边缘保真度**：在特征掩膜内比较去噪图与无噪声图的梯度，组合梯度方向余弦与梯度幅度保持率：

```text
fidelity = 0.5 * cosine + 0.5 * min(1, gradient_norm_ratio)
```

验收阈值在运行报告中显式声明，三组全部满足。

## 6. 特征提取与统计

检测阈值为：

```text
background + max(4.2 * sigma_noise, 0.07 * (p99.5 - background))
```

对亮区做确定性 8 连通域标记。每个连通域用高于阈值的像素强度加权得到中心，并由二阶协方差主轴得到：

- `kind`：`spot` 或 `line`；
- 权重中心；
- 面积、峰值、bbox；
- 方向角、伸长率；
- 平均强度。

每帧统计包含偏移、噪声、背景、阈值、各类型数量、中心均值与逐特征记录。序列统计汇总噪声范围、偏移范围、特征总数与质量汇总。

## 7. 预算调度

模式选择只依赖：

- 帧数；
- 单帧像素；
- 上采样因子；
- 时间预算和内存预算；
- 并行度和每线程缓冲数。

不读取实际耗时，因此同一输入在实时和离线路径产生相同决策。解析成本模型：

```text
full_seconds = frames * pixels * (reg_per_pixel + denoise_per_pixel) + fixed_overhead
full_memory  = result_frames + workers * 14 * frame_buffers + runtime_base
```

当全量模式不满足任一预算时，按 `2,3,4,5,6,8,10,12,16,24,32` 依次寻找可行降采样倍率。预览去噪配额为：

```text
ceil(frame_count * preview_denoise_fraction)
```

执行时按低分辨率噪声估计从高到低选择去噪帧，噪声相同时按帧号稳定排序。决策记录包含实际模式、倍率、配额、估计值、预算依据和逐帧决策；决策本身有稳定哈希。

验收中大规模组在 `5 s / 512 MB` 约束下选择 `8×` 预览、48 帧选择性去噪；解析估计 4.80 s，实测 3.19 s。

## 8. 状态机与提交边界

合法迁移：

```text
pending -> registered -> denoising -> committed
pending | registered | denoising -> failed
failed -> pending
```

- `committed` 无出边。
- 非法迁移抛 `E_STATE`，`details.current`、`details.attempted` 和允许目标可定位；当前状态保持不变。
- 提交时冻结 offset、去噪摘要、源内容哈希、参数版本和算法版本。
- 进度始终是 `committed / total`。

`LiveIncrementalSession.update_config()` 只在提交边界之间调用。它不触碰已提交记录；后续未提交帧采用新参数版本。一旦出现混合参数版本，该磁盘快照在重启时必须整体作废，不能继续拼接。

## 9. 检查点与原子性

每帧写一个 `frame_XXXXXX.npz`：

```text
offset
denoised
denoised_sha256
noise_sigma
background
stats_json
content_sha256
parameter_version
algorithm_version
sequence_fingerprint
sequence_id
frame_count
```

写入流程：

1. 在 `CheckpointStore` 的不可重入锁内确认帧号连续。
2. 写入同目录临时文件。
3. `flush + fsync` 文件。
4. `os.replace` 原子替换正式记录。
5. `fsync` 目录。
6. 原子更新 `snapshot.json` 头。

协调器只按帧号升序提交，因此并行完成顺序不会改变落盘顺序。中途强制终止最多留下未替换的临时文件；正式记录要么完整，要么不存在。

恢复时扫描连续记录并校验：

- 文件名和索引连续；
- shape、序列标识和序列指纹；
- 源内容 SHA-256；
- 去噪数组 SHA-256；
- 参数版本与算法版本；
- 快照头计数不低于实际记录覆盖范围。

任一项不匹配都会把整个目录重命名为 `.invalid-*` 隔离，再以空目录全量重算。不会保留可用前缀，也不存在旧帧与新帧混合恢复。

## 10. 三类一致性路径

### 全量

一次性对全部帧调度同一 `_process_frame()`，结果按帧号组装。

### 增量

逐帧推进状态机；每帧先持久化，再提交状态。`pause_after` 和事件只在提交边界停止。

### 恢复

新进程读取检查点记录，用保存的 `float32` 数组和标量恢复已提交帧，再处理剩余帧。未提交帧由同一纯函数重新计算。

由于以下条件同时成立：

- 帧处理不依赖全局随机数；
- 每帧输入相同；
- 配准、去噪、特征、统计全部确定性；
- 提交顺序固定；
- 恢复保存完整 `float32` 像素；

全量、增量、恢复和并行/单线程结果逐像素一致。

## 11. 差异定位

`compare_results()` 按以下顺序比较：

1. shape、帧数；
2. 偏移数组；
3. 去噪像素数组；
4. 特征中心及特征字段；
5. 统计字典。

像素差异在 C 顺序下定位首个位置，`E_DIVERGE.details` 包含：

- `frame`；
- `y`、`x`；
- `mismatch_count`；
- `max_abs_diff`；
- `mean_abs_diff`；

非像素字段则给出字段路径。

## 12. 并发资源边界

- 线程数由 `BudgetConfig.max_workers` 限制。
- 在途任务上限为 `max_workers * frames_per_worker`。
- 每个任务只持有一帧的工作数组和结果；共享参考帧只读。
- 工作线程只计算，不写检查点。
- 主协调线程按提交顺序写入唯一检查点存储。
- 每帧计算无共享可变 RNG 或缓存，因此并行度不改变结果。

## 13. 预览导出闸门

预览模式在低分辨率完成配准、噪声估计、选择性去噪和特征提取。导出时：

1. 重新以高精度全量处理；
2. 将高精度结果按同倍率块均值降采样；
3. 对预览中实际去噪的帧计算平均/最大绝对偏差；
4. 超过 `preview_mean_abs_limit` 时抛 `E_DIVERGE`；
5. 通过后只导出高精度像素、偏移、特征和统计。

预览结果不会被直接当作最终产物。

## 14. 复现验收

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest -q
.venv/bin/python -m sciseq acceptance \
  --samples samples \
  --output artifacts/acceptance.json
```

验收会重新生成缺失样本、运行三组四路径、检查质量阈值、执行精度阶梯、预览导出和错误码场景，并在 JSON 中记录环境、耗时、指标、阈值和决策哈希。

