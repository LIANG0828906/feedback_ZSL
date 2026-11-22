# IMQ Workbench

面向游戏贴图与互动媒体素材的本地图像压缩质量评估工作台。项目包含确定性样本生成、分块正交变换压缩、量化误差观测、SSIM/PSNR/图元边缘保真度评估、SQLite 断点检查点、三路径一致性比对、命令行入口和 PySide6 桌面界面。

运行时不访问网络，不依赖容器、Pillow 或外部数据。实际验收环境为 WSL2/Ubuntu 上的 Python 3.11.18；代码同时提供 Windows 批处理启动脚本。

## 环境与安装

已验证版本：

- Python 3.11.18
- pip 26.3
- NumPy 2.3.4
- PySide6 6.9.3

Windows 首次准备：

```bat
setup_windows.bat
```

脚本会执行：

```bat
py -3.11 -m venv .venv
call .venv\Scripts\activate.bat
python -m pip install -r requirements.txt
python -m imq_workbench generate --output-dir artifacts\samples --preset full
python -m imq_workbench check
```

若需完全离线安装，在一台可联网机器上预取 wheel：

```bat
py -3.11 -m pip download -r requirements.txt -d wheelhouse
```

将 `wheelhouse` 随工作区复制到离线 Windows 机器后，`setup_windows.bat` 会自动使用：

```bat
python -m pip install --no-index --find-links wheelhouse -r requirements.txt
```

手动安装方式：

```bat
py -3.11 -m venv .venv
.venv\Scripts\activate
python -m pip install -r requirements.txt
```

## 启动与重试

桌面工作台：

```bat
run_workbench.bat
```

等价命令：

```bat
.venv\Scripts\python -m imq_workbench app --samples-dir artifacts\samples
```

命令行首次运行：

```bat
.venv\Scripts\python -m imq_workbench run ^
  --samples-dir artifacts\samples ^
  --sample mixed_rgb_2048 ^
  --block-size 32 --quant-step 8 --eval-window 7 --workers 4 ^
  --mode realtime --checkpoint artifacts\state\checkpoints.sqlite
```

暂停或退出后从已提交块续跑：

```bat
.venv\Scripts\python -m imq_workbench run ^
  --samples-dir artifacts\samples ^
  --sample mixed_rgb_2048 ^
  --block-size 32 --quant-step 8 --eval-window 7 --workers 4 ^
  --mode resume --checkpoint artifacts\state\checkpoints.sqlite
```

忽略旧检查点并一次性全量重算：

```bat
.venv\Scripts\python -m imq_workbench run ^
  --samples-dir artifacts\samples ^
  --sample mixed_rgb_2048 ^
  --block-size 32 --quant-step 8 --eval-window 7 --workers 4 ^
  --mode recompute --checkpoint artifacts\state\checkpoints.sqlite
```

合作式暂停验证：

```bat
python -m imq_workbench run ... --stop-after 40 --mode realtime
```

`--stop-after` 在达到提交数后请求暂停；当前正在执行的小批块会安全提交，然后退出返回码 `3`，并明确输出 `incomplete snapshot: no complete quality report is claimed`。

强制终止恢复验证钩子：

```bat
python -m imq_workbench run ... --crash-after 20 --no-report
```

该参数只用于验收：进程以 `os._exit(137)` 直接终止，SQLite 中只保留完整提交事务。之后用 `--mode resume` 继续即可。

完整验收：

```bat
verify_workbench.bat
```

## 功能说明

### 确定性样本

`generate` 使用固定种子生成 4 张完整样本：

| 样本 | 分辨率 | 通道 | 内容 |
|---|---:|---:|---|
| `gradient_gray_512` | 512×384 | 1 | 平滑渐变 + 圆形边界 |
| `texture_rgb_768` | 768×512 | 3 | 高频纹理 + 矩形边界 |
| `shapes_gray_1280` | 1280×720 | 1 | 混合纹理 + 圆/矩形边界 |
| `mixed_rgb_2048` | 2048×1152 | 3 | 数千像素级混合媒体 + 4 个解析图元 |

每张样本写入：

- `.png`：可显示测试图。
- `.baseline.npy`：原始无压缩 uint8 基准像素。
- `.truth.npz`：`edge_mask` 与每个图元独立的边界坐标数组。
- `manifest.json`：种子、尺寸、通道数、SHA-256、图元参数、边界像素数与文件索引。

### 压缩链路

1. 将图像按 `block_size` 切分，右下边块只保留真实像素。
2. 边块用对称填充补齐后执行正交 DCT-II。
3. 按 `quant_step` 做 `round(coefficient / step)` 量化和反量化。
4. 逆 DCT、裁剪边块、四舍五入并裁剪到 `[0, 255]`。
5. 逐块记录量化前后系数误差、重建像素误差、邻接块误差差值与块哈希。

重建结果的尺寸、通道数和 dtype 与输入严格一致。

### 评估链路

- `PSNR/MSE`：在真实像素上逐块累计，不复用补零像素。
- `SSIM`：窗口大小可配置的确定性均匀窗口 SSIM；块边界使用 `overlap_band=(window-1)//2` 的重叠边带，按块独占区域合并。
- 边缘保真度：在中心差分梯度域比较原图与重建图，并只在解析真值 `edge_mask` 像素上汇总。
- 误差热力图：逐块、逐像素通道平均平方误差，导出 `.npy` 与彩色 `.png`。

分块合并与整图单次计算的 MSE、PSNR、SSIM、边缘保真度在实测中差异小于 `1e-14`。

### 三路径一致性

- `realtime`：清空目标快照后从头处理。
- `resume`：加载内容哈希和算法版本均匹配的已提交块，只处理未提交块。
- `recompute`：忽略已提交块，清空后全量重算。

三条路径使用相同的块编号、相同的块内运算与相同提交顺序。`compare_pipeline_results` 会逐像素、逐地图、逐指标和逐提交序比较；不一致返回 `E_DIFF`，并给出首个不一致像素、块号和差异阶段。

### 检查点与状态

状态机只允许：

```text
pending -> processing -> committed
pending -> processing -> failed
failed  -> processing
```

非法迁移返回 `E_STATE` 且保持原状态。已提交块参数冻结；续跑时修改 `quant_step` 或评估窗口只影响未提交块。`block_size` 决定分块划分，若同一图像已有其他分块尺寸的已提交快照，`resume` 会返回 `E_PARA(field=block_size)`；需要明确改用 `recompute` 建立新划分。

SQLite 检查点位于 `artifacts/state/`：

- 每块一个 `BEGIN IMMEDIATE ... COMMIT` 事务。
- 每块保存量化系数、重建像素、评估切片、统计 JSON、输入块哈希、参数和 payload SHA-256。
- 进程内用 `RLock` 和单连接串行化写入。
- 启动时若整图内容哈希、算法版本、形状、网格、块输入哈希或任一 payload 校验失败，则删除该快照全部块并全量重算。
- 强制终止后只恢复已提交事务；未完成快照不生成最终质量报告。

### 并发与资源

- 工作线程池上限 8。
- `max_buffer_pixels` 校验每线程单块工作集，默认 8,000,000 像素。
- 线程安全重建缓存复用邻域重建，避免每块 3×3 邻域重复 DCT。
- `executor.map` 保持块序；检查点写入按块号稳定串行提交。

## 参数范围

| 参数 | 范围 |
|---|---|
| `block_size` | `4, 8, 16, 32, 64` |
| `quant_step` | `0.1 .. 512.0` |
| `evaluation_window` | 奇数 `3 .. 63` |
| `max_workers` | `1 .. 8` |
| 重叠边带 | `(evaluation_window - 1) // 2` |
| 边带约束 | 重叠边带不得大于 `block_size`，否则 `E_BAND` |

## 错误码

| 错误码 | 含义 | 关键字段 |
|---|---|---|
| `E_PARA` | 参数越界或类型错误 | `details.field` |
| `E_STATE` | 非法块状态迁移、冻结块重试、未完成报告导出 | `block_index`, `from`, `to`, `state` |
| `E_DIFF` | 三路径或自动比对不一致 | `stage`, `first_pixel`, `block_index` |
| `E_BAND` | 评估窗口需要的边带超过块可提供范围 | `required_band`, `max_band` |
| `E_DECODE` | 图像/真值损坏或不可解码 | `path`, `reason` |
| `E_BLOCK` | 单块内部异常，已隔离为该块失败 | `block_index` |
| `E_IO` | 工作区或检查点写入失败 | `path`, `reason` |

CLI 发生可恢复业务错误时返回码为 `2`；合作式暂停未完成返回 `3`；成功返回 `0`。

## 桌面界面

界面提供：

- 原图与重建图并排对照。
- 逐块误差热力图。
- 累计 PSNR、SSIM、边缘保真度曲线。
- 可点击的四色块状态矩阵。
- 单块几何、量化步长、系数误差、像素误差与边缘指标详情。
- Start、Pause、Resume、Recompute、Retry selected、Export report。

若 GUI 不可用，全部功能可通过 CLI 完成。

## 测试与实测结果

自动化测试：

```bat
.venv\Scripts\python -m unittest discover -v
```

验收矩阵：

```bat
.venv\Scripts\python -m imq_workbench verify
```

详细实测记录见 `VERIFICATION.md`，原始机器可读结果位于：

- `artifacts/verification_cli.json`
- `artifacts/large_run.json`
- `artifacts/large_resume.json`
- `artifacts/unittest.log`
- `artifacts/check.json`
- `artifacts/runs/verification/quality_report.json`
- `artifacts/runs/large_2048/quality_report.json`
- `artifacts/runs/large_2048_resume/quality_report.json`

## 测试样例失败排查

- 若提示 `no samples found`，先运行 `python -m imq_workbench generate --output-dir artifacts/samples --preset full`。
- 若 `resume` 提示 `checkpoint_invalidated_reason`，表示内容、版本或块 payload 已变化；本次会从零全量重算，不会混用旧块。
- 若 `E_BAND`，增大 `block_size` 或减小 `evaluation_window`。
- 若 GUI 插件不可用，使用任何 CLI 命令；界面不可用不影响压缩、评估、检查点和报告。
- 若工作区不可写，执行 `python -m imq_workbench check` 查看具体目录。
