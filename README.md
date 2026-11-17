# 梆子腔五路分轨对齐与母带工具

该项目读取场记清单与五路 48 kHz 单声道 WAV，完成逐段板式重建、梆子锚点对齐、帮腔进入检测与重叠区声部增益分离，并输出可续算的立体声母带、稳定字段顺序的 JSON 报告和原子进度快照。

全部测试素材由代码按整数种子确定性生成，运行时不访问网络，不需要真实录音或外部付费素材。

## 环境

- Python 3.11
- NumPy
- SciPy

Windows 10/11：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -U pip
.\.venv\Scripts\python -m pip install numpy scipy
.\.venv\Scripts\python -m pip install -e .
```

Linux/macOS：

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install -U pip
.venv/bin/python -m pip install numpy scipy
.venv/bin/python -m pip install -e .
```

## 快速运行

```bash
.venv/bin/python -m bangzi_align generate --out demo --seed 237 --force
.venv/bin/python -m bangzi_align validate --manifest demo/manifest.json
.venv/bin/python -m bangzi_align render --manifest demo/manifest.json \
  --out-dir demo/output --block-samples 4096
```

输出：

- `demo/output/master.wav`：24-bit、48 kHz、双声道 PCM 母带
- `demo/output/master_report.json`：完整母带报告
- `demo/output/progress.json`：最近一次原子提交的块进度与已确定结果
- `demo/output/recovery_report.json`：仅在可处理中断时产生

续算时可直接复用快照中已提交的块长；若显式传入 `--block-samples`，则必须与快照一致：

```bash
.venv/bin/python -m bangzi_align render --manifest demo/manifest.json \
  --out-dir demo/output --block-samples 4096
```

`Ctrl+C` 会完成当前块、`fsync` 母带数据、原子替换快照、写出恢复报告，然后以退出码 `0` 结束。`SIGKILL` 或断电不会产生退出报告；下次启动从最后提交的块继续。若清单摘要、WAV 摘要、块长、段内偏移、帮腔检测结果或已提交前缀哈希与当前输入不一致，旧进度会被整体丢弃，程序输出 `E_RESUME_MISMATCH` 并以退出码 `3` 结束。

## 清单格式

生成的清单使用 `bangzi-session/v1`。核心字段如下：

```json
{
  "schema": "bangzi-session/v1",
  "seed": 237,
  "audio": {"sample_rate": 48000, "bit_depth": 24, "channels": 1},
  "channels": [
    {
      "id": "bangzi",
      "voice": "梆子",
      "path": "audio/bangzi.wav",
      "start_offset_samples": 311,
      "pan": -0.18
    }
  ],
  "segments": [
    {
      "id": "seg01",
      "ban_style": "慢板",
      "start_sample": 24000,
      "end_sample": 163200,
      "bangzi_reference_samples": [32640, 56640],
      "bangqiang_entry_sample": 136800,
      "separation_window_samples": [136800, 159840],
      "laosheng_drag_start_sample": 111840,
      "laosheng_drag_end_sample": 163680,
      "channel_gains_db": {
        "bangzi": -10.0,
        "banhu": -13.0,
        "yueqin": -19.0,
        "laosheng": -8.0,
        "bangqiang": -11.0
      }
    }
  ],
  "crossfade_samples": 3840,
  "master": {
    "target_lufs": -16.0,
    "true_peak_max_dbtp": -1.0,
    "block_samples": 4096
  }
}
```

位置字段也可用秒表示：`start_time`/`end_time`、`bangzi_reference_times`、`bangqiang_entry_time`、`separation_window_times`。若同时存在，优先使用采样点字段。清单必须恰好声明梆子、板胡、月琴、老生、帮腔五个唯一声部。

合成真值可由清单复算：

- 锚点击点的目标母带时刻就是 `bangzi_reference_samples`
- 帮腔目标进入时刻就是 `bangqiang_entry_sample`
- 老生拖腔目标区间由 `laosheng_drag_start_sample`/`laosheng_drag_end_sample` 声明
- 各通道录制起点由 `start_offset_samples` 声明；逐段运输漂移由 `seed` 和段序号确定性导出，并显式写入 `synthetic_transport_drift_samples`

板胡与月琴合成器包含约 4–5 Hz 揉弦和 45 ms 对数音高滑音；老生使用长包络拖腔，帮腔使用五声部失谐谐波合唱纹理。所有随机相位只由 `seed`、段序号和事件序号决定。

## 处理口径

**逐段对齐。** 对每段在梆子通道做包络起音检测，以该段全部参考击点为锚，取 `median(reference - detected)` 的四舍五入值作为该段整数校正 `s_b`。通道 `c` 的实际采样映射为：

```text
aligned_c[t] = source_c[t + shift_c]
shift_c = s_b + offset_b - offset_c
```

每段只使用本段 `shift_c`，段外内容不会继承相邻段的偏移。段间使用总长度为 `crossfade_samples` 的等功率 `sin/cos` 交叉淡化。

**帮腔分离。** 在帮腔通道的清单进入点附近检测起音，再按本段已确定的整数校正映射到母带时间；进入误差必须不超过 2 采样。分离窗内老生增益平滑降至 `0.56`，帮腔增益平滑升至 `1.18`；窗外两路增益严格为 `1.0`。报告记录分离前后老生/帮腔能量、帮腔能量占比和窗内修改采样数。

**响度与真峰值。** 积分响度按 ITU-R BS.1770-4 的 K 加权、400 ms 块、75% 重叠、绝对 `-70 LUFS` 与相对 `-10 LU` 门限计算。真峰值按 4 倍过采样测量。若峰值超过上限，使用零净延迟前瞻限幅器，母带采样数不变，报告触发采样数、门限、最小增益、限幅前后峰值和峰值降幅。最终响度与目标偏差必须不超过 `0.5 LU`。

**快照事务。** 每个块按以下顺序提交：

1. 将 PCM 块写入母带对应位置并 `fsync`
2. 原子替换 `progress.json`
3. 记录 `blocks_done`、清单/WAV 摘要、逐段偏移、帮腔进入、渲染增益、前缀 SHA-256

快照中的 `prefix_sha256` 覆盖 WAV 头和已提交数据。续算会重新验证清单与 WAV 摘要、全部已确定结果、块长和前缀哈希；任何不一致都进入 `E_RESUME_MISMATCH` 分支，而不是尝试修补或沿用旧块偏移。

## 诊断码

结构化错误写入 `stderr`：

```json
{"schema":"bangzi-diagnostic/v1","ok":false,"diagnostics":[{"code":"E_FILE_MISSING","channel":"bangzi","message":"WAV 文件缺失: audio/bangzi.wav"}]}
```

主要诊断码：

- `E_FILE_MISSING`、`E_WAV_READ`
- `E_SAMPLE_RATE`、`E_BIT_DEPTH`、`E_CHANNEL_COUNT`
- `E_DUPLICATE_CHANNEL`、`E_DUPLICATE_VOICE`、`E_VOICE_SET`、`E_INVALID_VOICE`
- `E_SEGMENT_OVERLAP`、`E_SEGMENT_RANGE`
- `E_HIT_ORDER`、`E_HIT_RANGE`、`E_ENTRY_RANGE`、`E_SEPARATION_RANGE`
- `E_FADE_TOO_LONG`、`E_HIT_DETECTION`、`E_ENTRY_DETECTION`
- `E_SILENT_MASTER`、`E_RENDER`
- `E_RESUME_MISMATCH`

退出码：成功或可处理中断为 `0`，清单/WAV 校验错误为 `2`，快照不匹配为 `3`，运行期错误为 `4`。

## 测试

```bash
.venv/bin/python -m unittest discover -s tests -v
```

测试覆盖：

- 五路样本与清单两次生成逐字节一致
- 采样率、位深、声道数、缺失文件、重复声部、段落重叠、击点乱序诊断
- 每段梆子锚点误差不超过 1 采样
- 帮腔进入误差不超过 2 采样，分离窗外增益不变
- BS.1770 响度目标与真峰值上限
- 限幅触发分支、长度不变与峰值下降
- 不同块长生成完全相同的母带字节
- `SIGKILL` 后续算母带与一次性渲染逐字节一致
- `SIGINT` 提交当前块、恢复报告、退出码 0，并成功续算
- 清单摘要或块长不一致时 `E_RESUME_MISMATCH` 且旧快照/旧母带被丢弃
- 报告字段顺序稳定，且不含时间戳和绝对路径

实际验证摘要见 `artifacts/verification_summary.json`。
