# 空间声学与声场重建工作台

面向游戏与互动媒体素材的本地、确定性、可复现空间声学工作台。项目从多声道 IR
估计逐帧直达延迟、早期反射能量、衰减、传播时差和能量比，再以持久化参数卷积渲染
多声道干声。核心不是单次播放，而是 **实时、断点恢复、参数全量复算三条路径逐样本
一致**，以及通过输入注入触发的稳定错误链路。

本项目不依赖 Docker、网络服务、真实账号或云端资源。运行期只读取本地文件。

## 环境

- Python 3.10+；本机实际验证使用 Python 3.14.4
- NumPy、SciPy、Matplotlib、PySide6、pytest
- 本机桌面依赖已安装到工作区 `.venv`

Linux/macOS：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
```

Windows（使用本地路径）：

```powershell
py -3 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
```

若系统缺少 `ensurepip`，需先安装对应发行版的 `python3-venv`，或使用
`get-pip.py` 初始化虚拟环境。

## 快速开始

```bash
# 1. 生成固定种子样本与四种故障变体
.venv/bin/python -m spatial_workbench.cli generate \
  --out data/samples --seed 42042

# 2. 查看逐帧分析结果与解析真值对比
.venv/bin/python -m spatial_workbench.cli analyze \
  --sample data/samples/spatial_normal.npz \
  --output artifacts/analysis.json

# 3. 实时逐帧运行；每提交一帧写一次原子检查点
.venv/bin/python -m spatial_workbench.cli run \
  --sample data/samples/spatial_normal.npz \
  --checkpoint artifacts/run/checkpoint.json \
  --mode realtime \
  --out artifacts/run/export

# 4. 从检查点恢复未提交后缀
.venv/bin/python -m spatial_workbench.cli run \
  --sample data/samples/spatial_normal.npz \
  --checkpoint artifacts/run/checkpoint.json \
  --mode recovery

# 5. 只用已持久化参数全量复算
.venv/bin/python -m spatial_workbench.cli run \
  --sample data/samples/spatial_normal.npz \
  --checkpoint artifacts/run/checkpoint.json \
  --mode replay

# 6. 完整验收：三路径、首个差异、快照失配、状态机、四类故障
.venv/bin/python -m spatial_workbench.cli verify \
  --sample data/samples/spatial_normal.npz \
  --sample-dir data/samples \
  --work-dir artifacts/verification

# 7. 桌面可视化面板
.venv/bin/python -m spatial_workbench.cli gui \
  --sample data/samples/spatial_normal.npz \
  --checkpoint artifacts/gui/checkpoint.json
```

测试：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest -q
```

## 目录

```text
spatial_workbench/
  samplegen.py     固定种子 NPZ 样本与故障变体
  io.py            元数据、解码、通道校验
  analysis.py      分帧延迟/反射/衰减/TDOA/IID/方向
  models.py        配置、帧参数、渲染结果、通道映射
  render.py        参数化 FIR、卷积、OLA、裁剪检查
  state.py         PENDING/ESTIMATED/COMMITTED/FAILED 状态机
  checkpoint.py    原子全快照、文件锁、完整性校验
  pipeline.py      实时/恢复/复算编排、并行度、取消
  compare.py       三路径逐样本比较与首个差异定位
  faults.py        程序化输入故障注入
  verification.py  自动验收与报告
  audio.py         NPY/WAV/逐帧 JSON 导出
  gui.py           PySide6 + Matplotlib 桌面面板
  cli.py           命令行入口
tests/             37 项 pytest 覆盖
data/samples/      固定种子正常样本与四个故障样本
artifacts/         运行产物及验收报告
```

## 样本格式

每个 `.npz` 自包含以下字段，不依赖旁路推断：

- `ir`: `float64[C, N]`，多声道脉冲响应
- `dry`: `float64[N]`，单声道干声片段
- `metadata`: UTF-8 JSON 字符串

`metadata` 包含：

- `sample_rate`、`channel_layout {name, channels, azimuths_deg}`
- 逐通道解析真值 `truth.direct_delay_samples`
- 逐通道能量衰减真值 `truth.energy_decay_coeff`
- 早期反射到达样本与增益真值
- `missing_channels`、`bad_frames` 等故障标记

缺失 `sample_rate`、`channel_layout` 或解析真值时返回 `E_META`；损坏 NPZ、
非法数组形状或非有限样本返回 `E_DECODE`。分析器不会从缺失元数据反推真值。

默认样本使用 `seed=42042`、16 kHz、四声道 `FL/FR/RL/RR`、32768 点（64 帧，
帧长 1024、帧移 512）。直达声、五个早期反射和指数尾音均由种子确定。

## 分析模型

1. 对每个通道从完整 IR 检测直达峰，得到整数样本直达延迟。
2. 按可配置帧长/帧移切分 IR；帧内估计局部到达、直达能量、早期反射能量、
   早期/总能量比、指数能量衰减系数和最多 `max_reflections` 个反射峰。
3. 以第一通道为参考计算 TDOA、ILD 和通道能量比。
4. 按声道方位角与帧能量加权得到二维方向向量、方位角和置信度。
5. 每帧参数包含全局直达延迟、局部到达、反射、能量、衰减、TDOA/IID、
   方向、输出映射和帧时间边界，全部可 JSON 往返。

解析真值只用于报告和验收，不参与估计。默认样本实测直达延迟在四个通道均与
解析真值完全一致；能量衰减系数最大绝对误差约 `5.25e-4`。

## 渲染

每帧从持久化参数合成参数化 FIR：

- 直达/局部到达脉冲
- 估计到的早期反射脉冲
- 由能量衰减系数决定的确定性指数尾音

干声按同一帧网格加窗，与各源通道 FIR 做 FFT 卷积，再按可配置
`ChannelMap` 映射到输出布局并用 OLA 累加。最终输出严格裁剪为
`(输出通道数, len(dry))`，与干声时长逐点一致。

支持标准布局 `mono/stereo/quad/5.1/binaural`，也支持显式权重映射：

```bash
--output-layout stereo
--channel-map '{"FL":{"FL":1.0,"RL":0.25},"FR":{"FR":1.0,"RR":0.25}}'
```

参数越界（帧长、帧移、阈值、权重、未知布局等）返回 `E_PARA`。输出或在
OLA 提交边界超过 `clip_limit` 返回 `E_CLIP`。

## 帧状态机

状态：

- `pending`：待估计
- `estimated`：已估计，参数可重估
- `committed`：已提交，参数冻结
- `failed`：失败，可重试

合法迁移：

```text
pending   -> estimated | failed
estimated -> pending   | committed | failed
failed    -> pending
committed -> (none)
```

`estimated` 重估路径是 `estimated -> pending -> estimated`。提交后任何尝试重估
或改写参数均返回 `E_STATE`，原状态和参数保持不变。失败帧通过
`failed -> pending` 重试，不影响其他帧已提交参数。

## 检查点

检查点是一个带完整性哈希的 JSON 全快照：

```text
{
  format,
  integrity: sha256(payload),
  payload: {
    input_hash,
    algorithm_version,
    generation,
    frames: [{
      frame_index,
      status,
      params,                 # 完整重建参数
      direct_delays_samples,
      energy_statistics,
      render_statistics,
      input_hash,
      algorithm_version,
      parameter_fingerprint
    }]
  }
}
```

提交规则：

- 进程内线程锁 + 跨进程文件锁，提交严格串行
- 帧索引必须等于当前已提交帧数，保证稳定时间顺序
- 同目录临时文件写入、`fsync`、`os.replace` 原子替换
- 文件损坏、输入哈希不匹配或算法版本不匹配时，整份快照失效，禁止部分混用
- 恢复路径检测到失配后清空旧代次并全量重算

坏帧故障标记不进入输入内容哈希，因此移除坏帧注入后可继续使用同一输入哈希和
已提交前缀重试；通道缺失、时反和增益异常会改变实际信号或显式元数据。

## 三路径一致性与 E_DIFF

- 实时路径：估计、渲染、逐帧串行提交
- 恢复路径：加载合法已提交前缀，仅计算未提交后缀
- 复算路径：不重新估计，只用检查点参数和干声重新渲染

三条路径必须得到：

- 相同通道顺序与形状
- 每个 `float64` 输出样本完全相等
- 相同帧参数指纹
- 相同逐帧输出能量统计

不一致时比较器返回首个不一致通道、采样位置和所属帧，`E_DIFF` 的
`details.first_difference` 包含完整定位信息。

## 异常与错误码

| 错误码 | 触发方式 | 链路位置 |
| --- | --- | --- |
| `E_META` | 缺少采样率、布局或解析真值 | 加载 |
| `E_DECODE` | 损坏文件、非法数组、非有限值 | 解码 |
| `E_PARA` | 帧长/帧移/阈值/映射权重越界 | 配置 |
| `E_STATE` | 非法状态迁移、提交越序、非完整复算 | 状态/检查点 |
| `E_DIFF` | 三路径参数/统计/采样不一致 | 比较 |
| `E_CHAN` | 缺失通道、通道数与布局不符 | 加载/输入校验 |
| `E_ORDER` | 直达到达晚于时序上界（如时反样本） | 输入校验 |
| `E_CLIP` | 输入峰或卷积/OLA 结果超限 | 输入/渲染 |
| `E_FRAME` | 注入坏帧 | 单帧估计 |
| `E_RESOURCE` | 工作线程缓冲估计超上限 | 分析调度 |
| `E_IO` | 导出或原子写入失败 | 导出/检查点 |

故障既可由 `generate` 生成的 NPZ 变体触发，也可通过 `FaultPlan` 对内存样本注入：

```python
from spatial_workbench.faults import FaultPlan

missing = FaultPlan(missing_channel="RR").apply(sample)
reversed_sample = FaultPlan(reverse_time=True).apply(sample)
overflow = FaultPlan(gain=32.0).apply(sample)
bad = FaultPlan(bad_frames=(3,)).apply(sample)
```

## 并发、取消与资源

- 估计与渲染均使用有界 `ThreadPoolExecutor`
- `max_workers` 限制最大并行度
- `max_buffer_bytes` 按每工作线程估计缓冲，超限返回 `E_RESOURCE`
- 渲染结果按帧索引排序后 OLA，多线程不改变输出
- 检查点提交仍在串行临界区
- `ReconstructionPipeline.cancel()` 在当前帧边界停止；已提交前缀可恢复
- GUI 提供“取消”按钮
- 强制终止不承诺完整退出报告；重启只能读取原子替换后的已提交快照

## 桌面面板

`gui` 子命令提供：

- 逐帧参数表与状态
- 通道直达延迟、早期反射能量曲线
- 帧总能量与方向置信度
- 二维方向向量示意
- 原始 IR 与渲染波形对比
- WAV/NPY/逐帧 JSON 导出
- QtMultimedia 试听
- 帧长、能量阈值、输出布局、并行度和缓冲上限调整

配置变更不会直接改写已提交帧。点击“应用配置到未提交帧”或直接恢复时，已提交
记录保持原参数，只有后续恢复出的未提交后缀使用新配置。帧移决定时间网格，
存在提交帧后锁定。

## 实际验证结果

最终代码实际运行结果：

- `pytest`: `37 passed`
- 正常样本：4 通道 × 32768 点，64 帧
- 三路径实时/恢复/复算输出哈希均一致
- 逐样本、逐帧参数和逐帧能量统计均完全一致
- 首个差异探针在样本 123 返回，并定位到帧 0
- 直达延迟真值四个通道均精确匹配
- 能量衰减系数最大绝对误差 `5.25e-4` 量级
- 输入哈希失配与算法版本失配均使整份快照失效并全量重算
- 异常码实测：`E_CHAN/E_ORDER/E_CLIP/E_FRAME`
- 坏帧故障在帧 3 失败，保留前 3 个提交帧；移除注入后恢复完成
- 缺失元数据 `E_META`、损坏样本 `E_DECODE`、非法状态 `E_STATE` 均实测

验收报告位于 `artifacts/verification/verification_report.json`。具体输出哈希以
该次报告为准；三路径哈希在报告中应完全相同。

## 设计说明与边界

- IR 分帧是局部时空快照；全局直达延迟保存在每帧参数中，渲染同时使用帧内局部
  到达与反射/尾音参数。
- 渲染是参数化重建，不承诺与直接使用原始 IR 做整段卷积得到逐样本相同结果；
  要求的是相同参数下三条业务路径逐样本一致。
- 修改帧移会改变时间网格，因此存在提交帧后 GUI 锁定帧移；帧长、阈值和布局
  可只作用于未提交帧。
- 强制终止发生在临时文件替换前时，旧检查点保持完整；发生在替换后时，新完整
  快照生效。两种情况下都不会出现半份 JSON。

