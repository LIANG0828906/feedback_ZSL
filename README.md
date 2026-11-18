# 交响乐彩排离线母带渲染器

该工程从零实现了一个确定性的离线母带组件，覆盖六路单声道分轨生成、清单校验、起音对齐、掩蔽解耦、声部平衡、BS.1770 响度/真峰值合规、固定块渲染、原子快照和强杀恢复。

工程不依赖网络服务或外部音频素材。`init` 使用清单中的整数种子在本地确定性生成六路分轨、六路独立监听参考 WAV 和 JSON 清单。

## 安装

Windows 10/11，Python 3.10 及以上：

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
```

也可以使用 `uv` 或现有 Python 3.10+ 环境。运行时需要 NumPy、SciPy、soundfile（含 libsndfile）和 pytest。

## 快速运行

```powershell
# 生成 8 秒演示工程
.\.venv\Scripts\python -m orchestral_mastering init --output-dir .\demo

# 严格校验清单和 12 个 WAV（六路 STEM + 六路参考）
.\.venv\Scripts\python -m orchestral_mastering validate --manifest .\demo\manifest.json

# 分块渲染母带、质量报告和进度快照
.\.venv\Scripts\python -m orchestral_mastering render `
  --manifest .\demo\manifest.json `
  --output .\demo\master.wav `
  --report .\demo\report.json `
  --snapshot .\demo\checkpoint.json `
  --block-frames 16384
```

一条命令完成生成和渲染：

```powershell
.\.venv\Scripts\python -m orchestral_mastering demo --output-dir .\demo --force
```

入口也兼容 `python mastering.py ...` 和安装后的 `orchestral-master ...`。

## 清单契约

清单路径可相对清单文件所在目录。核心字段如下：

```json
{
  "schema_version": 1,
  "seed": 24701,
  "sample_rate": 48000,
  "bit_depth": 24,
  "channels": 1,
  "duration_seconds": 8.0,
  "block_frames": 16384,
  "master": {
    "target_loudness_lufs": -18.0,
    "true_peak_max_dbtp": -1.0
  },
  "unmasking": {
    "window_seconds": [[1.15, 2.8], [4.2, 5.55]],
    "residual_threshold_db": -24.0,
    "min_attenuation_db": 12.0
  },
  "masking": [
    {
      "source_group": "铜管",
      "target_group": "木管",
      "gain_db": -7.0,
      "band_hz": [180.0, 9000.0],
      "window_seconds": [[1.15, 2.8], [4.2, 5.55]]
    }
  ],
  "tracks": [
    {
      "name": "弦乐组",
      "group": "弦乐",
      "path": "audio/01_strings.wav",
      "reference_path": "audio/ref_01_strings.wav",
      "anchor": {"x": -0.7, "y": -1.1, "z": 0.2},
      "target_energy_ratio": 0.24,
      "onset_reference_seconds": 0.22,
      "source_onset_seconds": 0.217115,
      "channels": 1
    }
  ]
}
```

必须恰好包含 `弦乐`、`木管`、`铜管`、`打击乐`、`合唱`、`指挥提示` 六组；也接受对应英文组名。`target_energy_ratio` 之和必须为 1，起音参考时刻必须严格升序。掩蔽关系必须至少包含 `铜管 -> 木管` 和 `合唱 -> 弦乐`。

`source_onset_seconds` 是 `init` 写入的合成源瞬态绝对位置；`onset_reference_seconds` 是目标对齐位置。两者之差即为可复算的源时间抖动。独立监听参考 WAV 是不含注入掩蔽成分的干净目标声部，用于估计局部本底和注入成分。

## 处理流程

1. 清单先完整校验，再读取音频。实际采样率、位深（PCM 16/24/32）、声道数、WAV 格式、组集合、比例和起音顺序任一不符即终止渲染。
2. 每路独立检测首个起音瞬态，计算整数采样偏移。失败通道记录 `DETECTION_FAILED`，保持原位且不改变其他通道偏移。
3. 对两类掩蔽关系，在声明时间窗和频带内用 `目标分轨 - 独立参考` 估计注入成分。检测成功时仅在该时间窗内衰减之；默认残余阈值为 -24 dB，同时保证衰减不低于清单要求。
4. 按目标能量比例和舞台锚点常量功率声像计算各组增益，合成立体声。组能量和总能量均按双声道平方和计算。
5. 使用 ITU-R BS.1770-4 K 加权、400 ms 块和 75% 重叠、绝对/相对门限计算积分响度。真峰值采用 4 倍过采样测量。
6. 超限时使用带前瞻的联动限幅器，左右声道共享增益包络；限幅延迟补偿为 0 个采样，母带长度完全不变。
7. 最终浮点母带按固定块写入原子块文件，每块后原子覆盖完整快照；完成后再量化为清单声明的 PCM WAV。

## 快照与中断

- 每完成一个固定块：先原子写入块文件，再原子覆盖 `checkpoint.json`。快照包含清单/输入摘要、块大小、总帧数、已完成块数、提交次数、每块 SHA-256、瞬态偏移和解耦参数。
- 强制终止只可能留下“块文件已写、快照未提交”的多余块；恢复时不会被采用。
- 清单摘要、输入音频摘要、块大小、引擎版本、已确定偏移或解耦参数与当前输入不一致时，整体删除旧快照和块目录，输出 `E_RESUME_MISMATCH`，退出码 3。
- Ctrl+C 使用信号标志而不在块中间抛出异常。当前块完成后提交快照，写出 `status: "interrupted"` 的恢复报告，以退出码 0 结束；再次运行相同命令即可续算。
- 恢复只使用已提交快照中的参数与块，母带与一次性完整渲染逐字节一致。

## 错误与退出码

所有失败以 JSON 输出到 stderr，包含业务错误码、通道名、组名和细节：

- `E_INVALID_MANIFEST`：JSON/字段、组集合、比例、顺序、时间窗或掩蔽关系非法。
- `E_MISSING_FILE`：清单、分轨或参考文件缺失。
- `E_FORMAT_MISMATCH`：格式、采样率、位深、声道数或 WAV 类型不符。
- `E_RESUME_MISMATCH`：快照与当前输入不一致，旧进度已丢弃。
- `E_RENDER_INTERNAL`：未预期的内部渲染错误。

退出码：`0` 成功或可处理中断；`2` 清单/输入校验失败；`3` 恢复状态不匹配；`4` 内部渲染错误。

## 报告字段

`report.json` 是排序键、固定浮点精度的稳定 JSON，不含耗时字段。主要字段：

- `alignment.tracks`：每组检测到的瞬态采样、参考采样、整数平移、对齐误差、对齐后时长。
- `unmasking.channels`：检测结果、注入能量前后、衰减量、干净声部损失、修改区间和修改采样数。
- `balance.tracks`：目标比例、校正前比例、线性/dB 校正系数、声像和贡献比例。
- `master`：限幅前后响度、真峰值、触发样本数、左右峰值、各组最终贡献比例和最大比例偏差。
- `snapshot`：块大小、总块数、已完成块数和提交次数。
- `warnings`：包括 `DETECTION_FAILED` 等不阻断渲染的质量告警。

## 本地验收

```powershell
.\.venv\Scripts\python -m pytest -q
```

本次实际运行结果：`17 passed`。验收覆盖：

- 六路起音对齐误差均为 0 个采样，母带长度等于最长平移后分轨长度。
- 铜管→木管、合唱→弦乐注入均被检测；衰减 24.0 dB，干净声部损失约 0.0002 dB。
- 默认工程积分响度 -18.0 LUFS，真峰值 -6.17 dBTP，未触发限幅；目标 -9 LUFS 的压力用例触发限幅，最终 -9.02 LUFS、-1.06 dBTP，声部比例最大偏差 0.0048。
- 强杀测试在第 4 个已提交块后 `SIGKILL`，重启续算后母带 SHA-256 与一次性渲染相同：`0c189c4e22b6b58d6e49e572b60587b3d3328b72c05d4fb98190825a1535d7db`。
- 重复运行相同输入时母带和报告逐字节一致；Ctrl+C 返回码为 0，快照/报告状态均为 `interrupted`，续算后母带逐字节一致。

POSIX 测试直接使用 `SIGKILL` 和 `SIGINT`。Windows 上可对渲染进程执行 `taskkill /F /PID <pid>` 验证强制终止恢复；Ctrl+C 的提交流程相同。
