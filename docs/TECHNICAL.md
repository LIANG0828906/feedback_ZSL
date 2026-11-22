# 技术设计与复现协议

## 1. 数据模型

`samples/dataset.json` 是场景索引。每个场景目录包含：

```text
metadata.json                 波段、曝光、细节层、光谱响应、白点、目标色域
truth.json                    真值偏移、色块期望 RGB、质量上下界
bands/band_XX.npy             量化线性辐射波段图
exposures/exposure_XX.npy     经相机响应编码的 12-bit 曝光帧
detail/detail_noisy.npy       16-bit 含噪低对比度细节层
truth/detail_clean.npy        仅用于质量评估的干净细节参考
preview/spectral_preview.png  线性光谱预览
```

`metadata.json` 关键字段：

- `pixel_size`：`[height, width]`。
- `bands[]`：索引、中心波长、FWHM、文件、dtype、位深、形状、SHA-256。
- `exposures`：位深、逆响应参数、各帧曝光时间与 SHA-256。
- `spectral`：400–700 nm、5 nm 采样的近似 CIE 1931 CMF、各波段高斯响应、积分所得 `xyz_weights`、源白点。
- `target`：目标线性 RGB 矩阵、目标白点、输出位深、声明色域意图。
- `metadata_version`：除自身外整个 metadata 的 canonical JSON SHA-256。

`truth.json` 关键字段：

- `registration_offsets_px`：每个波段相对参考波段的 `(dy, dx)` 真值偏移。
- `patches[]`：色块 bbox、期望 XYZ 与目标线性 RGB。
- `quality.max_residual_std`：噪声 ROI 的滤波残留标准差上界。
- `quality.min_edge_fidelity`：边缘 ROI 的边缘幅值余弦保真度下界。

所有样本数组由固定种子确定性生成。`--seed` 同时驱动场景内容、曝光噪声与细节噪声。

## 2. 输入校验顺序

1. JSON 可解码。
2. metadata 必需字段齐全。
3. `metadata_version` 自校验；与 truth、dataset index 的版本一致。
4. dataset index 的波段数、曝光数、波段/曝光/输出位深一致。
5. 波段尺寸等于 `pixel_size`，位深范围合法。
6. 文件存在、SHA-256 匹配、dtype/shape 匹配、量化值不越界。
7. 曝光时间有限、为正且严格递增。
8. 配准偏移数等于波段数，参考波段偏移为 `[0, 0]`。

任何损坏像素载荷都返回 `E_DECODE`，不会用零、邻域或默认值填充。

## 3. 处理链

### 3.1 配准

真值偏移表示波段内容相对参考波段的位移。对齐时使用负偏移：

```text
aligned[b] = scipy.ndimage.shift(band[b], -offset[b], order=O, mode="reflect")
```

`O` 可配置为 0–5。任一 `|dy|` 或 `|dx|` 超过 metadata 的 `max_registration_shift_px` 返回 `E_REG`，详情含 `band_pair` 与 `measured_offset`。对齐结果尺寸严格等于参考波段。

### 3.2 辐射线性化与曝光融合

曝光帧先按声明的黑电平/饱和电平归一化，再逆 gamma：

```text
encoded = (raw - black) / (saturation - black)
L_k = encoded^gamma * full_well / exposure_time_k
```

只使用未触黑/未饱和样本。每帧权重由 `exposure_weights` 提供，并乘以有效掩码：

```text
F = sum_k(w_k * valid_k * L_k) / sum_k(w_k * valid_k)
```

若某像素所有曝光均无效，确定性地选择偏离黑/饱和电平最远的帧，并记录 `fallback_pixel_count`，不静默丢弃。

### 3.3 引导滤波与增强

融合辐亮度 `F` 为引导层，含噪细节层 `D` 为输入：

```text
Q = guided_filter(guide=F, source=D, radius=r, eps=epsilon)
mean_Q = box_mean(Q, r)
delta = Q - mean_Q
E = Q + gain * sign(delta) * max(abs(delta) - threshold, 0)
```

引导滤波采用反射边界。每个分段使用 `halo = 3r + 4` 的 ROI，避免半径跨越分段边界。

质量定义：

- 残留标准差：`std(Q - clean_D)`，在 truth 的 `noise_roi` 内。
- 边缘保真度：干净细节与滤波细节的梯度幅值图余弦相似度，在 truth 的 `edge_roi` 内，范围 `[0, 1]`。

若不满足声明上下界，处理返回 `E_PARA` 并在 details 中记录实测值与界限。

### 3.4 多光谱到目标色域

对每个像元：

```text
XYZ = sum_b L_b * xyz_weights[b]
XYZ_adapted = Bradford(source_white -> target_white) @ XYZ
RGB = target_matrix @ XYZ_adapted
```

增强后的细节相对声明中性值调制 RGB。越界定义为任一线性通道 `< 0` 或 `> 1`。

色域压缩沿“当前颜色—等亮度中性色”连线向中性轴收缩，解析求交 RGB 立方体边界：

- `perceptual`：越界亮度先用全实数域平滑函数 `0.5 * (1 + y/(1+|y|))` 压缩到 `(0, 1)`，再收缩色度。
- `relative_colorimetric`：保持亮度；若亮度本身超出 `[0, 1]`，返回 `E_GAMUT`。

输出保存：

- `gamut_mask.npy`：原始越界像素集合。
- `boundary_rgb.npy`：每个像素对应的 RGB 立方体边界坐标。
- `out_of_gamut_boundary_coordinates.csv`：越界像素 `y,x` 与边界 RGB。
- `gamut_report.json`：意图、越界计数、比例、最大越界量。

最终 RGB 按 metadata 的目标位深量化，高于 8 bit 使用 `uint16` 存储但值域严格为 `0..2^depth-1`。

## 4. 分段状态机

合法迁移：

```text
pending -> processing -> committed
                      -> failed -> processing
```

`committed` 为终态。非法迁移返回 `E_STATE`，不修改当前状态。进度始终为“已提交段数/总段数”。

提交时锁定：

- 波段元数据快照与 `metadata_version`。
- 全部结果相关参数快照与 `params_version`。
- `algorithm_version`。

运行中修改参数只用于尚未提交段；已提交段数组不被改写。重启时若当前参数版本与快照不一致，整库作废并全量重算。

## 5. 检查点协议

每个场景目录：

```text
state.json
.lock
segments/seg_00000.npz
segments/seg_00001.npz
...
```

每次提交：

1. 对固定字段顺序的数组和版本/快照元数据计算 SHA-256。
2. 在临时文件写入压缩 NPZ，`fsync` 后 `os.replace`。
3. 更新 `state.json`，重新计算 manifest 自哈希。
4. 临时 JSON `fsync` 后原子替换。

线程锁与 Linux `flock` 保证同场景写入不交错。强制终止可能留下未进入 manifest 的孤儿段文件，但恢复只读取 manifest 中的已提交段。

恢复时校验：

- manifest 自哈希、schema、scene id、segment size。
- 每个已提交段的文件、数组形状/dtype、内容哈希。
- metadata、参数、算法版本。

任一项失败即把整个场景快照移入 `checkpoints/.invalidated/`，内存已恢复段清零，随后全量重算；不会混用部分旧段。

`state.json` 数组字段：

| 字段 | 含义 |
|---|---|
| `fused_radiance` | 融合辐亮度 |
| `cleaned_detail` | 引导滤波去噪细节 |
| `enhanced_detail` | 局部对比度增强结果 |
| `chroma_xyz` | 色度/XYZ 缓冲 |
| `linear_rgb` | 最终线性目标 RGB（已色域压缩） |
| `gamut_mask` | 原始越界掩码 |
| `boundary_rgb` | 色域边界坐标 |
| `gamut_excess` | 每像素最大越界量 |
| `synthetic_rgb` | 按声明位深量化后的合成图像 |

`segment_errors.json` 为每个分段记录噪声残留、边缘保真度、越界计数，以及该段覆盖的已知色块像素 `MAE/RMSE/最大绝对误差`。

## 6. 并发与像素预算

- `max_workers` 限制线程池上限。
- `max_pixels_per_thread` 限制单个分段 ROI 的像素数。
- 总预算为 `max_workers * max_pixels_per_thread`。
- 提交任务前先阻塞式预留像素，结果按分段索引顺序消费并提交；预算不足时等待，不丢弃任务。
- 分段函数只依赖只读全局辐亮度和独立 ROI，因此串行与并行输出严格相同。

## 7. 三路径与比对

- `full`：无检查点，一次性计算所有段。
- `realtime`：单工作线程逐段处理并提交。
- `resume`：先提交部分段，销毁引擎后从检查点恢复剩余段。

三条路径使用同一分段函数。比对字段：

```text
synthetic_rgb
fused_radiance
cleaned_detail
enhanced_detail
chroma_xyz
linear_rgb
gamut_mask
boundary_rgb
gamut_excess
filtering_quality.residual_std
filtering_quality.edge_fidelity
gamut.out_of_gamut_count
gamut.max_excess
```

数组使用 `np.array_equal` 严格相等。首个差异报告：

- 字段名
- `[y, x]` 坐标与通道
- 分段编号
- 左右值、形状或 dtype

同时写出 `comparison_*.json` 与可执行 `comparison_*.reproduce.sh`。

## 8. 异常注入

| 注入 | 实现 | 预期链路 |
|---|---|---|
| 坏段 | `--bad-segment N` | `E_SEG`；该段 failed，其他段可提交，重试成功 |
| 波段缺失 | 删除真实波段载荷文件 | `E_META`，字段指向 `bands[N]` |
| 曝光倒置 | 反转 metadata 曝光时间并合法重签版本 | `E_ORDER`，字段指向曝光帧 |
| 量化位深变更 | 修改位深但保持版本声明 | `E_VER` |

`scripts/run_acceptance.py` 会真实执行上述路径，包括子进程 `os._exit(91)` 后恢复，并将结果写入 `outputs/acceptance/acceptance_report.json` 与 `docs/VALIDATION.md`。

