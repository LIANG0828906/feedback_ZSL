# SpectraForge 多光谱 / HDR 合成工作台

SpectraForge 是一套完全本地运行的多光谱与高动态范围图像合成工作台，面向游戏与互动媒体素材。它包含：

- 固定种子、可重复的多光谱 HDR 样本生成器；
- 亚像素波段配准、逆相机响应、可靠度加权曝光融合；
- 从波段辐亮度到 XYZ、白点适配、目标 RGB 与色域映射的完整色度链；
- 可暂停、可退出、可在强制终止后恢复的分段处理与原子检查点；
- 实时处理、断点恢复、全量重算三路径逐像素一致性验证；
- PySide6 专业桌面界面，支持逐段检查、波形、色度示波器、差异热力图与导出。

![SpectraForge UI](artifacts/ui_workbench.png)

## 快速开始

### Windows 10/11

要求 Python 3.11 或更高版本，`python` / `py` 可在 PowerShell 中调用。

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
python generate_samples.py --out samples
python run_app.py
```

也可直接运行：

```powershell
.\setup_windows.ps1
```

### Linux / WSL

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
python generate_samples.py --out samples
python -m spectraforge gui
```

无显示器环境使用 Qt 离屏模式：

```bash
QT_QPA_PLATFORM=offscreen python capture_ui_demo.py
```

## 一键验收

```bash
python verify_acceptance.py
```

该命令依次执行：

1. 全部单元与端到端测试；
2. 实时 / 断点恢复 / 全量重算三路径验收；
3. `E_META`、`E_DIM`、`E_DECODE`、`E_PARA`、`E_STATE`、`E_GAMUT`、`E_DIVERGE` 错误码链路。

结果写入 `artifacts/acceptance/`。实际验证记录见 [VERIFICATION.md](VERIFICATION.md)。

## 项目结构

```text
spectraforge/
  imageio.py          校验和图像容器读写
  metadata.py         场景元数据校验与加载
  registration.py     亚像素配准与残差统计
  fusion.py           逆响应线性化与曝光融合
  color.py            XYZ 积分、Bradford 白点适配、色域映射
  tone.py             曲线、色调映射与显示编码
  pipeline.py         分段计算与确定性拼接
  state.py            四态分段状态机
  checkpoint.py       原子检查点、整快照校验、进程锁
  scheduler.py        线程池、缓冲预算、暂停与故障注入
  compare.py          无容差逐像素差异定位
  export.py           PNG / NPY / 统计 / 导出清单
  verification.py     三路径自动验收
  sample_generator.py 固定种子样本生成器
  ui/                 PySide6 专业工作台
samples/              已生成的确定性样本与故障样本
tests/                23 项自动化测试
artifacts/            实测报告、截图、导出样例
```

## 样本格式

`generate_samples.py` 使用固定种子 `424242` 生成 3 个场景。每个场景包括：

- 8 个波段：`400–700 nm` 光谱网格，每波段独立线性辐射图和量子化位深；
- 6 档曝光：按场景峰值与暗部自动确定曝光时间，写入逆响应前的传感器读数；
- `scene.json`：波段中心波长、带宽、曝光时间、白点、位深、像素尺寸、配准偏移、
  D50 源白点与 sRGB/D65 目标白点、相机响应；
- `truth.json`：8 个色块的解析绝对线性 sRGB、XYZ、亮度、峰值辐亮度与配准偏移真值；
- `catalog.json`：样本清单及三个故障样本的预期错误码。

图像使用自描述、无外部库依赖的 `MSI1` 容器：

```text
magic (8 B) | header length (u32 LE) | canonical JSON header | little-endian payload
```

头部包含尺寸、位深、角色、索引与载荷 SHA-256；加载时同时校验头部、载荷长度、位深范围和
载荷哈希。任何截断或篡改都会返回 `E_DECODE`。

故障样本：

- `samples/faults/missing_meta` → `E_META`
- `samples/faults/dim_mismatch` → `E_DIM`
- `samples/faults/corrupt_image` → `E_DECODE`

## 核心处理链

### 1. 配准

每个波段的偏移为 `(dx, dy)`。支持 `nearest`、`bilinear`、`bicubic`（Catmull-Rom）
三种确定性插值，边缘采用复制边界。对齐后输出：

- 全分辨率对齐波段立方体；
- 逐波段结构残差；
- 逐波段边界残差与均值统计。

配准残差可从 UI 的 `Pipeline` 页、`inspect --with-alignment` 输出和验证报告中查看。

### 2. 曝光线性化与融合

对曝光帧 `raw`：

```text
a = clip((raw / max_code - black) / (1 - black), 0, 1)
L = a^gamma / exposure_time
w = [4a(1-a)]^power * (exposure_time / max_exposure)^bias
```

权重在 `a = 0` 与 `a = 1` 处为零；低于欠曝阈值或高于饱和阈值的样本权重设为零。
所有有效权重为零时，选择响应最接近中灰的单帧作为确定性回退。曝光子集按索引排序后处理，
避免选择顺序改变浮点累加顺序。

过曝与欠曝像素显著降权；从只有最长曝光逐步加入更短曝光时，融合峰值单调上升，
并最终收敛到解析峰值。单帧路径可通过只勾选一档曝光直接对照。

### 3. 多光谱色度重建

生成器保存波段响应矩阵 `R` 与 CIE 1931 2° 匹配函数 `C`。运行时使用四象限基：

```text
A = R^T · Δλ
XYZ_basis = pinv(A)^T · C · Δλ
XYZ = Σ_b q_b · XYZ_basis[b]
```

其中 `q_b` 是对齐后的波段辐亮度。该过程等价于由波段响应积分进行最小二乘谱重建，
再对 CIE 匹配函数积分。

XYZ 经 Bradford 矩阵从 D50 适配到目标白点，再转换到 `sRGB`、`DisplayP3` 或 `Rec2020`。
色域判断在曝光无关的色度归一化空间进行：

```text
rgb_chroma = rgb / max(rgb)
mask = (rgb_chroma < 0) or (rgb_chroma > 1)
```

映射意图：

- `compress`：保持亮度方向，向灰轴去饱和直到进入目标色域；
- `clip`：硬裁剪到 `[0, 1]`；
- `strict`：存在越界时返回 `E_GAMUT`，包含像素坐标、通道、值和越界集合。

默认 `compress` 不终止处理，但越界像素数、通道数和前 64 个边界坐标始终可观测。

### 4. 合成与色调映射

最终线性 RGB 为：

```text
linear_rgb = mapped_chroma * fused_HDR_radiance
```

支持 Reinhard、ACES、Hable filmic 三类曲线，以及 EV、Key、白点、饱和度和显示 gamma。
显示值经量化得到 8-bit PNG；核心检查点保留 float32 线性 RGB，输入波段为 16-bit、曝光帧为 12-bit。

## 分段、状态与检查点

### 状态机

```text
PENDING -> PROCESSING -> COMMITTED
                     \-> FAILED -> PROCESSING
```

`COMMITTED` 为终态。非法迁移返回 `E_STATE`，原状态保持不变。每个分段记录自己的：

- 融合辐亮度缓冲；
- 绝对/归一化色度缓冲；
- 最终线性 RGB 与显示缓冲；
- 色域通道掩码；
- 内容哈希；
- 波段元数据版本与哈希；
- 参数版本与完整参数快照；
- 算法版本。

### 原子写入

每个分段先写入同目录临时文件，`flush + fsync` 后 `os.replace` 到固定文件名；随后在进程锁
与写锁内更新按索引排序的 `snapshot.json`。因此：

- 写入不会交错；
- 崩溃最多留下可忽略的孤立临时文件/未登记分段；
- 已登记分段一定有完整文件；
- 导出与恢复始终按稳定分段索引拼接。

### 整快照作废

恢复时校验整个快照，而不是逐段混用：

- 检查点 schema / format；
- 场景 ID、元数据版本与哈希；
- 算法版本；
- 分段布局；
- 参数快照哈希；
- 每个分段文件内容哈希与缓冲尺寸。

任一项不匹配会把整个快照移入 `checkpoints/_invalid/`，从零开始，不保留部分旧段。

### 参数锁定

参数版本按完整规范化参数对象哈希。已提交分段保存不可变参数快照；之后在 UI 或 CLI 修改
曝光子集、融合权重、色域意图或色调曲线，只会作用于尚未提交/失败重试的分段。已提交分段的
文件字节、内容哈希与参数快照均不变。如果要让新参数作用于全部段，必须显式执行
`REPROCESS ALL`，该操作会先归档旧快照。

### 并发与资源边界

- `workers` 限制线程池并行度；
- 每个分段启动前申请加权缓冲预算；
- 每线程缓冲上限不足时返回 `E_PARA` 并指出 `per_thread_buffer_mb`；
- 总预算耗尽时工作线程阻塞等待，不会静默丢段；
- 单段异常只把该段置为 `FAILED`，其余段继续并保持可重试。

## 三条路径逐像素一致

`verify` 自动执行：

1. **实时路径**：多线程分段处理并提交；
2. **断点恢复路径**：先处理一部分、关闭管理器、重新加载已提交段，再继续；
3. **全量重算路径**：独立工作目录、单线程、从零计算。

比较不使用 `np.allclose`，也不接受容差。逐阶段比较：

```text
radiance -> rgb_chroma -> linear_rgb -> display -> gamut_channel_mask
-> peak_radiance -> gamut_set
```

首个不一致会给出：

- 阶段名；
- 像素 `(x, y)`；
- 所属分段编号；
- 左右值；
- 峰值与色域集合是否一致。

不一致时 CLI 返回 `E_DIVERGE`。测试还使用一个 float32 ULP 的改动证明比较不会掩盖真实差异。

## 界面说明

- `Composite`：HDR 融合、最终合成、色度缓冲和选中分段对照。
- `Band Array`：8 个对齐波段与 HDR 融合概览。
- `Segment`：选中分段的融合、色度、合成缓冲和锁定统计。
- `Signal`：辐亮度直方图、色调响应曲线。
- `Chroma`：CIE 1931 色度示波器与目标色域三角形。
- `Difference`：逐像素差异热力图；完全一致显示 `EXACT MATCH`。
- 分段表：状态、参数版本、峰值、越界像素、内容哈希。
- 暂停只停止新任务调度；在途分段完成并原子提交后停止。
- 关闭窗口会先排空在途提交；强制终止后只能恢复已持久化且版本匹配的分段。

![Band array](artifacts/ui_band_array.png)

![Difference heatmap](artifacts/ui_difference.png)

## CLI

```bash
# 生成样本
python generate_samples.py --out samples --seed 424242

# 检查场景与解析真值
python -m spectraforge inspect --scene neon_forge_424242 --with-truth --with-alignment

# 处理/恢复
python -m spectraforge process --scene neon_forge_424242 --workspace runtime --workers 4

# 暂停后再次运行即恢复
python -m spectraforge process --scene neon_forge_424242 --workspace runtime --pause-after 4

# 三路径验收
python -m spectraforge verify --workspace runtime --workers 4

# 导出完整结果
python -m spectraforge export --workspace runtime --out runtime/exports/neon

# 错误码链路
python -m spectraforge check-errors --workspace runtime/errors

# 桌面应用
python -m spectraforge gui
```

完整导出包含 `display.png`、`radiance.npy`、`rgb_chroma.npy`、`linear_rgb.npy`、
`gamut_channel_mask.npy`、`aligned_bands.npy`、`alignment_report.json`、`stats.json`
与带 SHA-256 的 `export_manifest.json`。所有数组按稳定分段顺序拼接。

测试专用故障注入：

```bash
# 让分段 3 失败，随后再次 process 会重试
python -m spectraforge process --fail-segment 3

# 在提交 3 段后硬终止进程（退出码 86）
python -m spectraforge process --crash-after 3
```

## 错误码

| 代码 | 含义 | 关键详情 |
|---|---|---|
| `E_META` | 元数据缺失、损坏、哈希不匹配 | 文件/字段 |
| `E_DIM` | 波段/曝光尺寸或缓冲尺寸不一致 | 声明值、实际值 |
| `E_DECODE` | 图像损坏、截断、校验失败 | 文件、原因 |
| `E_PARA` | 参数越界 | `field`、值、允许范围 |
| `E_GAMUT` | 严格色域模式存在越界 | x、y、通道、值、集合 |
| `E_STATE` | 非法状态迁移或快照状态异常 | from/to、分段 |
| `E_DIVERGE` | 三路径或比对不一致 | 首个差异阶段、坐标、分段 |
| `E_INCOMPLETE` | 不完整快照被要求导出 | committed/total |
| `E_LOCK` | 同一快照被其他进程锁定 | 锁文件 |
| `E_IO` | 原子写入或文件系统操作失败 | 路径/原因 |
| `E_CANCEL` | 用户中断 | — |
| `E_INTERNAL` | 未预期内部异常或注入故障 | 原因 |

## 测试

```bash
QT_QPA_PLATFORM=offscreen python -W error::ResourceWarning -m unittest discover -s tests -v
```

测试覆盖：

- 固定种子字节级可重复；
- 三类损坏样本错误码；
- 色块解析真值、峰值误差与白点适配；
- 过曝/欠曝降权、曝光子集峰值单调；
- 四态迁移与状态保持；
- 已提交参数锁定；
- 单段失败隔离与重试；
- 严格色域越界的分段失败、全局坐标与通道诊断；
- 单段损坏导致整快照作废；
- 子进程硬终止后只恢复 2 个已提交分段；
- 线程池缓冲阻塞；
- 每线程缓冲上限 `E_PARA`；
- 实时/恢复/全量三路径完全一致；
- 波段分段模式三路径一致；
- float32 单 ULP 差异定位；
- 峰值与色域集合精确比较；
- 完整导出、顺序稳定、哈希验证、不完整导出拒绝；
- PySide6 离屏 UI 加载、处理、分段查看与截图。

## 运行说明与边界

- 不依赖 Docker、网络或外部数据集。
- 核心与 CLI 仅需 `numpy`；桌面界面需要 `PySide6`。
- Windows 使用 `msvcrt` 文件锁，Linux/WSL 使用 `fcntl` 文件锁。
- 强制终止后若仍有未提交分段，导出会明确返回 `E_INCOMPLETE`；不会宣称完整导出。
- 色域严格模式会让包含越界像素的分段失败；默认 `compress` 会保留可观测的越界集合。
- 波段分段模式保存每段的 HDR 融合缓冲与部分色度缓冲；最终由全部波段稳定求和，并使用最后提交波段锁定的参数快照执行一次色域映射与色调映射。
