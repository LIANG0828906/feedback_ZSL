# Haptic Studio

面向游戏手柄与触觉背心联调的确定性触觉波形预览应用。应用读取本地 JSON
波形描述，把多段波形采样、叠加并量化为左右马达驱动信号，同时提供设备配置
版本、增量重算、原子快照、强杀恢复和 PySide6 桌面预览。

## 快速开始

目标环境为 Windows 10+、Python 3.11。没有真实触觉设备也可运行。

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\activate
pip install PySide6 numpy pytest
python -m haptic_studio generate --out samples
python -m haptic_studio gui --samples samples --runtime runtime
```

也可以直接运行 `run_gui.bat`。应用会载入 `samples/` 中的确定性样本；也可用
工具栏的 `Load wave JSON` 和 `Load device JSON` 载入本地文件。

可重复演示与验证：

```powershell
python -m haptic_studio demo --out artifacts\demo --sample dense_9
python -m haptic_studio crash-recover --out artifacts\crash
python -m pytest -q
```

`--out` 指定的目录必须可写。运行前请关闭会锁定输出目录的编辑器、同步盘或
杀毒扫描进程，避免 Windows 上原子重命名被文件锁干扰。

## 交付内容

- `haptic_studio/models.py`：波形与设备配置模型、字段级校验。
- `haptic_studio/sampler.py`：包络、载波、双马达映射、叠加、限幅和内容哈希。
- `haptic_studio/engine.py`：影响因子差异分析与增量重算计划。
- `haptic_studio/versions.py`：配置版本账本、不可变发布器和陈旧句柄检测。
- `haptic_studio/persistence.py`：版本哈希链、原子快照、检查点、轨迹日志。
- `haptic_studio/session.py`：重算、发布、回退、恢复和单任务门控。
- `haptic_studio/gui.py`：PySide6 桌面应用。
- `haptic_studio/samples.py`：确定性本地样本与基线生成器。
- `tests/`：字段校验、增量等价、限幅、版本隔离、强杀恢复、GUI 和可复现性测试。

## JSON 格式

波形文件：

```json
{
  "schema_version": 1,
  "sequence_id": "demo",
  "total_duration_ms": 800,
  "segments": [
    {
      "start_ms": 20,
      "duration_ms": 120,
      "frequency_hz": 45.5,
      "amplitude": 0.8,
      "envelope": "rectangular"
    }
  ]
}
```

`envelope` 可取 `rectangular`、`linear_decay`、`sine`，也接受 `rect`、
`linear`、`sin` 等别名。频率必须为正且不超过
`min(10000 Hz, sample_rate / 2)`；幅度必须在 `[0, 1]`。

设备配置文件可包含多个版本：

```json
{
  "sample_id": "demo",
  "versions": [
    {
      "version": 1,
      "label": "balanced_hard",
      "config": {
        "sample_rate": 1000,
        "bit_depth": 8,
        "allocation_ratio": {"left": 0.5, "right": 0.5},
        "polarity": {"left": 1, "right": -1},
        "limiter": "hard"
      }
    }
  ]
}
```

也接受 `allocation_ratio: [left, right]`、`polarity: [left, right]`、
`limiter: "soft"` 等等价写法。分配比必须非负且总和为 1；极性只能是
`+1` 或 `-1`。采样率范围 `64..384000`，量化位深范围 `2..24`。

## 采样与限幅规则

所有时间到样本的转换使用非负数的确定性 half-up 取整：

```text
sample = floor(milliseconds * sample_rate / 1000 + 0.5)
```

段区间为 `[start_sample, end_sample)`。归一化包络时间
`u = (t - start) / duration`，包络分别为：

- 矩形：`1`
- 线性衰减：`1 - u`
- 正弦：`sin(pi * u)`

载波为 `sin(2*pi*frequency_hz*t)`。每段每通道的整数贡献为：

```text
round(amplitude * envelope * carrier * max_code * allocation * polarity)
```

其中 `max_code = 2^(bit_depth-1) - 1`。不同采样率下包络使用真实毫秒时间，
段端点沿用同一 half-up 规则，因此不会因采样率改变而改变设定时长之外的段。

叠加前按 `(start_sample, segment_index)` 稳定排序。段贡献以 `int32`
存储，多段使用 `int64` 逐样本求和。硬限幅直接截到
`[-2^(bit_depth-1), max_code]`；软限幅在 `0.8 * max_code` 膝点后平滑压缩，
渐近于 `max_code`。限幅统计按“任一通道溢出的帧”计数，同时记录左右通道
各自计数和首个溢出的全局样本位置。发布结果还保存逐帧限幅掩码供界面标记。

每段输出：

- `start_sample`、`end_sample`
- 段贡献的左右数组哈希
- 段参数与设备影响因子的哈希

所有哈希都对规范化 JSON 或确定的 little-endian 整数数组计算 SHA-256。

## 增量重算

段参数和设备配置共同构成影响因子。差异分析规则如下：

| 变化 | 处理 |
| --- | --- |
| 某段起点、时长、频率、幅度或包络 | 重算该段，并重混其旧/新区间及前后各一个过渡采样点 |
| 左右分配比或极性 | 所有段贡献均受影响，重混各段区间并复用无段覆盖的样本 |
| 采样率 | 保守全量重算，轨迹原因 `sample_rate_changed` |
| 量化位深 | 保守全量重算，轨迹原因 `bit_depth_changed` |
| 限幅策略 | 保守全量重算，轨迹原因 `limiter_changed` |
| 总时长、schema 或无法证明安全的内部状态 | 保守全量重算并写明原因 |
| 无有效变化 | 复用全部段和全部样本 |

增量结果会把旧发布中的段贡献与最终驱动数组作为缓存，只在独立工作副本上
重混受影响区间。提交前会与全量重算结果做测试比对，左右字节与内容哈希应
完全一致。

## 版本与预览隔离

设备配置版本从 1 单调递增。回退会复制旧配置并追加一个新版本，历史记录不会
删除。每次发布同时记录：

- 发布版本号、配置版本号
- 配置内容哈希、波形内容哈希
- 受影响段、重算/复用段
- 重算/复用样本区间和数量
- 限幅样本数、首个限幅位置
- 驱动信号内容哈希和版本哈希链父哈希

预览句柄绑定发布代际。发布新版本后，旧句柄查询固定返回
`E_STALE_HANDLE`；需要查看历史时使用显式的
`preview_version(version)`。发布使用完整不可变对象做引用替换，查询在同
一把发布锁内取得某一整份版本，不会混合新旧数组。

## 原子快照与强杀恢复

会话目录包含：

- `session.snapshot.json`：配置版本、已发布版本、每段原始贡献、采样区间
  哈希、驱动信号、版本哈希链和未完成重算检查点。
- `trace.jsonl`：规范化 JSON Lines 轨迹日志。

快照先写同目录临时文件，`flush + fsync` 后使用 `os.replace` 原子替换，并在
支持的平台同步目录。加载时逐版本校验配置哈希、信号哈希、父哈希和链尖。
任何断链或缺失字段返回 `E_RECOVERY_INVALID`，并定位第一个损坏版本。

重算开始前先落盘 pending 记录；每完成一个段采样区间后原子更新已完成段。
若进程被 `SIGKILL`，重启时恢复最近完整发布，再跳过 pending 中已完成且影响
因子一致的段，只重算剩余段，最后完成同一目标发布版本。

## 桌面界面

- 段表支持新增、删除和编辑起点、时长、频率、幅度、包络。
- 设备面板支持采样率、位深、分配比、左右极性和限幅策略。
- 时间轴显示段包络；驱动图显示左右马达曲线与逐样本限幅三角标记。
- 版本时间轴和版本列表可查看历史发布；可多选版本查看差异，并把任一历史
  配置回退为新版本。
- `Export CSV` 输出
  `sample_index,timestamp_ms,left,right,limited`。
- 重算在 `QThread` 中执行；重复任务返回 `E_BUSY`，取消在采样检查点生效，
  且保留最后一次完整发布。

## 实际验证结果

在仓库当前环境（Python 3.11.18、PySide6 6.12.0、numpy 2.4.6）执行：

```text
$ .venv/bin/pytest -q
52 passed
```

覆盖内容包括：

- 字段缺失、负时长、频率越界、Nyquist 越界和段尾越界均定位到段号与字段。
- 137/500/1000/2000/4800 Hz 采样率下三种包络区间一致。
- 硬/软限幅范围、计数、首位置和逐帧掩码。
- 段编辑、分配比、位深、采样率和限幅策略的增量/全量判定。
- 增量驱动字节与全量重算逐字节一致。
- 陈旧句柄、历史查询、配置回退和历史哈希重算。
- 进程强杀后恢复已完成段并只补算剩余段。
- 首次发布前强杀留下的“仅配置快照”可带外部波形重新进入首次发布。
- 快照缺失字段和父哈希断链的版本定位。
- 两次完整 demo 的 `trace.jsonl`、5 份驱动 CSV 和汇总逐字节一致。
- GUI 在 `QT_QPA_PLATFORM=offscreen` 下实例化、编辑并完成异步重算。

`crash-recover` 的实际结果：子进程退出码 `-9`，恢复报告显示已恢复段
`[0, 1]`、补算段 `[2, 3, 4, 5, 6, 7, 8]`，最终发布和历史哈希校验一致。

仓库中的 `samples/` 与 `artifacts/verification/` 保存了本次生成的样本、
基线摘要、轨迹和验证记录。相同种子和操作序列可重新生成相同字节。

