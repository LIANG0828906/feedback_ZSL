# 广播响度在线计值器

本项目交付一个可在本地完全复现的 48 kHz PCM WAV 响度分析与界面演示：

- ITU-R BS.1770-4 K 加权、400 ms 观测块、100 ms 步进。
- 3 s 短期响度、带绝对/相对两阶段门限的积分响度、4 倍过采样真峰值。
- 在线播放使用确定性虚拟传输推进，不打开音频设备。
- 在线与离线共用同一观测块内核；在线按同步标记与离线逐块比较。
- 参数字典版本、受影响块集合、不可变计值快照和整版替换发布。
- JSON 报告原子写入、SHA-256 哈希链校验和中断后已提交块恢复。
- 确定性合成样本、脚本化操作序列、JSONL 轨迹和逐字节复现验证。

## 快速开始

环境已验证为 Python 3.11.18、NumPy 2.4.6、PySide6 6.12.0。以下步骤不需要网络
之外的素材，也不会访问音频设备。

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -e . pytest ruff

# 生成确定性的 12 s、6 声道、24-bit 样本及独立参考
.venv/bin/bl-meter synth \
  --seed 20261120 --channels 6 --duration 12 --bit-depth 24 \
  --out artifacts/sample.wav

# 整段离线分析，报告内包含每个观测块
.venv/bin/bl-meter offline \
  --input artifacts/sample.wav --out artifacts/offline.json

# 脚本化在线/离线验收；不提供 --operations 时使用内置操作序列
.venv/bin/bl-meter demo \
  --seed 20261120 --channels 6 --duration 12 --bit-depth 24 \
  --operations examples/acceptance_operations.json \
  --out-dir artifacts/demo

# 桌面界面
.venv/bin/bl-meter gui --input artifacts/sample.wav --refresh-ms 100
```

本项目已在 Wayland 桌面会话中实际创建并显示 1120x760 窗口；无桌面环境时可使用
`QT_QPA_PLATFORM=offscreen` 做界面烟雾测试。

![在线响度计界面](artifacts/ui_snapshot.png)

## 命令行

入口为 `bl-meter`，也可以使用 `python -m broadcast_loudness`。

### `synth`

生成本地合成唱音、和声与指数扫频。输出 WAV 和同名 `.wav.json` manifest：

- `sha256`、帧数、位深、声道布局。
- 默认参数与独立参考结果。
- 参考路径使用逐样本直接二阶级联滤波和 8 倍频域插值，不依赖在线缓存。

### `offline`

对整段 WAV 计算所有完整 400 ms 观测块，并输出短期、积分前缀、真峰值、窗口
样本边界、同步标记和内容哈希。可配置：

```bash
--target-lufs -23
--true-peak-limit -1
--absolute-gate -70
--relative-gate -10
--downmix identity|stereo
--weights 1,1,1,0,1.41,1.41
```

`stereo` 是内置的 5.1 下混示例：FL、FR、FC、BL、BR 进入立体声，LFE 不参与。

### `demo`

同一素材只创建一个分析会话，按操作序列推进虚拟播放位置，并按独立刷新周期读取
快照。输出：

| 文件 | 内容 |
| --- | --- |
| `sample.wav` | 确定性合成素材 |
| `report.json` | 最终不可变快照分析报告 |
| `checkpoint.json` | 进入最终快进前保存的部分报告，用于恢复演示 |
| `offline.json` | 使用最终参数整段分析的逐块基线 |
| `trace.jsonl` | 刷新序号、播放位置、观测块、计值、版本和哈希 |
| `validation.json` | 在线/离线、参数重算、恢复和产物哈希摘要 |

### 操作脚本

操作脚本可以是操作数组，也可以是包含 `initial_params` 与 `operations` 的对象。
示例见 `examples/acceptance_operations.json`。

| `op` | 字段 | 行为 |
| --- | --- | --- |
| `play` / `pause` | 无 | 切换虚拟播放状态 |
| `advance` | `advance_ms` 或 `advance_samples` | 按固定样本数推进 |
| `seek` | `sample`、`seconds` 或 `block` | 跳转并从已有前缀重建滤波器状态 |
| `restart` | 无 | 播放位置回到 0，保留最后快照 |
| `change_params` | 参数键；可含 `downmix_preset` | 新参数版本与原子重发布 |
| `refresh` | `count` | 只读取已提交快照，并写刷新轨迹 |
| `save_checkpoint` | 无 | 原子保存当前已提交块报告 |
| `advance_to_end` | 无 | 推进到素材末尾 |

播放推进单位是固定样本数，不读取墙钟时间；刷新次数和刷新周期不参与任何计值。

## 计算语义

### WAV 与下混

- 只接受 48 kHz、16-bit 或 24-bit、整数 PCM、1 到 6 声道。
- 支持单声道、立体声、3.0、四声道、5.0 和 5.1。5.1 顺序为
  `L R C LFE BL BR`。
- 下混矩阵为“输出声道 × 输入声道”，可按会话参数修改并写入报告。
- 默认 BS.1770 权重：前置/中置为 1，环绕为 1.41，LFE 为 0。
- 真峰值在未加 K 权重的下混输出上按 4 倍带限插值估计，包含下混矩阵保留的 LFE。
- 不支持的采样率、位深、声道数或 WAV 编码返回 `E_FORMAT_UNSUPPORTED`。

### 观测块与同步

- 每个观测块窗口为 400 ms，即 19,200 样本；步进为 100 ms，即 4,800 样本。
- 只发布完整窗口；文件末尾不足 400 ms 的尾巴不构成观测块。
- 每个块记录 `[window_start_sample, window_end_sample)`、同步标记、内容哈希、
  测量哈希和参数版本。
- 同步标记由素材 SHA-256 前缀和起止样本组成，在线与离线不依赖数组位置或刷新次数。
- 短期响度使用以该块结束样本为右端的精确 3 s 窗口；不足 3 s 的前缀块为 `null`。
- 每个块中的积分响度是该块之前已观测前缀的两阶段门限结果，不会因后续块到达而
  静默改写。最终积分响度取最后已提交块的值。

### K 加权与积分

- 使用 BS.1770-4 的 48 kHz 预滤波和 RLB 双二阶系数。
- 频域实现将零填充后的两个滤波器响应一次作用于前缀，数值上等价于从样本 0
  开始的因果双二阶链；因此跳转后使用的滤波器状态仍由完整前缀连续决定。
- 400 ms 块能量为各下混输出声道 K 加权均方能量乘以声道权重之和。
- 积分响度块响度为 `-0.691 + 10 log10(energy)`。
- 第一阶段剔除低于绝对门限的块；第二阶段以第一阶段平均响度减相对门限为阈值，
  再剔除低于该阈值的块。

## 在线快照与增量参数

`MeterSnapshot` 是不可变对象，包含块元组、参数版本、参数历史、提交边界和缓存统计。
所有在线计算在会话内部完成后，以一次引用替换发布新快照：

- 界面每次刷新只调用一次 `session.view()`，获得一个快照引用和一个位置视图。
- 刷新不会提交块、不会改变积分，也不会触发滤波器计算。
- 界面检测到快照版本变化后清空两条曲线，再用同一快照重建，旧版和新版不会混绘。
- 参数重算期间，已发布快照仍保持原样；重算线程完成后才原子替换。
- 已提交块对象在未受影响时直接复用；旧快照及其块元组保持不可变。

受影响集语义：

| 参数 | 受影响块 |
| --- | --- |
| 目标响度 | 空集；只影响目标比较和报告元数据，不改变测量 |
| 真峰值上限 | 仅超限状态发生变化的块 |
| 绝对/相对门限 | 积分前缀值实际变化的块集合 |
| 声道权重、下混矩阵 | 当前所有已提交块，因为每个块的下混/能量都受影响 |

每次参数发布记录参数版本、变更字段、受影响块、快照版本、测量缓存命中和实际重算数。
普通播放提交只推进增量积分跟踪器，不重算历史前缀。

## 会话与并发边界

- `SessionManager` 按素材 SHA-256 限制同一素材同时只有一个活动会话。
- 重复启动返回 `E_BUSY`。
- 暂停、跳转和关闭不打开临时音频句柄；WAV 在读取后完整驻留内存。
- 关闭只释放会话注册并保留最后快照对象，不删除已提交计值。
- UI 参数重算在 `QThread` 中执行，主线程继续持有旧快照直到完成信号到达。

## 报告、恢复与确定性

报告保存流程为：

1. 在目标目录写同目录临时文件。
2. `flush` 并对文件执行 `fsync`。
3. 使用 `os.replace` 原子重命名。
4. 返回最终文件字节的 SHA-256。

报告包含报告版本、素材 SHA-256、观测参数、当前参数、规则说明、参数历史、
所有已提交块、摘要和 SHA-256 链。每个块的 `chain_hash` 覆盖前一个链头及该块完整
规范化记录。

`load_report` 校验：

- 报告 schema 和版本。
- 链创世值、逐块前驱、逐块链哈希和最终链头。
- 若提供素材，还校验素材 SHA-256、每块内容哈希和同步标记。

任一不匹配返回 `E_REPORT_MISMATCH`，载入路径不会写回或覆盖现有报告。强制终止后，
只能从最近一次已原子写盘的报告恢复其中已有的已提交块；未写盘块不会被承诺重建。

确定性约束：

- 合成器使用固定种子的 `numpy.random.Generator`。
- 报告、轨迹和 manifest 使用排序键、固定分隔符、UTF-8 和固定换行。
- 日志不含时间戳，也不以文件大小作为真值。
- 报告不含输出目录、PID 或临时路径。
- 浮点值由同一 NumPy/Python 算法路径产生，在线和离线共用观测块缓存和哈希定义。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_FORMAT_UNSUPPORTED` | WAV 格式、采样率、位深或声道布局不支持 |
| `E_BUSY` | 同一素材已有活动分析会话 |
| `E_LUFS_DIVERGENCE` | 在线与离线在首个观测块发生超差 |
| `E_REPORT_MISMATCH` | 报告版本、哈希链、素材或内容哈希不匹配 |
| `E_INVALID_PARAMETER` | 参数范围、矩阵形状或会话状态非法 |

## 测试与实测结果

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest
.venv/bin/ruff check broadcast_loudness tests
```

当前结果：

- `27 passed`。
- `ruff check` 全部通过。
- 12 s、6 声道、24-bit、种子 `20261120` 的脚本演示：117 个观测块，
  在线与离线逐块比较 `max_abs_delta = 0.0`，声明容差 `1e-6`。
- 最终积分响度：`-13.314905342778777 LUFS`。
- 参数演示：目标响度变更重算 0 块；真峰值上限变更只影响 10 块且测量重算 0 块；
  声道权重变更使当时 30 个已提交块全部重算，重算测量块数为 30。
- 部分报告恢复出 39 个已提交块；最终报告 `117` 块通过完整哈希链校验。
- 同一操作序列双跑，`sample.wav`、`report.json`、`trace.jsonl`、`offline.json`
  和 `checkpoint.json` 的 SHA-256 均逐字节一致。

完整机器可读结果在 `artifacts/validation/validation.json`，最终报告在
`artifacts/validation/report.json`，轨迹在 `artifacts/validation/trace.jsonl`。

报告、轨迹和离线基线的主要 SHA-256：

```text
sample.wav    185b0f79708e51f625129dbd33c31896d8a20884e494359695210c7821fe50a8
report.json   614b2a4617973bacec6118fcbef6d1c55a62df5ee811017363a01506e1d0df5b
trace.jsonl   ea8cc49053dbb5f5788e09d0c9c55391e21613d221d9d0813891ff13bbd0df37
offline.json  539edf11cbc2ec8dea5d6ca4e32206638653bee3494951e3bfac318e7ce9a181
```

其中最终报告链头为
`345b1d5aa6fb060fce28398d22c17d877899ad6cc9e45eaf06290e2a80d68f4f`。
