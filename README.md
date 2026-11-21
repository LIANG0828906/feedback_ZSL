# 5.1 声道块级响度分域诊断引擎

本仓库实现一套可在 CPU 上离线运行的 5.1 PCM 响度诊断链路。它把
K 加权、观测块切片、块级短期响度与 4 倍过采样真峰值、两阶段门限、
积分响度、并行调度、不可变版本查询和断点恢复拆成可独立观测的模块。
全部输出使用规范 JSON，不写入时间戳；相同样本、参数和操作序列产生
逐字节一致的报告与轨迹。

## 快速开始

要求 Python 3.10+ 与 NumPy。

```bash
uv venv --python 3.10 .venv
uv pip install -r requirements.txt

.venv/bin/python -m loudness_diag generate \
  --output artifacts/demo/samples --seed 20261122

.venv/bin/python -m loudness_diag diagnose \
  --wav artifacts/demo/samples/program_51_16bit.wav \
  --output artifacts/demo/report \
  --asset-id program_16 \
  --block-samples 19200 \
  --config config.example.json
```

输出：

- `report.json`：素材证据、逐块能量/响度/真峰值、门限状态、积分结论和内容哈希。
- `trace.log`：每行一个规范 JSON 事件，包含调度波次、内存预算、块号、筛选状态和哈希。

一键验收：

```bash
.venv/bin/python scripts/run_acceptance.py
.venv/bin/python -m unittest discover -v
uv tool run ruff check loudness_diag tests
```

## 模块与数据流

```text
WAV 严格校验
  -> 原始声道序列证据（计数/峰值/SHA-256）
  -> BS.1770 K 加权（48 kHz 两级双二阶）
  -> 可配置样本窗口切片（同步标记使用样本位置）
  -> 每块：未门限能量、短期响度、4x 真峰值、内容哈希、计值哈希
  -> 绝对门限
  -> 绝对通过块的均值推导相对门限
  -> 绝对 + 相对最终筛选
  -> 门限后积分响度
```

真峰值上限是独立合规判据，不会改变积分响度；每个块同时输出
`true_peak_pass` 和 `compliance_pass`。

## 1. WAV 读取与下混

`loudness_diag.wav.read_wav()` 仅接受以下输入：

- `WAVE_FORMAT_EXTENSIBLE` PCM；
- 48000 Hz；
- 16-bit 或 24-bit，且 `valid_bits == container_bits`；
- 6 声道，声道掩码为 `0x003F`（FL/FR/FC/LFE/BL/BR）或
  `0x060F`（FL/FR/FC/LFE/SL/SR）；
- fmt、block align、byte rate、data 长度互相一致。

任一矛盾返回 `E_HEADER_INVALID`。读取结果保留每个声道的原始 PCM 字节，
并输出：

- `sample_count`
- 归一化 `peak` 与整数 `peak_raw`
- 声道原始样本 SHA-256
- 素材内容 SHA-256

下混采用 BS.1770 功率增益 `G = {1,1,1,0,1.41,1.41}`。诊断波形为
`sum(sqrt(G_i) * x_i)`，LFE 权重为 0。门限能量不会用该单波形近似，
而是逐声道计算 `sum(G_i * mean(x_i^2))`，避免声道交叉项污染。

哈希定义：

- 声道哈希：该声道 channel-major 原始 PCM 字节的 SHA-256。
- 素材哈希：域分隔字符串、声道数、每声道序号/长度和原始 PCM 字节的 SHA-256。
- 块内容哈希：域分隔字符串、声道数、每声道序号/长度和窗口原始 PCM 字节的 SHA-256。
- 块计值哈希：引擎版本、过采样倍数、窗口、内容哈希、能量、响度和真峰值的规范 JSON SHA-256。

## 2. K 加权与块级计值

48 kHz 系数采用 ITU-R BS.1770-4：

```text
Stage 1 b = [1.53512485958697, -2.69169618940638, 1.19839281085285]
        a = [1, -1.69065929318241, 0.73248077421585]
Stage 2 b = [1, -2, 1]
        a = [1, -1.99004745483398, 0.99007225036621]
```

K 加权在完整声道上执行，以保持双二阶状态跨块连续；调度器再按块并行计算。
若需要重新切片，调用：

```python
from loudness_diag import compare_slicing, read_wav

wav = read_wav("program.wav")
comparison = compare_slicing(
    wav,
    old_block_samples=19200,
    new_block_samples=9600,
    old_oversample=4,
    new_oversample=4,
)
```

结果包含 `block_count_delta`、`boundary_deviation_samples`、每个旧块到新块的
重叠区间/窗口偏差，以及 `content_hash_equal`、`evaluation_hash_equal` 映射。
最后不足一个块的部分保留为 `complete: false`，能量按其实际样本数计算。

真峰值使用确定性的 4 倍窗函数 sinc 多相插值。0 相位严格等于原始样本，
其余相位为带限插值；每块取所有原始声道（含 LFE）过采样序列的最大绝对值。

## 3. 两阶段门限与可重建报告

对块 `b`：

```text
z_b   = sum_i G_i / N_b * sum_n y_i[n]^2
L_b   = -0.691 + 10*log10(z_b)
T_abs = 10^((absolute_lufs + 0.691) / 10)
```

先做绝对门限 `z_b >= T_abs`。再对绝对通过块求均值能量并计算第一阶段响度，
加上 `relative_offset_lu` 得到相对门限。最终通过条件为：

```text
absolute_pass and relative_pass
```

报告逐块给出：

- `ungated_energy`
- `short_term_loudness`
- `true_peak` / `true_peak_dbtp`
- `absolute_pass` / `relative_pass` / `final_pass`
- `true_peak_pass` / `compliance_pass`
- `removed_reason`：`absolute_gate`、`relative_gate`、`true_peak_limit` 或 `null`
- `block_id`、窗口、同步标记、内容哈希和计值哈希

`gate.removed_block_ids`、`gate.gated_block_ids` 和
`decision_by_block_id` 可直接定位每个块。`reconstruct_gate_decisions()` 可从
块 ID、块能量和两个门限能量重建全部筛选结论，不需要总积分值。

积分响度仅对最终通过块的能量取均值，再换算为 LUFS。没有通过块时，
积分响度输出 `null`。

## 4. 批量参数实验

```bash
.venv/bin/python -m loudness_diag batch \
  --wav artifacts/demo/samples/program_51_24bit.wav \
  --output artifacts/demo/batch \
  --asset-id program_24 \
  --block-samples 9600 \
  --params artifacts/demo/params.json \
  --config config.example.json
```

参数文件：

```json
{
  "combinations": [
    {"absolute_lufs": -70.0, "relative_offset_lu": -10.0, "true_peak_limit_dbtp": -1.0},
    {"absolute_lufs": -50.0, "relative_offset_lu": -4.0, "true_peak_limit_dbtp": -12.0}
  ]
}
```

输出包含每个组合的积分响度、最大真峰值、通过计数、逐块
`status_matrix`，以及至少一个状态维度发生翻转的
`flipped_block_ids`。组合按输入序列编号为 `0000`、`0001`……

## 5. 并行调度

配置字段：

```json
{
  "parallelism": 2,
  "memory_budget_bytes": 524288,
  "block_memory_bytes": 131072
}
```

- `parallelism`：调度器全局线程上限，不会在运行时提高。
- `memory_budget_bytes`：单次素材调度窗口的累计内存预算。
- `block_memory_bytes`：每个块任务的预算占用估计。
- `block_memory_bytes > memory_budget_bytes` 直接返回 `E_CONFIG_INVALID`。

块号先稳定排序，再按提交序号组成可运行波次。波次内并行执行，结果始终按块号
返回。因并行度或累计内存不足而延迟的块在轨迹中记录
`wait_reason: "parallelism"` 或 `"memory_budget"`。同一素材正在运行时再次启动
返回 `E_BUSY`。为保证日志可复现，同一调度器一次只派发一个素材，但该素材的块
任务使用固定全局并行度。

调度轨迹不依赖线程调度顺序：提交、排队、波次开始和结束都按提交序号记录；
并行与串行的逐块结果完全相同。

## 6. 版本化发布与隔离查询

```python
from loudness_diag import VersionedStore

store = VersionedStore("artifacts/versions")
v1 = store.publish(report_v1)
v2 = store.publish(report_v2)

with store.query(v1) as snapshot:
    assert snapshot.version_hash == v1
    old_report = snapshot.report
```

对应 CLI：

```bash
.venv/bin/python -m loudness_diag publish \
  --report artifacts/demo/report/report.json --versions-root artifacts/demo/versions
.venv/bin/python -m loudness_diag query \
  --versions-root artifacts/demo/versions --version <version_hash>
```

每个版本写入 `versions/<version_hash>.json`，发布清单原子换成
`publish_manifest.json`。版本哈希为域分隔字符串加规范报告字节的 SHA-256；
清单自身含 `manifest_hash`。查询期间旧版本文件保持只读，发布不会修改旧文件。
查询返回实际命中的 `version_hash` 和 `manifest_hash`，并在句柄关闭后释放引用。

以下情况返回 `E_VERSION_MISMATCH`：

- 清单哈希不匹配；
- 请求版本不在清单；
- 版本文件哈希或规范 JSON 字节不匹配；
- 清单记录的报告哈希与实际报告不一致。

## 7. 原子快照与强制终止恢复

快照目录结构：

```text
snapshots/<asset>-<asset_hash>-<params_hash>/
  meta.json
  manifest.json
  blocks/00000000.json
  blocks/00000001.json
  ...
  gate.json
```

所有 JSON 都先写入同目录临时文件，`fsync` 后用 `os.replace()` 原子重命名。
每个块记录包含 `prev_hash` 和 `record_hash`，首块链接参数/素材 genesis。
`manifest.json` 记录已提交块范围、头哈希和每块记录哈希，自身也有
`manifest_hash`。门限两阶段中间值单独写入 `gate.json` 并纳入清单。

恢复流程只依赖磁盘：

1. 校验 meta、参数版本、素材哈希、清单哈希和块哈希链。
2. 扫描块文件；允许原子重命名前遗留的临时文件，并忽略它们。
3. 若块文件已落盘但清单尚未更新，将其作为已提交尾块收养，不重算。
4. 从第一个未提交块继续；已提交块文件绝不覆盖。
5. 字段缺失、序号缺口、清单/块哈希或链哈希不一致时返回
   `E_RECOVERY_INVALID`，并通过 `first_broken_block` 定位首个断点。

`SnapshotStore.resume()` 的回调只会收到未提交块号；测试中通过真实
`SIGKILL` 终止子进程，再重新载入并验证回调只包含剩余块。

CLI 首次运行和恢复：

```bash
.venv/bin/python -m loudness_diag diagnose ... --snapshot-root artifacts/demo/snapshots
.venv/bin/python -m loudness_diag recover  ... --snapshot-root artifacts/demo/snapshots
```

## 8. 确定性与轨迹

- JSON 统一使用排序键、无空格分隔符和 UTF-8 编码。
- 不使用时间戳；块同步标记为 `sync:<asset>:<block_id>:<start>-<end>`。
- 相同输入、参数、配置和操作序列下，报告、批量报告、版本文件和轨迹逐字节相同。
- 轨迹中的块事件包含素材标识、块序号、未门限能量、筛选状态、并行度、
  预算用量和块内容哈希。

## 错误码

| 业务码 | CLI 退出码 | 含义 |
|---|---:|---|
| `E_HEADER_INVALID` | 2 | WAV 头、位深、声道、掩码或数据长度无效 |
| `E_BUSY` | 3 | 同一素材已在运行 |
| `E_CONFIG_INVALID` | 4 | 配置、块长或过采样参数无效 |
| `E_VERSION_MISMATCH` | 5 | 版本哈希与发布清单不一致 |
| `E_RECOVERY_INVALID` | 6 | 快照字段缺失或哈希链断裂 |
| `E_SNAPSHOT_RELEASED` | 7 | 已释放的快照句柄仍被读取 |

错误以规范 JSON 写到 stderr，例如：

```json
{"error":{"code":"E_HEADER_INVALID","message":"..."},"ok":false}
```

## 测试与实际验证

测试集覆盖：

- 16/24-bit 5.1 WAV、逐声道证据、LFE 下混、4x 真峰值；
- 头字段矛盾与业务码；
- 块级能量/响度/真峰值、两阶段门限和块 ID 重建；
- 并行与串行逐块相等、内存等待轨迹、重复启动 `E_BUSY`；
- 重切片块数差异、窗口偏差和哈希映射；
- 批量参数翻转与逐字节确定性；
- 旧版本隔离查询、清单/版本文件篡改；
- 快照字段缺失/断链定位、只补算未提交块；
- 真实 `SIGKILL` 后仅依赖磁盘恢复；
- CLI 两次运行的报告、批报告和轨迹逐字节相等。

本机最近一次验证：

```text
$ .venv/bin/python -m unittest discover -v
Ran 21 tests in 3.818s
OK

$ uv tool run ruff check loudness_diag tests
All checks passed!
```

持久化验收实验可通过 `scripts/run_acceptance.py` 重放；摘要写入
`artifacts/acceptance/acceptance_summary.json`，其中包含报告/版本哈希、
并行一致性、版本隔离、恢复补算块号和 `summary_hash`。

最近一次验收摘要：

```text
ok                                  true
diagnose_byte_identical             true
parallel_serial_blocks_equal        true
batch_byte_identical                true
version_isolation                   true
recovery_only_missing               true
block_count / block_samples         10 / 9600
reslice_block_count_delta           -5
snapshot_committed_before           2
snapshot_computed_indices           [2,3,4,5,6,7,8,9]
report_hash                         dd6b962d106462c3c44ca1e688b0697c3ea666e87d616cc535f699dcbc916ba0
summary_hash                        31ad1c6ef8ad4339d880ffb64d4439b672215995bce9a4fd3f551ec7e446fc5a
```

## 边界说明

- 这是可解释的诊断引擎，不宣称替代经过认证的广播响度计。
- 当前输入范围固定为 48 kHz、16/24-bit、6 声道 extensible PCM；
  其它采样率、位深、普通非 extensible PCM 或缺失声道掩码均被拒绝。
- 真峰值采用确定性带限插值实现；跨平台的浮点 JSON 字节一致性以同一
  Python/NumPy 构建为前提。
