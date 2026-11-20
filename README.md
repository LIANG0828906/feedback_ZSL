# 虚拟演出场馆空间声学声场重建工作台

本项目实现一个离线、确定性的 5.1 空间声学重建工作台。输入为本地 16-bit PCM WAV
脉冲响应（6 声道、48000 Hz）、场馆几何、墙面材质查表和测试信号；输出为每个听音点的
声场系数、5.1 可听化片段、依赖失效清单和可复现诊断轨迹。

项目不依赖网络服务、GPU、Docker 或外部音频素材。桌面界面使用 PySide6，算法只依赖
NumPy 与 Python 标准库。

## 快速开始

```bash
python3 -m pip install -r requirements.txt

# 生成三个场馆（hall / club / arena）的 IR、测试信号和基线声场系数
python3 -m acoustic_workbench.samplegen --output artifacts/samples

# 启动桌面工作台（不监听任何端口）
python3 run_workbench.py --samples artifacts/samples/venue_hall

# 脚本化增量诊断
python3 -m acoustic_workbench.diagnostics \
  --samples artifacts/samples/venue_hall \
  --changes artifacts/changes.json \
  --trace artifacts/diagnostics/acceptance_trace.jsonl \
  --output-dir artifacts/diagnostics/acceptance

# 也可使用位置参数
python3 run_diagnostics.py \
  artifacts/samples/venue_hall artifacts/changes.json \
  artifacts/diagnostics/acceptance_trace.jsonl

# 标准库测试
python3 -m unittest discover -s tests -v
```

工作台是本地桌面 GUI，不创建 HTTP/WebSocket 监听端口，因此不存在端口冲突。
本机验证环境为 Python 3.14.4；代码保持 Python 3.11/3.12 兼容，未使用 3.13+
专属语法。

## 样本内容

`acoustic_workbench.samplegen` 生成：

- `venue_hall`、`venue_club`、`venue_arena` 三种馆型和材质配置。
- 每馆两个听音点，每个听音点一个 6 声道、16-bit、48000 Hz、1.35 秒 WAV IR。
- `ir_layout.json`：声道顺序为 `FL, FR, FC, LFE, BL, BR`，参考通道为 `FC`。
- `test_signal.wav`：由正弦衰减分量合成的确定性单声道测试信号。
- `baseline/fields/`：每个听音点的基线系数矩阵、场清单和索引。
- `baseline/renders/`：每个听音点的基线 5.1 可听化片段。
- 场馆 `.acoustic_cache/`：基线所有链路节点的内容寻址缓存与依赖清单。

样本合成使用一阶镜像声源反射路径、材质倍频程吸收、直达声通道偏移和确定性滤波噪声
晚期尾，仅用于可重复的本地工程验证，不代表实测脉冲响应。

## 重建链路

每个听音点的节点顺序固定为：

```text
direct_align
  -> early_reflections
  -> band_decay
  -> late_reverb
  -> multichannel_stitch
  -> field_coefficients
  -> render (按需，不属于场系数依赖链)
```

1. `direct_align`：逐声道检测首个超过门限的样本，以 `FC` 的直达样本为基准做整数样本
   对齐和增益归一。结果记录原始 WAV 样本索引、移位量和增益；任一声道检测失败返回
   `E_ALIGN_FAIL`，且不会执行后续节点。
2. `early_reflections`：在时间窗内检测反射峰，对每个事件做本地 Hann-FFT 倍频程能量
   计算，并将事件匹配到一阶反射墙面。非法时间窗、FFT 尺寸或频带配置返回
   `E_BAND_INVALID`。
3. `band_decay`：按频带和声道对反射事件能量做对数线性拟合，输出斜率、T60、R² 和
   初始能量。拟合不足时使用确定性的有界回退曲线。
4. `late_reverb`：根据衰减参数和事件频带能量生成确定性带限噪声尾。随机种子由听音点、
   声道、频带和上游内容哈希派生。
5. `multichannel_stitch`：在内存中拼成固定形状 `[6, frames]` 的完整响应，再整体写入
   缓存。任何形状、有限性或声道检查失败返回 `E_STITCH_FAIL`，不会留下部分声道文件。
6. `field_coefficients`：输出可卷积的 6 通道系数矩阵 `coefficients.npy`、场版本、
   依赖哈希和摘要 `field.json`。

渲染使用 FFT 卷积，将确定性测试信号与每个声道的场系数卷积，输出 6 声道 16-bit
48000 Hz WAV。渲染前必须满足以下全部条件，否则返回 `E_FIELD_STALE` 并拒绝写出音频：

- 磁盘 `heads.json` 中提交的场版本等于当前场景重新推导出的场节点键。
- 场清单中的 `version` 与 `dependency_hash` 等于当前场版本。
- 上游每个节点在磁盘缓存中存在，且输出哈希与场清单依赖一致。
- 系数文件内容哈希与清单 `coefficient_hash` 一致。

## 增量失效规则

每个影响因子映射到最早失效节点，再沿依赖链传播到 `field_coefficients`：

| 影响因子 | 最早失效节点 | 影响范围 |
| --- | --- | --- |
| `material` | `early_reflections` | 全部听音点（晚期混响为全局）；`direct_align` 复用 |
| `source_position` | `direct_align` | 全部听音点 |
| `listener_position` | `direct_align` | 仅指定听音点；其他听音点全部复用 |
| `band_config` | `early_reflections` | 全部听音点；`direct_align` 复用 |
| `window` | `early_reflections` | 全部听音点；`direct_align` 复用 |
| `alignment` | `direct_align` | 全部听音点 |
| `late_config` | `late_reverb` | 全部听音点；前三个节点复用 |

材质修改还会输出“受影响的反射事件集合”：事件通过镜像声源路径的到达时间与墙面 ID
匹配，只标记对应墙面的事件。频带、窗和声源变化会标记当前早期事件集合。

## 内容哈希缓存

缓存位于场馆目录下的 `.acoustic_cache/`：

```text
.acoustic_cache/
  nodes/<listener>/<node>/<cache_key>/
    result.json
    aligned.npy | early_segment.npy | late_response.npy | response.npy | coefficients.npy
  state/heads.json
  state/render_heads.json
  locks/<listener>.lock
  renders/<listener>/<render_key>/
```

节点键由节点名、实现版本、听音点、上游输出哈希和当前有效参数共同构造。缓存命中只依赖
内容哈希与文件哈希，不使用文件修改时间。`result.json` 同时记录数据载荷哈希和每个
产物的 SHA-256；加载时重新计算并验证。

节点先写入 `.acoustic_cache/tmp/` 暂存目录，完整后使用目录级 `os.replace` 提交。
因此进程在写节点中途被终止时，旧节点目录仍然完整，下一次运行可从磁盘清单继续。

## 并发、取消与恢复

- 每个听音点有线程锁和 `flock` 跨进程锁。同一听音点同时只允许一个重建或渲染任务，
  重复提交返回 `E_BUSY`。
- 不同听音点可独立执行；听音点位置修改只触发目标点的链路。
- 重建在进入/离开节点前后设置取消检查点；取消返回 `E_CANCELLED`。
- 取消或异常不会删除最后一个完整节点和已提交 `heads.json`；暂存目录会被清理。
- 新进程不依赖内存状态，只从 `scene.json`、`result.json`、`heads.json` 和内容哈希
  恢复缓存。

## 诊断入口

`acoustic_workbench.diagnostics` 每个新进程都会建立独立增量缓存和独立全量缓存：

```bash
python3 -m acoustic_workbench.diagnostics \
  --samples artifacts/samples/venue_hall \
  --changes artifacts/changes.json \
  --trace artifacts/diagnostics/acceptance_trace.jsonl \
  --output-dir artifacts/diagnostics/acceptance
```

轨迹 JSONL 每行是一条规范化 JSON 记录，包含：

- 基线节点状态和输入内容哈希。
- 每个影响因子、失效节点、失效原因、实际复算节点、缓存命中节点。
- 受影响反射事件 ID。
- 每个节点的 `cache_key` 与输出哈希。
- 增量渲染片段哈希。
- 独立全量重算的场哈希、最大绝对误差和容差结论。

相同输入与操作序列下，轨迹日志和最终系数均逐字节一致。验证时连续运行两次并使用
`cmp` 比较轨迹，结果为 `TRACE_BYTE_IDENTICAL`。

修改序列示例见 `artifacts/changes.json`。干净运行可重现完整基线；若需要研究重启复用，
传入 `--keep-cache` 复用已有诊断缓存。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_ALIGN_FAIL` | 任一声道未检测到直达声、参考声道无效或移位超限 |
| `E_BAND_INVALID` | 非法早期时间窗、FFT 尺寸、频带或衰减参数 |
| `E_WAV_INVALID` | 非 16-bit PCM、非 6 声道或采样率不符 |
| `E_STITCH_FAIL` | 5.1 声道形状、有限性或拼接一致性检查失败 |
| `E_FIELD_STALE` | 场版本、依赖清单或系数内容哈希不匹配 |
| `E_BUSY` | 同一听音点已有重建/渲染任务 |
| `E_CANCELLED` | 在检查点取消 |
| `E_CONFIG_INVALID` / `E_FACTOR_INVALID` | 场景、材质或影响因子格式非法 |
| `E_RENDER_FAIL` | 测试信号或卷积渲染失败 |

## 图形工作台

`run_workbench.py` 提供：

- 俯视场馆平面、墙面材质颜色、声源、听音点和选中听音点的一阶反射路径。
- 材质和声源位置编辑；编辑值变化时实时预览依赖失效高亮。
- 节点依赖表：预览失效为橙色，复算完成为绿色，缓存命中为灰色。
- 提交后展示全馆失效范围、每点复算/命中节点数和受影响反射事件数。
- 取消按钮在节点检查点停止任务，并保留最后一次已提交缓存。
- “试听最近有效片段”仅在存在有效渲染时启用；播放前再次校验场版本和依赖清单。

界面预览见 `artifacts/workbench_preview.png`。

无显示环境可执行：

```bash
QT_QPA_PLATFORM=offscreen python3 run_workbench.py \
  --samples artifacts/samples/venue_hall --smoke-test
```

## 实际验证结果

本次工作区已生成：

- 3 个场馆、6 个听音点、6 个基线场系数矩阵和 6 个基线 5.1 渲染片段。
- 验收轨迹：`artifacts/diagnostics/acceptance_trace.jsonl`。
- 重复运行轨迹：`artifacts/diagnostics/repeat_trace.jsonl`，与验收轨迹逐字节相同。
- 验收轨迹 SHA-256：
  `2ddaece68ed7229e7f99bc1b3cc67d8f6247809d1c61c9fc6dff885ba56f6dd8`。
- 修改序列 SHA-256：
  `ebd36577e5f7a231db377974c0d70932ed438efb92b68ddad6ca11c31ee7f2cc`。

验收轨迹中的关键结果：

- 材质修改：`direct_align` 缓存命中，`early_reflections` 至 `field_coefficients`
  复算，并输出受影响反射事件。
- 声源移动：从 `direct_align` 起复算全部六个节点。
- 听音点位置修改：仅目标听音点复算；另一听音点六个节点全部缓存命中。
- 频带配置和窗参数修改：`direct_align` 命中，后五个节点复算。
- L1 增量/全量系数哈希：
  `5ed8a537e19f62bda66fd5c6ef9c7cd8da9acf2181c8015134009377194b40e0`。
- L2 增量/全量系数哈希：
  `c433d091105cc5da10503e9125a61d78ddfc211243175327b9122cd9767da70a`。
- 两个听音点的 `max_abs_difference = 0.0`，`within_tolerance = true`，
  `byte_identical = true`。
- 轨迹二次运行逐字节相同。
- `python3 -m unittest discover -s tests -v`：10 项测试全部通过。
- GUI headless smoke test 通过；自动化 GUI 提交流程显示失效范围、复算列表并生成可试听
  片段。

## 项目结构

```text
acoustic_workbench/
  alignment.py      直达声检测、样本对齐、增益归一
  reflections.py    早期事件检测、FFT 频带能量、材质事件匹配
  decay.py          对数衰减拟合与 T60
  late.py           确定性晚期混响合成
  stitch.py         5.1 拼接与场摘要
  field.py          场清单和系数哈希校验
  cache.py          内容寻址节点/渲染缓存与原子提交
  graph.py          依赖图、影响因子失效传播
  service.py        重建编排、版本校验、并发与渲染
  locks.py          线程锁 + 跨进程 flock
  cancellation.py   检查点取消令牌
  samplegen.py      确定性样本与基线生成
  diagnostics.py    增量/全量对照和 JSONL 轨迹
  gui.py            PySide6 工作台
tests/
  test_engine.py
  test_diagnostics.py
```
