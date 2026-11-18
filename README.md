# 离线有声书母带渲染器

这是一个只依赖 Python 3.11 与 NumPy 1.26 的确定性有声书后期组件。它读取章节规则和分包 WAV 清单，完成规则校验、响度测量、章级电平对齐、安静段底噪保护、真峰值保护、跨章拼接、块级快照恢复，并输出整本 WAV 母带和分章 JSON 报告。

运行过程不使用 FFmpeg、外部媒体、网络或其他容器环境。

## 环境

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements.txt pytest
.venv/bin/python -m pytest -q
```

本工作区验证版本：Python 3.11.18、NumPy 1.26.4。

## 命令行

```bash
# 确定性生成清单中的合成分包
.venv/bin/python -m audiobook_master generate \
  --rules rules.json --manifest manifest.json

# 校验并输出每个分包的有效采样数、峰值、真峰值、声道数
.venv/bin/python -m audiobook_master inspect \
  --rules rules.json --manifest manifest.json

# 完整渲染
.venv/bin/python -m audiobook_master render \
  --rules rules.json --manifest manifest.json \
  --out-master book.wav \
  --out-report report.json \
  --state-dir book.state

# 从最终母带复算报告中的响度、长度和真峰值
.venv/bin/python -m audiobook_master verify \
  --master book.wav --report report.json
```

`render` 的 `--state-dir` 可省略，默认是母带同目录下的 `.母带名.state`。测试钩子 `--interrupt-after-blocks N` 与真实 `SIGINT`/`SIGTERM` 走同一协作中断路径。

## 输入格式

`rules.json` 示例：

```json
{
  "sample_rate": 48000,
  "bit_depth": 24,
  "target_loudness_lufs": -18.0,
  "relative_gate_lu": -10.0,
  "true_peak_dbtp": -1.0,
  "max_noise_floor_rise_db": 6.0,
  "boundary_tolerance_db": 0.5,
  "block_frames": 4096,
  "chapters": [
    {"id": "ch01", "expected_duration_seconds": 2.2},
    {"id": "ch02", "expected_duration_seconds": 2.0},
    {"id": "ch03", "expected_duration_seconds": 2.4}
  ]
}
```

`manifest.json` 示例：

```json
{
  "chapters": [
    {
      "id": "ch01",
      "packages": [
        {
          "file": "audio/ch01.wav",
          "channels": 2,
          "sample_rate": 48000,
          "batch": "light"
        }
      ]
    }
  ]
}
```

同一章可有多个分包。分包未写 `duration_seconds` 时，会按章期望时长等分；也可用 `expected_samples` 声明。章顺序由规则文件决定。输入 WAV 必须是无压缩整数 PCM，支持 16/24/32 bit。

校验失败统一输出 `E_CONFIG_INVALID` JSON 诊断并返回退出码 2，不写母带。诊断中的 `path` 指向具体字段，例如：

```json
{"ok":false,"error":{"code":"E_CONFIG_INVALID","message":"package frame count does not match the declared duration","path":"manifest.chapters[0].packages[0].expected_duration_seconds"}}
```

## 处理策略

- 响度测量使用 BS.1770 K 加权、400 ms 块、100 ms 步长、绝对门限 -70 LUFS 和规则相对门限。
- 每章先做电平对齐，再用全书统一校正量把整本响度收敛到目标；报告同时给出章级决策增益、全书校正量、静态增益和有声区额外增益。
- 安静段只承受不超过规则上限的静态增益。超过底噪预算的部分由平滑 20 ms 活动包络只施加到有声区，避免把噪声一起抬升。
- 真峰值保护采用 20 ms 前视、快速攻击/缓慢释放的增益限幅，并用 4 倍 Catmull-Rom 插值复检。最终 PCM 的样本绝对值和实测真峰值均不得超过规则上限。
- 相邻章电平差超出容差时，只衰减较响一侧的章尾或章首，使用渐变恢复，不增删、复制或重叠任何帧。
- 母带为规则指定采样率/位深的规范 RIFF PCM WAV；报告中的帧数、RIFF 长度、数据长度、响度、真峰值和边界电平均可由最终母带复算。

## 快照与恢复

状态目录包含：

- `render.pcm`：已提交的量化 PCM 前缀。
- `snapshot.json`：当前进度快照，使用校验和信封和临时文件原子替换。
- `commits/NNNNNNNNNNNN.json`：每次块提交的不可变归档。

每块顺序为：追加 PCM、`fsync`、更新 SHA-256、原子提交归档快照、原子替换当前快照、检查协作中断。

- `SIGINT`/`SIGTERM`：完成当前块和快照，写 `status:"interrupted"`、`E_INTERRUPTED` 恢复报告，退出码 0。
- `SIGKILL` 等强制终止：不保证写退出报告；下次启动忽略并截断未提交的 PCM 尾部。
- 当前快照损坏：校验和或 PCM 前缀不一致时自动扫描最近的提交归档并回退，不需要人工清理。
- 规则哈希或输入摘要变化：整体删除旧状态，以 `E_RESUME_MISMATCH` 记录在最终报告和恢复来源中，并从零重渲。
- 一致状态：从下一块继续。已提交的量化 PCM 字节直接复用，续跑母带与一次性渲染逐字节一致。

## 报告

顶层字段顺序固定为 `schema_version`、`status`、`business_code`、`master`、`rules`、`loudness`、`chapters`、`boundaries`、`peak`、`validation`、`recovery`。报告不包含时间戳或绝对路径。

每章包含：

- `before_loudness_lufs`、`after_loudness_lufs`
- `gain_db`、`static_gain_db`、`active_region_gain_db`、`effective_gain_db`
- `peak_protection_samples`、`limiter_active_samples`
- `noise_floor_before_dbfs`、`noise_floor_after_dbfs`、`noise_floor_rise_db`
- `sample_peak_after_dbfs`、`true_peak_after_dbtp`

`validation` 同时检查全书响度、真峰值、每章峰值保护、底噪上限、边界容差、总帧数和连续硬削风险。

## 实际验证

测试覆盖六组端到端场景：

```text
.venv/bin/python -m pytest -q
......                                                                   [100%]
6 passed
```

验证内容包括：

- 同一规则/清单重复生成的 WAV SHA-256 一致。
- 非法目标响度、正 dBTP、时长不符、同章采样率不一致均返回 `E_CONFIG_INVALID` 和字段路径，且没有母带。
- 完整渲染达到全书响度、真峰值、底噪、边界、总帧数要求。
- 5 块后中断、追加未提交 PCM 尾部、续跑后母带 SHA-256 与一次性渲染一致。
- 破坏当前快照后自动从提交归档恢复，母带 SHA-256 仍一致。
- 修改规则后检测 `E_RESUME_MISMATCH` 并重渲，结果与新状态完整渲染一致。
- 子进程收到真实 `SIGTERM` 后完成当前块、写中断报告、退出码 0，续跑成功。

固定三章演示（48 kHz/24 bit 立体声，目标 -18 LUFS，真峰值 -1 dBTP，底噪上限 6 dB，边界容差 0.5 dB）的最终代码实测：

```text
全书响度             -18.013 LUFS
三章响度             -17.953 / -17.935 / -17.934 LUFS
三章底噪抬升          5.9999 / 2.2058 / 5.9999 dB
三章实测真峰值        -1.020 / -1.000 / -1.020 dBTP
边界电平差            0.000 / 0.000 dB
母带帧数              316800（= 105600 + 96000 + 115200）
一次性渲染 SHA-256    5f913973ee66cf50764e5b8aa70c791107a350ada1581ec9da876123c9bd6021
第 13 块续跑 SHA-256  5f913973ee66cf50764e5b8aa70c791107a350ada1581ec9da876123c9bd6021
```

上述数值来自本工作区的本地确定性合成素材；没有使用参考母带、外部媒体或网络。

## 实现说明

- 非母带采样率的输入使用确定性线性重采样后再量化，目标是离线可复现；若制作环境需要更高阶重采样质量，可在 `config.py` 中替换该步骤而不改变状态和报告协议。
- 真峰值使用 4 倍插值估计，并在限幅后以同一口径复检；规则上限以该离线口径执行。
- 章边界只做电平匹配，不进行重叠交叉淡化，因此总帧数严格等于各章帧数之和。
