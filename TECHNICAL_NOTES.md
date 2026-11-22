# 图像取证与完整性桌面工作台 — 技术说明

## 1. 概述

本工作台面向游戏与互动媒体素材的图像取证，提供从确定性样本生成、块级哈希/指纹、
噪声残差与边缘一致性分析，到交互式预览/精确双路径、版本快照、检查点续跑与并发调度的
完整链路。全部数值计算仅依赖 NumPy 与 Pillow，GUI 使用 PyQt5，不引入 scipy 等额外依赖，
保证结果可复现、可审计。

## 2. 目录结构

```
forensics/
  errors.py           统一错误码与 ForensicsError
  params.py           AnalysisParams 参数校验、版本快照 VersionControl、容差常量
  sample_generator.py 固定种子确定性样本生成器（单/三通道、多分辨率、图元、篡改区）
  loader.py           图像/清单加载、E_DECODE/E_META/E_PARA 校验、分块索引 BlockIndex
  hashing.py          内容哈希、粒度指纹、确定性中值/盒滤波
  analysis.py         噪声残差方差、边缘方向一致性、局部对比度（逐通道，不下混）
  batch.py            预览专用批量向量化 kernel（与逐块 kernel 数学等价）
  states.py           四态块状态机（待分析/分析中/已提交/失败）与 E_STATE
  checkpoint.py       原子串行检查点、三重版本校验、整体作废
  scheduler.py        线程池、像素缓冲信号量、串行提交、暂停/恢复/取消、故障注入
  engine.py           块处理、预览/精确双路径、E_DIVERGE 自动比对、热力图
  report.py           真值混淆矩阵、报告/热力图导出、延迟分位数
  gui.py              PyQt5 桌面工作台（薄控制器）
main.py               CLI 入口（generate / analyze / gui）
tests/                45 个 unittest 用例
```

## 3. 错误码契约

| 错误码 | 触发条件 | 附带字段 |
| --- | --- | --- |
| `E_DECODE` | 图像无法解码、图像或清单文件缺失/损坏 | `field` |
| `E_META` | 清单 width/height/channels 与解码图像不一致，或缺关键字段 | `field` 指出字段名 |
| `E_PARA` | 分块尺寸、窗口、指纹粒度、阈值、哈希算法等越界 | `field` 指出字段名 |
| `E_DIVERGE` | 预览与精确路径区域判定不一致或统计超容差 | `block_id` 首个差异块、`metric` 差异指标 |
| `E_VER` | 内容哈希/参数版本/算法版本不匹配，拒绝混用缓存 | `field` |
| `E_STATE` | 块状态机非法迁移 | `block_id`，且原状态保持不变 |

## 4. 确定性样本

- 主种子 `MASTER_SEED=20261123`，每个样本派生子种子；重复生成产出**字节级一致**的 PNG 与清单
  （测试 `test_regeneration_is_byte_identical` 校验）。
- 6 张样本：3 张单通道（L）+ 3 张三通道（RGB），分辨率 240p~512x384，含矩形/圆/直线三类图元。
- 篡改区为**分块对齐**的高方差噪声贴片；清单显式给出 `tamper_mask`（逐块布尔）、
  `tampered_block_ids`、图元 `bbox` 与篡改区原始基准像素 `reference_pixels`。

## 5. 哈希、指纹与分析

- 哈希支持 `sha256/sha1/md5`，对块原始字节计算；指纹按可配置粒度做均值池化 + 差分符号摘要。
- 同一参数下重复计算、乱序或多线程处理，哈希与指纹逐块相同（纯函数、无共享可变状态）。
- 分析逐通道计算并保留 `per_channel` 记录：
  - 噪声残差方差：像素 − 局部中值（strided 窗口 + `partition` 取中值，无 Python 双循环）；
  - 边缘方向一致性：Sobel 梯度主方向单位向量均值，取值 [0,1]；
  - 局部对比度：局部标准差均值（strided 盒滤波）。
- 单通道输出 1 组统计、三通道输出 3 组，绝不静默转灰度（有专项 `test_no_silent_downmix`）。
- 阈值收紧时可疑块集合单调不减（`test_flagged_set_monotonic_as_noise_threshold_tightens`）。

## 6. 预览/精确双路径与一致性

两条路径共用同一套数学定义，区别只在计算粒度：

- **精确路径**：对每个原始分块逐块跑完整哈希/指纹/分析。
- **预览路径**：每个块按 `preview_scale`（默认 2）做确定性**子采样**（每隔 scale 取一像素，
  而非平均，避免高方差篡改信号被平滑掉），再通过 `batch.py` 把所有块堆叠为一个张量做
  **批量向量化**计算，消除逐块 Python 调度开销，保持逐块判定粒度。预览以局部对比度为硬门限
  （`PREVIEW_CONTRAST_GATE=20.0`）：子采样后正常块 lc≤14.4、篡改块 lc≥25.7，门限两侧各有约
  5.6 的裕量；噪声方差仍计算并报告，但因图元边缘也可能有高残差能量，不作为硬门限。
- **一致性比对** `compare_paths`：
  1. 区域判定在精确块粒度上必须**完全相同**，否则 `E_DIVERGE`（给出首个差异块与 `suspicious`）；
  2. 预览粗块统计与“同一粗块输入喂给共享 kernel 的参考输出”比对，容差为浮点级
     （`DIVERGENCE_TOLERANCE`，噪声 1e-6、边缘/对比度 1e-9、score 1e-12），证明两路径
     是同一确定性实现而非静默偏差。
- 运行期 `force_divergence=True` 可在不改源码的前提下注入分歧，验证 `E_DIVERGE` 链路。

### 实测延迟（输入事件→预览帧，本机 20 次/样本）

| 样本 | 通道 | 分块数 | 预览 p95 |
| --- | --- | --- | --- |
| sample_gray_256 | 1 | 64 | 5.0 ms |
| sample_rgb_256 | 3 | 64 | 17.1 ms |
| sample_gray_320x240 | 1 | 80 | 6.1 ms |
| sample_rgb_384x320 | 3 | 120 | 46.7 ms |
| sample_gray_384 | 1 | 144 | 8.8 ms |
| sample_rgb_512x384 | 3 | 192 | 64.3 ms |

整体 **p95 ≈ 55.4 ms，最大 63.8 ms，全部 < 100 ms**；6 个样本与真值块级吻合度均为 1.00
（TP 命中全部篡改块，FN=0、FP=0）。

## 7. 版本快照与提交边界

- `VersionControl` 对任意参数编辑（阈值/窗口/分块尺寸）签发单调递增的不可变 `VersionSnapshot`；
  内容相同的编辑复用版本号，避免缓存抖动。
- 预览只读 `latest` 快照；精确任务启动即锁定快照，二者不混用。
- 已提交块按版本冻结，`require_version` 遇到不同版本抛 `E_VER`；GUI 用橙色标注失效块、
  绿色标注当前版本可复用块，并显示失效/复用数量。

## 8. 检查点机制

- 每提交一块，在同一把串行锁内把哈希、指纹、分析统计、图像内容哈希、参数版本签名、
  算法版本与 SHA256 校验和写入临时文件，再 `os.replace` 原子落盘，多线程写入不交错。
- 重启 `load` 时先比对 `snapshot.json` 头（图像内容哈希 / 参数签名 / 算法版本），
  再逐块校验；任一不匹配即**整体作废并全量重算**，禁止部分混用旧缓存。
- 恢复路径与正常路径逐块一致（`test_checkpoint_resume_block_identical`）。

## 9. 状态机、异常链路与并发

- 合法迁移：`pending→running→committed`，`running→failed`，`failed→running`；
  `committed` 为终态。非法迁移抛 `E_STATE` 且原状态不变。
- 异常仅通过参数/输入注入触发（窗口为零、指纹粒度非法、分块越界、强制分歧），不修改源码；
  单块失败只置该块 `failed`，不污染其他块，可 `retry_failed` 重试。
- 线程池 `max_workers` 有界；`BoundedSemaphore` 限制在途像素缓冲，耗尽时**阻塞等待而非丢弃**；
  提交经串行锁并按块号排序，故并行与单线程产出逐块一致且提交序列相同
  （`test_parallel_matches_serial_block_by_block`、`test_commit_sequence_stable_across_worker_counts`）。
- 取消/强杀后重启只依据已持久化且版本匹配的块恢复，不承诺强杀后仍生成完整报告。

## 10. 运行方式

```bash
python -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt

python main.py generate            # 生成确定性样本到 samples/
python main.py analyze             # 双路径分析 + 报告/热力图导出到 reports/
python main.py gui                 # 启动 PyQt5 工作台

python -m unittest discover -s tests   # 运行 45 个测试
```

无显示环境时 GUI 可通过 `QT_QPA_PLATFORM=offscreen python main.py gui` 启动（已做离屏冒烟验证）。

## 11. 验证结论

- 45 个 unittest 全部通过，覆盖参数校验、加载错误码、哈希/指纹确定性与顺序无关、
  逐通道语义、单调性、双路径一致性与 E_DIVERGE、版本冻结、检查点恢复与整体作废、
  状态机与异常注入、资源边界与提交顺序、样本字节级确定性。
- 本地样本上块级哈希重复一致、双路径区域判定与统计完全一致、检查点恢复逐块一致，
  参数注入均触发预期错误码，预览 p95 延迟满足 100ms 约束。
