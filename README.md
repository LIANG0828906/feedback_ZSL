# PanoWorkbench

面向游戏场景截图与多媒体素材的确定性图像拼接/配准桌面工作台。项目从本地生成器造出的局部视图出发，完成：

- 数据加载与结构校验
- SIFT 特征检测、双向最近邻与比率匹配
- 确定性 RANSAC 单应估计与质量统计
- 按规模自动选择全对匹配或受限候选匹配
- 全局曝光补偿、最小代价缝合线、多分辨率融合
- 图像对/全景分层状态机
- 原子检查点、暂停/取消/断点续接
- 实时、续接、全量重算三条路径逐像素一致性验证
- PyQt5 GUI 与命令行（CLI）双入口

不依赖网络、真实图库、Docker 或其他容器环境。

## 环境

- Python 3.10+
- NumPy、OpenCV、PyQt5、pytest
- 当前实测环境：CPython 3.14.4、NumPy 2.5.5、OpenCV 5.1.0、PyQt5 5.15.11

Windows PowerShell：

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
pip install -r requirements.txt
python -m panoworkbench generate --output data\samples --seed 20261123 --sets all
python -m panoworkbench gui --dataset data\samples\large
```

也可双击 `run_gui.bat`，或执行：

```bat
run_gui.bat data\samples\large
```

Linux/WSL：

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
python -m panoworkbench generate --output data/samples --seed 20261123 --sets all
python -m panoworkbench gui --dataset data/samples/large
```

## 快速验证

```bash
pytest -q

# 三路径一致性：实时处理、暂停后重启续接、一次性全量重算
python -m panoworkbench verify \
  --dataset data/samples/large \
  --output-root artifacts/verification-large

# 同一集合的全对模式与受限模式取舍对比
python -m panoworkbench compare-modes \
  --dataset data/samples/large \
  --output-root artifacts/mode-comparison-large \
  --mode-tolerance 3
```

`verify` 会核对：

- 暂停时存在已提交图像对检查点，且尚无可声称完成的全景
- 续接仅恢复内容哈希、参数版本、算法版本全部匹配的图像对
- 三条路径的全景 PNG 逐像素一致
- 三条路径的配准报告（去除输出路径等易变字段后）完全一致
- 若不一致，报告首个不一致像素坐标、两侧 BGR 值及所属图像对编号

## 样本集

`generate` 使用固定种子生成 5 组数据：

| 集合 | 图像数 | 单图尺寸 | 理论画布 | 说明 |
|---|---:|---:|---:|---|
| tiny | 3 | 560×420 | 692×499 | 最小回归集 |
| small | 6 | 720×540 | 892×624 | 小规模全对模式 |
| medium | 12 | 760×570 | 997×773 | 中等规模 |
| large | 24 | 640×480 | 898×737 | 大规模受限候选模式 |
| highres | 4 | 1800×1200 | 2067×1467 | 数千像素级画布 |

每个集合包含：

```text
dataset/
  dataset.json
  images/
    view_000.png
    ...
```

`dataset.json` 中的关键真值：

- 每张图 `homography_to_base`：局部图像坐标到基准图像坐标的精确 3×3 单应
- `overlap_with_base`：重叠区对应点坐标
- `canvas`：理论全景宽、高与原点
- 每张图尺寸、通道数、SHA-256、合成曝光与旋转/尺度参数

生成器使用同一合成主场景，通过已知单应采样出带平移、旋转、轻微透视和亮度差异的局部视图。所有内容由固定随机种子确定。

## CLI

```bash
python -m panoworkbench --version
python -m panoworkbench generate --output data/samples --seed 20261123 --sets all
python -m panoworkbench validate --dataset data/samples/small

python -m panoworkbench run \
  --dataset data/samples/large \
  --output output/large \
  --mode auto \
  --workers 4 \
  --cache-images 8

python -m panoworkbench run \
  --dataset data/samples/large \
  --output output/large \
  --pause-after 3

# 重启续接：同一输出目录、同一参数版本
python -m panoworkbench run \
  --dataset data/samples/large \
  --output output/large
```

主要参数：

- `--max-features`：SIFT 特征数量上限
- `--octave-layers`：尺度层数
- `--contrast-threshold`：响应阈值
- `--ratio-test`：双向比率测试阈值
- `--min-matches`：图像对最低匹配数，低于此值返回 `E_MATCH`
- `--reproj-threshold`：RANSAC 重投影阈值
- `--consensus-threshold`：固定共识模型包络；最终内点仍按 `--reproj-threshold` 裁剪
- `--ransac-iterations`：RANSAC 最大迭代数
- `--min-inliers`：最低内点数，低于此值返回 `E_HOMO`
- `--mode auto|full|restricted`
- `--workers`、`--cache-images`、`--buffer-pairs`：并行度与缓冲上限
- `--no-resume`：忽略本地检查点并全量重算
- `--cancel-after`：指定秒数后发出取消请求

## GUI

启动：

```bash
python -m panoworkbench gui --dataset data/samples/large
```

界面功能：

- 加载并校验本地数据集
- 调整特征预算、尺度层数、响应阈值、比率、RANSAC 阈值/迭代、并行度
- 逐对表格展示待配准/配准中/已提交/失败状态
- 特征匹配连线、配准叠加、缝合线示意、累计配准误差热力图
- Pause 在当前图像对提交边界暂停
- Cancel 取消但不把未持久化结果声明为已提交
- Retry failed 从已提交检查点继续并重试失败对

![GUI](artifacts/gui_screenshot.png)

## 算法取舍

自动策略使用 `work_units = 图像数 × 平均百万像素` 衡量工作量：

```text
if 图像数 <= small_max_images or work_units <= work_unit_threshold_mp:
    高特征预算 + 全对匹配
else:
    低特征预算 + 重叠候选筛选
```

受限模式：

- 强制保留所有“基准图—其他图”候选，保证全局连通
- 保留相邻采集序号边作为连通骨架
- 对全对图像计算轻量 ORB 重叠分数
- 按分数补充候选，候选数上限为 `min(max_restricted_pairs, 2N)`

每个报告记录：

- 实际模式与选择原因
- 实际特征预算
- 候选图像对数量
- 粗筛考虑的图像对数量
- 选择模式参与 `parameter_version`，避免与检查点版本混淆

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_DECODE` | 图像损坏、清单缺失或无法解析 |
| `E_META` | 尺寸/通道/清单字段不匹配，单应非法 |
| `E_PARA` | 参数越界或曝光估计参数越界 |
| `E_MATCH` | 匹配数低于下限，图像对失败 |
| `E_HOMO` | 单应估计失败或内点不足 |
| `E_CLIP` | 曝光补偿溢出或融合质量门限失败 |
| `E_STATE` | 非法状态迁移 |
| `E_IO` | 持久化/输出 I/O 失败 |
| `E_CANCEL` | 取消保留码 |

图像级校验失败不会阻断其他有效图像，失败图像不会进入配准。图像对内部异常只将当前对置为 `failed`，其他对不受污染，可在 GUI/CLI 中重试。

## 输出

每次运行输出：

```text
output/
  panorama.png
  owner.npz
  registration_error_heatmap.png
  seams.json
  report.json
  checkpoint.json
  checkpoint.json.lock
  run_state.json
```

- `panorama.png`：最终全景
- `owner.npz`：每个像素归属图像编号与原始覆盖计数
- `registration_error_heatmap.png`：累计配准误差热力图
- `seams.json`：逐图像对缝合线路径
- `report.json`：逐图像对单应、内点率、残差、协方差条件数、真值误差，逐图像全局单应与画布报告
- `checkpoint.json`：只含已提交图像对的版本化快照
- `run_state.json`：`running/paused/cancelled/failed/completed` 运行状态

每次运行开始时先清除旧全景产物并写入 `running` 报告。强制终止后，`run_state.json` 与 `report.json` 不会声称全景已完成；重启只使用版本匹配的已持久化图像对。

## 本次实际验收

环境：WSL2/Linux，CPython 3.14.4，NumPy 2.5.5，OpenCV 5.1.0，PyQt5 5.15.11，24 vCPU。

自动化测试：

```text
40 passed in 8.35s
```

三路径一致性：

| 集合 | 图像数 | 模式 | 理论/实际画布 | 最大真值角点误差 | 接缝梯度 p95 | 三路径结果 |
|---|---:|---|---:|---:|---:|---|
| tiny | 3 | full | 692×499 / 692×499 | 0.156 px | 16.0 | 像素与报告完全一致 |
| small | 6 | full | 892×624 / 892×624 | 0.079 px | 16.0 | 像素与报告完全一致 |
| large | 24 | restricted | 898×737 / 898×737 | 0.792 px | 20.0 | 像素与报告完全一致 |
| highres | 4 | full | 2067×1467 / 2067×1467 | 0.109 px | 11.0 | 像素与报告完全一致 |

大规模双模式取舍：

| 模式 | 特征预算 | 候选对 | 耗时 | 画布 |
|---|---:|---:|---:|---:|
| full | 5000 | 276 | 58.07 s | 898×737 |
| restricted | 900 | 48 | 8.86 s | 898×737 |

受限模式约快 6.6 倍；两种模式理论画布一致，逐图像角点单应差异最大 `0.582 px`（声明容差 `3 px`），`parameter_version` 不同。

详细机读结果：

- `artifacts/VALIDATION_REPORT.md`
- `artifacts/verification-tiny/verification.json`
- `artifacts/verification-small/verification.json`
- `artifacts/verification-large/verification.json`
- `artifacts/verification-highres/verification.json`
- `artifacts/mode-comparison-large/mode_comparison.json`

## 项目结构

```text
panoworkbench/
  sample_generator.py  确定性样本与真值生成
  dataset.py           清单加载、结构校验、内容哈希
  features.py          SIFT/双向匹配
  homography.py        确定性 RANSAC、残差与协方差统计
  strategy.py          规模策略与候选筛选
  exposure.py          全局曝光拟合
  seam.py              最小代价缝合线
  blending.py          多分辨率融合
  state.py             图像对/全景状态机
  checkpoint.py        原子版本化检查点
  pipeline.py          端到端管线
  report.py            报告、全局配准图、输出比对
  diagnostics.py       匹配/叠加/缝合/误差可视化
  cli.py               CLI
  gui.py               PyQt5 工作台
tests/                 40 项自动化测试
docs/TECHNICAL.md      实现与一致性设计说明
```

## 已知边界

- 当前样本为平面场景，单应模型与生成器一致；真实非平面/强视差素材需要额外深度或局部模型。
- 画布四角可能为黑色，因为旋转矩形并集的包围盒不一定被图像完全覆盖。
- 全局配准采用已提交图像对图上的最短路径到基准图，不做全局光束法平差。
- 当前环境为 WSL2/Linux；“Windows 原生 GUI”由纯 PyQt5 路径和 `run_gui.bat` 提供，但未在本机 Windows 会话中实际启动验证。
