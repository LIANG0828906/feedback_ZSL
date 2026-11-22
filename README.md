# EdgeForge

面向游戏与多媒体素材的本地桌面边缘检测、形态学处理与分块调度诊断工具。项目从零实现固定种子样本生成、单/三通道处理、分块阶段状态机、带预算的线程池调度、跨块重叠边带、原子检查点、断点恢复、三路径逐像素比对和 PyQt5 可视化界面。

应用运行时不访问网络，不使用 Docker；样本、真值、运行日志和检查点全部位于本地工作区。

## 环境与安装

- Python 3.10 及以上
- NumPy
- PyQt5

```powershell
python -m venv .venv
.venv\Scripts\python.exe -m pip install --no-index --find-links wheelhouse -r requirements.txt
```

将本地 wheel 放入 `wheelhouse`，或使用组织内网镜像。应用本身没有网络请求。

Linux/macOS：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install --no-index --find-links wheelhouse -r requirements.txt
```

界面字体随项目附带 `assets/fonts/wqy-microhei.ttc`（Apache-2.0/GPL-3+ with font exception，详见同目录许可证），用于无中文系统字体的环境。

## 启动

```bash
# 生成全部固定种子样本
.venv/bin/python -m edgeforge samples --out samples

# 启动桌面诊断台
.venv/bin/python -m edgeforge gui --samples samples --run-dir runs/gui

# 无界面确定性验收
.venv/bin/python -m edgeforge verify
```

Windows 可直接使用 `run_cli.bat`、`run_gui.bat` 和 `run_tests.bat`。

## 确定性样本

样本生成器版本为 `edgeforge.samples.v1`，主种子 `20261123`，每个样本另有写死的独立种子。生成顺序不影响内容。

| 样本 | 分辨率 | 通道 | 场景 |
| --- | ---: | ---: | --- |
| `low_contrast_gray_320x240` | 320×240 | 1 | 低对比度、轻噪声 |
| `noisy_rgb_800x600` | 800×600 | 3 | 高噪声彩色 |
| `mixed_gray_1600x1000` | 1600×1000 | 1 | 中高分辨率混合纹理 |
| `panorama_rgb_2048x1152` | 2048×1152 | 3 | 宽幅高分辨率彩色 |

每个样本目录包含：

- `image.png`：8 位 PNG。
- `truth_mask.npy`：理想边缘掩膜 `uint8`，只含 0/1。
- `meta.json`：尺寸、通道、种子、直线端点、圆心/直径、边缘覆盖率、图像与真值内容哈希。

`samples/manifest.json` 是样本清单。加载器每次独立读取并校验 PNG 头、尺寸、通道、真值掩膜形状、覆盖率声明和内容哈希；没有进程级可变缓存，因此加载顺序不影响结果。

## 处理管线

阶段固定为：

1. `gradient`：Sobel / Prewitt / Scharr 归一化梯度幅值，输出与输入同通道。
2. `threshold`：高阈值产生强边缘，低阈值弱边缘仅在强边缘 1 像素邻域内保留。
3. `open`：结构元素开运算。
4. `close`：结构元素闭运算。
5. `thin`：Zhang-Suen 细化。
6. `prune`：删除 8 连通分量像素长度不超过阈值的短线段。

所有形态学与细化都使用重叠边带。边带宽度不是固定值，而是由阶段依赖推导：

| 阶段 | 所需边带 |
| --- | --- |
| `gradient` | 1 |
| `threshold` | 1 |
| `open` | `2 * (open_size // 2)` |
| `close` | `2 * (close_size // 2)` |
| `thin` | `thin_iterations + 1` |
| `prune` | `prune_length + 1` |

开/闭运算各包含两次半径为 `r` 的形态学操作，因此核心像素最多依赖 2r 邻域；细化和剪枝按迭代/长度逐像素传播。配置强制边带小于所需宽度时返回 `E_BAND`，携带块号、阶段、所需宽度和配置宽度，不会静默截断。

双阈值采用确定性的局部弱边缘支持（强边缘、弱边缘、1 像素支持半径），而不是无界全局连通传播。这样阈值阶段具有有限依赖半径，分块结果才能与整图严格逐像素一致。

## 分块调度与预算

- 图像按 `tile_size` 切成固定核心块，块号按行优先稳定分配。
- 每块每阶段只有 `pending → processing → committed|failed`，失败可重试；其余迁移返回 `E_STATE`。
- 工作线程池最大线程数为 `max_parallel`。
- `thread_buffer_pixels` 限制单任务扩展像素缓冲；超限返回 `E_BUDGET`。
- `memory_budget_bytes` 是全局边带内存预算。预算不足时调度器等待完成任务并记录 `memory_budget` 排队事件，然后继续提交，不会丢块。
- 检查点提交只在协调线程内按块号顺序执行；更早块未完成时，后完成块会等待，不越过提交顺序。
- 完成块按块号提交，因此正常路径的提交序列稳定为“阶段顺序 × 块号顺序”。

用户取消只保留已原子写入的检查点。进程被强制终止时不承诺生成完整报告，重启后仅加载哈希、参数版本和算法版本全部匹配的记录。

## 检查点

运行目录结构：

```text
runs/.../
  run_meta.json
  records/
    stage-00_block-00000_seq-000000.npz
    ...
```

每个记录含：

- 当前块阶段输出数组
- 规范数组内容 SHA-256
- 分块编号、阶段、全局提交序号
- 参数版本
- 算法版本
- 运行指纹

写入流程为“同目录临时文件 → flush/fsync → `os.replace` 原子替换”。提交由锁和单一协调线程串行化。启动时会扫描所有记录；任一记录不可读、哈希不符、参数/算法版本不符、序号断档或阶段顺序非法，会把整个快照重命名为 `*.invalid-*`，然后从零全量重算。崩溃遗留的 `.tmp-*` 文件会被忽略。

若输入、真值、分块尺寸或参数版本改变，运行指纹不匹配，也会整体作废快照。

## 可视化界面

PyQt5 界面包含：

- 原图、真值边缘、六阶段中间结果和差异热力图。
- 分块 × 阶段状态矩阵，颜色区分待处理、处理中、已提交、失败。
- 点击单元格查看块号、阶段、尝试次数与错误。
- 失败块重试、暂停/继续、取消、全量重算。
- 逐阶段覆盖率、精确率、F1、直线端点偏差、缺失端点数和圆覆盖率。
- 预算排队、检查点提交、作废、恢复和失败日志。
- 导出原图、真值、阶段 PNG、`metrics.json`、块状态 CSV、排队事件和检查点信息。

![EdgeForge GUI](artifacts/gui_screenshot.png)

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_DECODE` | PNG/真值/元数据无法解码，或内容哈希不符 |
| `E_META` | 尺寸、通道、真值形状或声明字段不一致，并携带 `field` |
| `E_PARA` | 参数越界、低阈值大于高阈值或非法算子，并携带 `field` |
| `E_STATE` | 非法块阶段迁移；原状态和提交结果保持不变 |
| `E_BAND` | 边带不足；携带块号、阶段与所需宽度 |
| `E_BUDGET` | 单任务超过每线程缓冲或全局预算无法容纳 |
| `E_CHECKPOINT` | 检查点路径、读写或作废失败 |
| `E_CANCEL` | 用户取消；只保留已提交记录 |
| `E_PIPELINE` | 阶段内存在失败块，附带 `failures` 清单 |

## 当前实测结果

2026-11-23 在当前工作区实际执行：

| 项目 | 结果 |
| --- | --- |
| 单元/集成测试 | 22/22 通过 |
| 端到端验收 | 13/13 检查通过 |
| 主样本 | 320×240×1，64 像素分块，20 块/阶段，共 120 个块阶段 |
| 串行 vs 并行 | 首差异 `null`，几何指标差异为空 |
| 整图单块 vs 分块 | 首差异 `null` |
| 完整阶段恢复 | 持久化 60 条，恢复 60 条，首差异 `null` |
| 部分提交阶段恢复 | 第 25 个提交事件触发取消，恢复 25 条，首差异 `null` |
| 检查点篡改 | `record_hash_mismatch`，旧快照整体作废，重算后一致 |
| RGB 并行 vs 串行 | 600×800×3，首差异 `null` |
| 高分分辨率完整管线 | 2048×1152×3，40 块，240 个块阶段全部提交，六阶段形状一致 |
| 低预算排队 | 200,000 B 预算，峰值 187,200 B，23 次 `memory_budget` 排队，无丢块 |

最终剪枝阶段的真值统计：覆盖率 36.80%，精确率 100%，F1 53.80%，直线端点平均偏差 35.71 像素，圆覆盖率 65.29%。完整分阶段统计和所有检查证据在 `artifacts/verification.json`。

## 验证

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -v
.venv/bin/python -m edgeforge verify
```

完整验收报告写入 `artifacts/verification.json`。当前实测结果见该文件；`verify` 覆盖：

- 固定种子生成与加载顺序独立性。
- 单通道/三通道形状一致。
- 串行、并行、整图单块逐像素一致。
- 并行提交顺序稳定性。
- 完整阶段恢复与阶段内部分提交恢复。
- 全量重算一致。
- 检查点哈希篡改导致整体作废并重算。
- RGB 并行/串行一致。
- 低预算阻塞排队且不丢块。
- `E_DECODE`、`E_META`、`E_PARA`、`E_STATE`、`E_BAND`、`E_BUDGET` 实际触发。

更详细的算法与一致性说明见 `docs/TECHNICAL.md`。

