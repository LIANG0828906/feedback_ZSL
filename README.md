# 虚拟演播厅声学校准工作台

这是一个完全本地运行的 Python/PySide6 工作台，用于载入 48 kHz、16-bit PCM、单声道或 5.1 声道 WAV 脉冲响应，完成：

- 直达声检测、以参考声道为基准的整样本对齐与增益归一。
- 本地确定性 FFT 的倍频程/三分之一倍频程频带能量与误差上界估计。
- 频带平滑、目标曲线补偿、线性相位 FIR 设计与群延迟输出。
- 量化、加窗、分段方差、平滑耦合、滤波器截断的逐节点误差传播与阈值归因。
- 追加式校准版本、任意版本比较、回退追加、内容哈希与父哈希链。
- 受影响节点增量复算、整体不可变发布、`E_VERSION_STALE` 并发保护。
- 按声道原子检查点、强制终止后的磁盘恢复、断链区间定位与未完成区间续算。
- PySide6 图形工作台：能量曲线、误差上界包络、滤波器响应、声道表、告警表、版本与回退操作。

题目中的样本全部由本地确定性随机源生成，不需要外部音频或网络资源。

## 环境

要求 Python 3.11 或更高版本：

```bash
python3 -m pip install numpy pyside6
```

本仓库实际验证环境：

- Python 3.14.4
- NumPy 2.5.5
- PySide6 6.12.0
- Linux / Ubuntu，未使用 Docker 或其他容器

## 快速开始

运行完整本体验收场景（样本生成、多个版本、回退、取消、磁盘恢复、全量重算对比）：

```bash
python -m acoustic_calibration.cli verify --workspace artifacts/verification
```

生成一个确定性 5.1 样本并发布一版校准：

```bash
python -m acoustic_calibration.cli demo --workspace artifacts/demo
```

启动图形工作台：

```bash
python -m acoustic_calibration.cli gui
```

在无桌面的 CI 环境可使用 Qt 离屏后端：

```bash
QT_QPA_PLATFORM=offscreen python -m unittest discover -s tests -v
```

导出指定版本的每声道 FIR 系数、群延迟和逐频带误差摘要：

```bash
python -m acoustic_calibration.cli export \
  --workspace artifacts/demo/measurement \
  --version 1 \
  --output artifacts/demo/calibration_v1.json
```

`--workspace` 指向包含 `state/` 和 `snapshots/` 的校准工作区根目录。

## 目录

```text
acoustic_calibration/
  alignment.py      直达声、对齐、增益与差异摘要
  spectral.py       分帧加窗、FFT、频带聚合与误差上界
  filters.py        平滑权重、目标曲线补偿与 FIR
  propagation.py    逐节点误差传播与归因
  models.py         不可变数据契约
  engine.py         增量计划、后台任务、取消、原子发布、恢复编排
  versioning.py     追加式版本存储、哈希链、差异与回退
  recovery.py       按声道原子快照、哈希链校验与断链定位
  storage.py        临时文件 + fsync + 原子重命名
  wavio.py          16-bit PCM / WAVE_FORMAT_EXTENSIBLE 读写
  samples.py        确定性多声道脉冲响应样本
  gui.py            PySide6 工作台
  cli.py / __main__.py
tests/              24 个自动化测试
docs/TECHNICAL.md   算法、协议与并发边界说明
docs/VALIDATION.md  实际验证命令和结果
```

## 关键语义

版本目录：

```text
workspace/
  state/
    versions/v0000000001.json
    latest.json
    trace.jsonl
  snapshots/run-*/manifest.json
  snapshots/run-*/segment-000-ch00.json
```

- 已发布版本文件只写一次并设为只读；加载时重新计算内容哈希。
- 回退不改写旧版本，而是复制目标版本产物并以新父哈希追加一个带 `rollback_of` 的新版本。
- 查询只返回完整版本文件；发布顺序是“不可变版本文件原子重命名”后“更新 latest 指针”，查询可安全看到旧版或新版。
- 快照先写声道段临时文件并 `fsync`，再原子重命名，最后原子更新 manifest；恢复只信任 manifest 列出的段。
- 恢复校验 base 版本、快照参数哈希、每段内容哈希和段间前向哈希；任何断链均返回 `E_RECOVERY_INVALID` 并给出 `first_broken_interval`。
- 已提交的声道区间在恢复时直接复用，不再写入；缺失声道从下一个检查点继续，最终只发布一次完整新版本。

业务码：

| 业务码 | 含义 |
| --- | --- |
| `E_FILTER_INVALID` | 滤波器阶数/目标曲线非法；保留上一有效版本与滤波器 |
| `E_VERSION_STALE` | 父哈希、内容哈希或测量源哈希不匹配 |
| `E_BUSY` | 同一测量已有复算/恢复任务 |
| `E_RECOVERY_INVALID` | 快照缺字段、段缺失或哈希链断链 |
| `E_CANCELLED` | 在声道/频带检查点协作取消 |
| `E_WAV_INVALID` | 非 48 kHz / 16-bit PCM / 非单声道或 5.1 |
| `E_PARAMS_INVALID` | 非滤波器类参数非法 |

## 验证

```bash
QT_QPA_PLATFORM=offscreen python -m unittest discover -s tests -v
python -m acoustic_calibration.cli verify --workspace artifacts/verification
```

本次实际结果、覆盖范围和关键输出见 [docs/VALIDATION.md](docs/VALIDATION.md)。算法和恢复协议细节见 [docs/TECHNICAL.md](docs/TECHNICAL.md)。
