# 京剧武场离线母线渲染器

这是一个完全离线的确定性渲染组件，用于处理五路京剧武场打击乐单声道
分轨：板鼓、大锣、小锣、铙钹、堂鼓。组件负责场记校验、确定性素材
生成、采样级瞬态对齐、早期反射补偿、立体声下混、BS.1770 响度/真峰值
合规、固定块快照以及强制终止后的续算与逐字节校验。

项目只依赖 `numpy` 和 `soundfile`，支持 Python 3.10 及以上版本：

```bash
python -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

## 快速运行

```bash
# 生成五路 48 kHz / 24-bit WAV、manifest.json 和示例目录
python -m wuchang demo --directory demo

# 一次性完整渲染
python -m wuchang render \
  --manifest demo/manifest.json \
  --out demo/master.wav \
  --report demo/master.report.json \
  --state-dir demo/.render_state \
  --fresh

# 运行验收测试
python -m unittest discover -s tests -v
```

`render` 成功写出 24-bit PCM WAV 母线及稳定 JSON 报告。报告不含时间戳
或绝对路径，字段顺序固定。退出码：

| 退出码 | 含义 |
| --- | --- |
| `0` | 成功，或 Ctrl+C/SIGTERM 完成当前块并提交可恢复快照 |
| `2` | 清单/WAV 校验硬错误 |
| `3` | 恢复母带与一次性母带不一致 |
| `4` | 非预期渲染失败 |

## 清单

`demo/manifest.json` 是完整示例。核心字段：

```json
{
  "schema_version": 1,
  "sample_rate": 48000,
  "bit_depth": 24,
  "duration_seconds": 8.8,
  "block_frames": 4096,
  "target_loudness_lufs": -16.0,
  "true_peak_limit_dbtp": -1.0,
  "channel_search_ms": {"before": 120.0, "after": 120.0},
  "channels": [
    {
      "name": "bangu",
      "instrument": "板鼓",
      "file": "audio/bangu.wav",
      "seed": 1103,
      "start_offset_samples": 731,
      "fallback_alignment_samples": -731,
      "cues": [0.55, 0.90, 1.25],
      "mix_weights": [0.95, 0.88],
      "early_reflection": {
        "enabled": true,
        "delay_ms": 28.5,
        "gain_db": -7.5,
        "window_ms": [20.0, 45.0]
      }
    }
  ]
}
```

- `file` 相对于清单目录解析；清单中的偏移均为整数采样。
- `start_offset_samples` 是生成时注入的物理起始偏移，可由清单直接复算：
  第 `i` 个直接瞬态位于 `round(cues[i] * sample_rate) + start_offset_samples`。
- `fallback_alignment_samples` 是检测失败时直接采用的补偿偏移。
- `mix_weights` 是该通道到立体声左/右声道的下混权重。
- 反射窗口为 `window_ms`，延迟必须位于窗口内；窗口跨度同时决定反射
  衰减包络。

## 处理链路

1. **清单与素材校验**：检查 JSON、必填字段、唯一乐器名、严格升序锣鼓点、
   WAV 存在性、实际采样率、位深和单声道属性。任一硬错误均整体拒绝，
   不写出母线。
2. **确定性合成**：每通道以清单整数种子和事件序号构造独立随机流；每路
   瞬态数量严格等于 `cues` 数量。部分通道叠加由延迟、增益和参考窗口
   包络决定的早期反射，再按整数采样注入起始偏移。同一参数重复生成
   逐字节一致。
3. **瞬态检测与对齐**：检测器在“参考时刻 + 清单默认物理偏移”附近搜索
   导数域中的最陡起音沿，避免长锣尾音盖过新击。通道取各点整数误差中位数，
   反向平移后再次检测，要求最终偏差绝对值不超过 1 采样。检测失败记录
   `DETECTION_FAILED` 质量告警并采用清单默认偏移。
4. **反射补偿**：在声明的窗口内搜索与直达模板最匹配的延迟，以窗口包络
   构造反射基函数并最小二乘估计增益，然后在反射起点减去该基函数。
   补偿不触碰直达区；报告输出延迟、增益、补偿前后窗口能量、直达峰值
   及损失。
5. **立体声下混**：各通道按权重叠加，母线长度取最长通道长度。报告同时
   给出各通道加权能量贡献占比与最终左右声道峰值。
6. **响度与真峰值**：使用 BS.1770 K 加权、400 ms 块、75% 重叠、-70 LUFS
   绝对门限和相对门限计算积分响度；真峰值采用 4 倍过采样。离线前瞻限幅
   使用 5 ms 前瞻，随后显式补偿前瞻延迟，母线采样数保持不变。报告包含
   触发样本数、最大衰减、最终响度和真峰值。

## 快照与恢复

状态目录默认位于输出文件旁的 `.wuchang_state`，包含：

- `state.json`：以“临时文件 + `fsync` + `os.replace`”整体替换，记录清单
  摘要、输入摘要、已完成块数、已确定对齐/反射参数、块哈希等；
- `blocks/*.f32le`：已提交的固定块立体声 float32 数据。

块文件先持久化，快照后提交。强制终止（SIGKILL）最多丢失未提交的当前块。
下次启动校验清单、WAV 哈希、对齐参数、反射参数及所有块哈希，仅复用已
提交块；恢复母带若与一次性确定性母带不一致，会返回
`E_RESUME_DIFFERENCE`，报告首个差异块号及两个块的哈希/帧数，删除不可信
母带并以非零码结束。

若清单或已确定参数不一致，旧状态整体删除，报告
`E_RESUME_MISMATCH`（含块号和 `difference_fields`），本次从零渲染。

Ctrl+C 或 SIGTERM 只设置中断标志，不打断当前块。当前块提交后写出
`interrupted` 恢复报告并以 `0` 退出。`SIGKILL` 无法捕获，因此不保证产生
退出报告，续算只依赖已提交快照。

测试钩子：

- `WUCHANG_TEST_KILL_AFTER_BLOCKS=N`：提交第 N 块后以 137 强制退出；
- `WUCHANG_TEST_BLOCK_DELAY_MS=N`：每块提交后延迟，便于注入 SIGINT/SIGTERM。

## 报告要点

成功报告顶层顺序固定为：

`status`, `schema_version`, `hard_errors`, `warnings`, `manifest`,
`resume`, `channels`, `master`, `loudness`, `render`。

每个通道包含：

- `alignment_offset_samples`、对齐前后检测起点与最终偏差；
- `contribution_percent`；
- `reflection.delay_samples`、`estimated_gain_db`、
  `energy_before_db`、`energy_after_db`、`reduction_db`、
  `direct_peak_loss_db`；
- 检测失败时 `alignment_status = "fallback"`，并在 `warnings` 中记录
  `DETECTION_FAILED`。

## 实测验收（2026-11-18）

环境：Python 3.14.4、NumPy 2.5.5、SoundFile 0.14.0，48 kHz/24-bit 示例
15 个锣鼓点、104 个 4096 帧块。

```text
确定性生成：两次生成五路 WAV SHA-256 完全相同
对齐：板鼓 -731、大锣 -1489、小锣 -2213、铙钹 -2939、堂鼓 -347 采样
对齐后最大偏差：0/0/0/1/0 采样
反射估计：1368 / 2112 / 2784 采样处，增益 -7.500 / -9.000 / -8.500 dB
反射窗口能量下降：116.06 / 116.01 / 119.62 dB
直达峰值损失：0.000 dB（三路）
积分响度：-16.02 LUFS，目标 -16.00，偏差 -0.02 LU
真峰值：-1.14 dBTP，上限 -1.00 dBTP
限幅：已触发，5 ms 前瞻延迟已补偿，422400 帧不变
一次性母带两次 SHA-256：f4e5498e518a7b4e300ab7c62b60c4671b447d1c0ca135493ed556c690492a85
强制终止后恢复母带：与一次性母带逐字节一致
Ctrl+C：完成当前块、提交快照、写出恢复报告、退出 0
恢复差异注入：返回 E_RESUME_DIFFERENCE、块号 60、删除母带、退出 3
```

运行 `python -m unittest discover -s tests -v` 可重复上述路径，包括五类
硬错误码、反射指标、限幅延迟、SIGKILL、SIGINT、`E_RESUME_MISMATCH` 和
`E_RESUME_DIFFERENCE`。

最终回归结果：`Ran 9 tests in 79.130s`，`OK`；`ruff check wuchang tests`
无告警。

