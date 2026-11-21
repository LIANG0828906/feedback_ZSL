# 虚拟演出场馆空间声学声场重建工作台

本项目实现一条完全本地、确定性的 5.1 声场重建链路：

```text
16-bit PCM WAV + 场馆几何
  -> ir_load
  -> direct_alignment
  -> early_reflections
  -> decay_model
  -> late_reverb
  -> spatial_synthesis
  -> field_coefficients
  -> 5.1 可听化 WAV
```

实现包含内容寻址缓存、影响因子失效、原子版本发布、陈旧系数拦截、异常注入、协作式取消和监听点级并发租约。所有样本和测试信号均由本地生成器产生，不访问网络或外部设备，不使用 Docker。

## 快速运行

### 依赖

```bash
python3 --version
python3 -m venv .venv
. .venv/bin/activate
python -m pip install --upgrade pip
pip install numpy pyqt5 matplotlib
```

当前验证环境使用 Python 3.14.4；代码兼容 Python 3.9+。若系统禁止直接 `pip install`，可使用：

```bash
uv venv --python python .venv
uv pip install --python .venv/bin/python numpy pyqt5 matplotlib
```

### 生成样本并建立基线

```bash
.venv/bin/python -m acoustic_workbench.cli sample \
  --workspace artifacts/demo/samples \
  --seed 20260101
```

会生成小型录音室、中型音乐厅、大型场馆三套样本。每套包含：

- 48 kHz、16-bit、6 声道 WAV 脉冲响应；
- `scene.json`：几何、声源/听音点、材质查表、频带、窗参数和渲染配置；
- `baselines/<listener>.json`：本地基线声场系数版本；
- `manifest.json`：场景、WAV 和基线的内容哈希。

### 重建与渲染

```bash
.venv/bin/python -m acoustic_workbench.cli rebuild \
  --workspace artifacts/demo/samples/scenes/small \
  --listener L1 --summary

.venv/bin/python -m acoustic_workbench.cli render \
  --workspace artifacts/demo/samples/scenes/small \
  --listener L1 \
  --output renders/L1.wav
```

### 异常注入

```bash
.venv/bin/python -m acoustic_workbench.cli rebuild \
  --workspace artifacts/demo/samples/scenes/small \
  --listener L1 \
  --inject ir_invalid
```

支持：

| 注入参数 | 业务码 |
|---|---|
| `ir_invalid` | `E_IR_INVALID` |
| `decay_negative` | `E_DECAY_NEGATIVE` |
| `position_out_of_bounds` | `E_POSITION_OUT_OF_BOUNDS` |
| `channel_layout_missing` | `E_CHANNEL_LAYOUT_MISMATCH` |

注入发生在输入边界，不修改处理链路源码。错误 JSON 包含 `code`、`message`、`injection_point` 和 `details.detected_at`。注入验证在发布任何新场系数或写渲染片段之前完成。

### GUI

```bash
.venv/bin/python -m acoustic_workbench.cli gui \
  --workspace artifacts/demo/samples/scenes/small
```

PyQt5 工作台支持监听点选择、重建、5.1 渲染和逐通道 RMS 系数图。命令行与 GUI 共用同一引擎和缓存。

## 算法与产物

### 直达声对齐

- 逐声道读取 WAV，以每通道峰值乘以门限得到检测门限；
- 首个越过门限的样本作为原始 WAV 帧索引中的直达声到达样本；
- 以听音点 `reference_channel` 为基准做整数样本移位；
- 将各声道直达声峰值归一到参考声道，并记录每通道增益；
- 中间产物的真值来源包含原始 WAV `sha256`、原始帧索引和声道布局；
- 门限或参考声道变化会使 `direct_alignment` 及其下游失效，轨迹日志输出新旧
  `source_sample_index`、`aligned_sample_index` 及差值。

### 早期反射与衰减模型

- 在直达声窗之后、早期窗口结束之前寻找连续越门限区域，取区域内峰值作为反射事件；
- 每个事件记录声道、到达样本、相对直达声时间、幅度和原始 WAV 样本索引；
- 对事件附近窗口做确定性 FFT，按倍频程积分频带能量并输出 dB 摘要；
- 对早期响应做分帧 FFT，计算倍频程带能量与 Schroeder 衰减曲线；
- 拟合 dB/s 斜率，并结合 Sabine 估算与材质吸收查表得到每频带 T60；
- 材质、频带、窗参数或几何变化会使 `decay_model` 失效；轨迹日志列出失效节点、
  重算原因、受影响反射事件集合和实际变化事件。

### 晚期混响与多通道拼接

- 以频带 T60 构造确定性指数衰减包络；
- 使用每条链路的稳定哈希作为 PCG64 种子，做频带谱合成；
- 早期响应与晚期尾按声道拼接，通道长度、5.1 布局和时间对齐逐项校验；
- 全部通道先在内存中组装成功后才写缓存；任一声道失败返回
  `E_SYNTHESIS_FAILED`，不会写半成品 WAV；
- 声场系数包含每通道 RMS/峰值增益、延迟、早晚期能量比、频带能量、空间权重、
  融合 T60 和滤波器摘要。

### 渲染

- 从本地确定性正弦/噪声测试信号出发，逐通道做 FFT 卷积；
- 渲染前重新计算当前场景的节点指纹，并与已发布系数的 `dependency_manifest`
  逐节点比较；
- 版本内容哈希或任一依赖不一致时返回 `E_FIELD_STALE`，不写输出；
- 所有声道卷积完成后再原子写 5.1 WAVE_FORMAT_EXTENSIBLE WAV。

## 增量更新

影响因子包括：

```text
ir_file, channel_layout, alignment, window_params, frequency_config,
source_position, listener_position, material, decay_config, reverb_config
```

最早失效阶段映射：

| 变化 | 最早失效节点 |
|---|---|
| WAV 内容、声道布局 | `ir_load` |
| 门限、参考声道、增益归一 | `direct_alignment` |
| 窗参数、频带配置、声源/听音点位置 | `early_reflections` |
| 材质、衰减配置 | `decay_model` |
| 混响配置 | `late_reverb` |

每个节点指纹由节点类型、算法 schema、上游节点指纹和节点相关参数共同决定。未失效节点直接从内容寻址缓存读取；若缓存缺失或损坏，则保守重算并记录 `conservative_recomputes`。不同听音点的节点命名空间和任务租约相互独立。

示例：仅修改墙面材质时，实际轨迹为：

```json
{
  "factors_changed": ["material"],
  "earliest_affected_node": "decay_model",
  "recomputed_nodes": [
    "decay_model",
    "late_reverb",
    "spatial_synthesis",
    "field_coefficients"
  ],
  "cache_hits": ["ir_load", "direct_alignment", "early_reflections"]
}
```

## 缓存、发布与恢复

```text
<workspace>/cache/blobs/<sha256-prefix>/<sha256>.npy
<workspace>/cache/nodes/<node-fingerprint>.json
<workspace>/cache/fields/<field-version>.json
<workspace>/cache/published/<listener>.json
<workspace>/cache/state/<listener>.json
<workspace>/logs/trajectory.jsonl
<workspace>/renders/<listener>.wav
```

- 数组 blob 按 SHA-256 内容寻址并原子写入。
- 节点元数据最后写入，读取时校验指纹、blob 哈希、形状和结果哈希。
- 新场系数先写版本文件，再以一次 `os.replace` 原子替换监听点发布指针。
- 取消或异常不会替换发布指针，上一次已提交系数保持可读。
- 强制终止后仅以磁盘节点、发布指针和版本文件为恢复依据；不生成也不承诺退出报告。

## 并发

- 同一监听点的重建与渲染共享一个非阻塞文件租约；
- 进程内重复提交立即返回 `E_BUSY`，跨进程使用 `flock` 保护；
- 取消令牌在阶段边界、早期事件循环、衰减帧循环、混响/拼接/渲染通道循环设置检查点；
- 无论成功、取消或异常，租约和文件句柄都在 `finally` 中释放。

## 轨迹日志

`logs/trajectory.jsonl` 每行都是排序键、无空白的规范 JSON，包含：

- 操作序号、操作类型、监听点和状态；
- 变化影响因子、原因、最早失效节点、失效节点集合；
- 重算节点、缓存命中、保留节点和保守重算原因；
- 每个节点指纹和节点结果内容哈希；
- 原始 WAV 哈希、最终场版本、滤波系数哈希；
- 到达样本差异、反射事件影响、渲染输出哈希；
- 失败/取消业务码和注入定位。

日志不包含时间戳，也不把文件大小作为真值。相同输入和操作序列生成的日志、场系数 JSON 与渲染 WAV 逐字节一致。

## 验证结果

执行：

```bash
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -m acoustic_workbench.cli diagnose \
  --workspace artifacts/verification_workspace
```

实际结果：

- 11 项单元/集成测试全部通过；
- 12 项诊断检查全部通过；
- 三套场馆样本及 L1/L2 基线成功建立；
- 材质变更仅重算 `decay_model` 之后四个节点，前三个节点命中缓存；
- 陈旧系数渲染返回 `E_FIELD_STALE` 且未写文件；
- 四类注入分别返回独立业务码且已发布系数字节不变；
- 占用监听点租约时重复提交返回 `E_BUSY`；
- 在早期反射后取消返回 `E_CANCELLED`，上次发布系数字节不变；
- 两次独立工作区的轨迹日志、场系数和渲染 WAV SHA-256 完全一致。

诊断报告与样例产物：

- `artifacts/verification/diagnostic_report.json`
- `artifacts/verification/small_trajectory.jsonl`
- `artifacts/verification/sample_manifest.json`
- `artifacts/verification/deterministic_field.json`
- `artifacts/verification/deterministic_render.wav`
- `artifacts/verification/filter_849aeb4bf0c94f6ddd3f059a30623781f51024bef748d48d6a39cdaeaa4f9821.npy`

本次确定性重放摘要：

```text
field      8e817530cfeb4045acae3478370159b7c542ecbe4cbff43279ef9ae75b6c2c52
trajectory 11d6652779b276c6aaed75b4658d6a43cafda697dc79ab1513eb9e8493a7afa7
render     b7b926e7a63f6a9ad55576b9e78efe5e851d54ef397e1a1afe80a3eb33d973ad
```

## 代码结构

| 文件 | 职责 |
|---|---|
| `acoustic_workbench/scene.py` | 场景校验、材质查表、影响因子和节点指纹 |
| `acoustic_workbench/wavio.py` | PCM WAV 解析、内容哈希、原子 5.1 WAV 写出 |
| `acoustic_workbench/dsp.py` | 对齐、反射、FFT 频带、衰减、混响、拼接、卷积 |
| `acoustic_workbench/cache.py` | 内容寻址节点缓存和字段原子发布 |
| `acoustic_workbench/engine.py` | 重建编排、增量失效、渲染版本校验和轨迹日志 |
| `acoustic_workbench/injection.py` | 输入边界异常注入与统一校验 |
| `acoustic_workbench/locks.py` | 监听点级并发租约 |
| `acoustic_workbench/samplegen.py` | 多场馆确定性 WAV/场景/基线生成器 |
| `acoustic_workbench/diagnostics.py` | 脚本化端到端诊断 |
| `acoustic_workbench/gui.py` | 可选 PyQt5 + matplotlib 工作台 |
| `tests/test_workbench.py` | 对齐、增量、异常、取消、原子写入和确定性测试 |

