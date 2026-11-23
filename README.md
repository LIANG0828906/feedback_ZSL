# Broadcast Loudness Workbench

面向游戏与互动媒体音频素材的本地广播响度、动态范围和跨版本工程工作台。项目从空仓库建立，仅使用 Go 标准库，不依赖网络、外部服务或容器。

## 交付内容

- 固定种子确定性样本生成器，产出 48 kHz、6 个单/立体声节目段、WAV 音频、三版工程文件和逐段真值清单。
- v1/v2/v3 统一加载器，内部规范模型保留源版本，支持清单一致性校验、字段路径诊断和逐项等价比较。
- BS.1770 风格两段 K 加权、可配置分帧、绝对/相对门限积分响度、4x 窗化 sinc 真峰值。
- 平滑启动/释放压缩器、前瞻限幅器、带限幅约束的全节目迭代归一。
- 有界并行的段处理、全段统计提交屏障、全局增益决策、逐段状态机和原子串行检查点。
- 全量并行、实时串行、断点恢复三条路径的逐样本比对与首个差异定位。
- 内嵌本地 Web 工作台：时间轴、状态、响度/真峰值曲线、压缩增益曲线、差异热力图和参数编辑。

## 环境

实际验证环境：

```text
Go: go1.22.12 linux/amd64
Module: loudnessworkbench
依赖: 仅标准库
```

检查命令：

```bash
go version
go list -m all
```

WSL 中检测到了 Windows PowerShell，但 Windows 侧 `PATH` 没有 Go，因此本环境无法完成“在 Windows PowerShell 内直接执行 `go version`”的原生验证；Linux/WSL 工具链已实测。若 Windows 已安装 Go，PowerShell 中应直接执行 `go version` 并得到 `go1.22` 或更高版本。

## 快速开始

```bash
# 1. 运行测试
go test ./...

# 2. 生成固定种子样本、三版工程、音频和真值清单
go run ./cmd/loudness-wb generate -out demo -seed 20261124

# 3. 查看某个版本规范化后的内部模型
go run ./cmd/loudness-wb inspect \
  -project demo/projects/program.v3.json \
  -manifest demo/manifest.json

# 4. 运行完整处理链并导出 mix.wav、逐段 WAV 和 report.json
go run ./cmd/loudness-wb run \
  -project demo/projects/program.v1.json \
  -manifest demo/manifest.json \
  -out out/final -mode full -parallel 4

# 5. 自动验证互操作、三路径一致性、检查点失效与真值误差
go run ./cmd/loudness-wb verify \
  -data demo -out artifacts/verification.json -parallel 4

# 6. 启动本地工作台
go run ./cmd/loudness-wb serve -data demo -addr 127.0.0.1:8080
```

浏览器访问 `http://127.0.0.1:8080`。工作台只监听本地地址，无外部资源。

## 目录结构

| 路径 | 作用 |
| --- | --- |
| `audio/` | PCM 信号、交错浮点表示、WAV 编解码、内容哈希 |
| `dsp/` | K 加权、门限积分、真峰值、压缩、限幅、迭代归一 |
| `project/` | 内部规范模型、三版加载/写出、清单校验、等价比较 |
| `checkpoint/` | 带整体哈希的原子快照存储，进程内串行写入 |
| `engine/` | 状态机、有界并行调度、全局归一、恢复、路径比对、真值核验 |
| `samplegen/` | 固定种子确定性样本与多版本工程生成器 |
| `workbench/` | 本地 HTTP API 与内嵌 HTML/CSS/JS 诊断界面 |
| `cmd/loudness-wb/` | CLI：`generate/inspect/run/verify/compare/serve` |
| `demo/` | 已生成的本地确定性验收数据 |
| `out/final/` | 已生成的处理输出、逐段 WAV 与报告 |
| `artifacts/verification.json` | 实际自动验收报告 |

## 三版工程格式

三个版本都映射到同一内部模型，但表达方式不同：

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 版本标识 | `version: 1` | `schemaVersion: "2.0"` | `format: "loudness-workbench/3"` |
| 字段风格 | snake_case | camelCase | 分组对象 + camelCase |
| 门限 | `gate.absolute_lufs`, `gate.relative_lu` | `gating.absoluteLufs`, `gating.relativeDropLu` | `analysis.gates[]`，相对门限用 `energy_ratio` |
| 阶段清单 | `compression_chain: ["compress","limit"]` | `stageOrder: ["compressor","limiter"]` | 有序 `processingStages[]` 对象 |
| 音频引用 | 段内 `wav-slice` 路径、偏移、帧数、哈希 | 顶层 `assets`，段引用 `assetRef` | `media.baseDir` + `clips` 映射，段引用 `clip` |

相对门限等价换算：

```text
relative_lu = -10 * log10(energy_ratio)
```

v3 的浮点换算会规范化到 1e-12，确保三版 `DecisionParamsHash` 相同。

## 处理语义

### 测量

- 支持采样率：8000、11025、16000、22050、24000、32000、44100、48000、88200、96000、192000 Hz。
- 默认帧长 400 ms、帧移 100 ms；帧长/帧移可配置，但必须落在有效采样点范围且帧移不大于帧长。
- 每个通道先经过 BS.1770 两段 K 加权，再计算重叠帧均方能量；立体声两个通道权重为 1。
- 绝对门限默认 `-70 LUFS`，相对门限默认 `-10 LU`。积分响度使用 `-0.691 + 10log10(mean power)`。
- 真峰值使用 4x、16 tap 窗化 sinc 插值，并在最终导出前量化为 float32。
- 静音/无门限内帧表示为 `-Inf`，JSON 输出为 `null`；整节目无有效帧时拒绝归一并返回 `E_PARA`。

### 压缩与限幅

- 压缩器：阈值、压缩比、启动时间、释放时间；立体声联动检测，增益按一阶时间常数平滑，不阶跃。
- 限幅器：dBTP 上限、前瞻时间；先按前瞻窗口求未来峰值包络，再平滑，并对量化后的输出执行最终真峰值约束。
- 输出帧数和通道数始终与输入一致。
- 后置检查发现真峰值仍高于上限时返回 `E_CLIP`，包含段号、段 ID、实测值与上限。

### 全局归一

压缩后的所有段先完成统计并形成节目级门限响度。归一不是简单的单次 `target - loudness`，而是在限幅器参与下迭代求解全局增益：

1. 用压缩后节目响度给出初始增益。
2. 对全部段施加同一增益并限幅。
3. 重新门限测量拼接节目响度。
4. 以阻尼更新全局增益，直到误差不超过 0.005 LU 或达到 8 次迭代。

因此增益决策依赖全段统计，并且最终节目响度仍能命中目标。若限幅上限约束导致目标不可达，求解器返回最佳可达结果并记录 `normalization_converged=false`。

## 错误契约

结构化错误同时由 CLI JSON 和 HTTP API 返回：

| 错误码 | 场景 |
| --- | --- |
| `E_VER` | 未知工程版本、版本标识缺失、工程必填字段缺失 |
| `E_SCHEMA` | 工程内容与清单冲突、音频时长/通道/哈希不符、首个字段路径冲突 |
| `E_PARA` | 采样率不支持、分帧参数越界、动态参数越界、缓冲上限不足 |
| `E_CLIP` | 限幅后实测真峰值仍超过上限 |
| `E_LOSSY` | 两版本内部模型不等价，附首个路径、两侧值、降级映射痕迹 |
| `E_STATE` | 非法段状态迁移或提交后修改参数 |
| `E_DECODE` | WAV 损坏、非有限采样、缺失音频 |
| `E_DIVERGE` | 三条路径首个不一致采样，含采样/帧/通道/段编号与两侧值 |
| `E_CHECKPOINT` | 检查点无法读取或损坏 |

## 段状态机

```text
pending -> processing
processing -> committed | failed | pending
failed -> processing | pending
committed -> (terminal)
```

非法迁移返回 `E_STATE` 且保留原状态。已提交段不可重新处理；若存在失败段，同一会话可重试失败段并复用已提交段与检查点中记录的增益决策。

## 并行与检查点

- `max_parallel` 限制工作线程数；`per_worker_buffer_bytes` 限制单段工作缓冲。
- 工作队列使用阻塞发送，缓冲耗尽时等待，不会静默丢段。
- 结果按节目段索引重排后汇总，完成顺序不影响样本流或增益序列。
- 每提交一段，整份快照在互斥锁下通过临时文件、`fsync` 和原子重命名替换，写入不会交错。
- 快照保存段响度统计、压缩统计、最终增益、输入/输出内容哈希、参数哈希、源版本和算法版本。
- 启动恢复时校验快照整体哈希、项目、源版本、参数哈希、算法版本、输入哈希、输出形状与输出哈希。任一不匹配会整体删除快照并全量重算。

## Web 工作台

内嵌页面提供：

- 节目段时间轴与 `pending/processing/committed/failed` 状态。
- 逐段输入响度、输入真峰值、输出响度与输出真峰值。
- 压缩增益曲线。
- 全量/实时/恢复三路径差异热力图和首个差异位置。
- 目标响度、分帧、门限、压缩器、限幅器参数编辑。
- 开始、取消、重置和三路径比对操作。

已提交段在 UI/API 中锁定，参数编辑只发生在提交屏障之前；需要修改已提交工程时执行“重置工程”。

HTTP API：

```text
GET  /api/state
POST /api/run
POST /api/config
POST /api/version
POST /api/reset
POST /api/verify
GET  /api/interop
```

## 实际验收

`artifacts/verification.json` 和 `docs/VERIFICATION.md` 记录了实际结果：

- 6 次运行：v1 全量、v1 实时、v1 完整恢复、v1 部分恢复、v2 全量、v3 全量。
- 6 个输出 mix SHA-256 完全一致。
- 3 组版本互操作全部等价。
- 5 组逐样本/增益决策比较全部一致。
- 2 个检查点失配场景均整体作废并全量重算，结果与基线一致。
- 逐段真值最大响度误差 `0 LU`，最大真峰值误差 `0 dB`。
- 最终节目响度 `-23.0022 LUFS`，目标 `-23 LUFS`；瞬态段输出真峰值 `-1.0000086 dBTP`，上限 `-1 dBTP`。

## 限制与说明

- “实时”路径是段级、有界内存的串行两阶段处理，保证确定性输出和缓冲阻塞语义，不声明硬实时调度截止时间。
- 跨版本等价要求所有字段归一化后等价；仅字段拼写、门限表达、阶段清单和引用方式不同会被接受，语义差异返回 `E_LOSSY`。
- 参数调整与已提交结果冲突时选择一致性优先：已提交段锁定，必须显式重置后全量重算。
