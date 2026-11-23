# 实际验证记录

生成日期：2026-11-24  
工作目录：`/home/liang/feedback/uid42a8691e-default-176`

## 环境

```text
$ go version
go version go1.22.12 linux/amd64

$ go list -m all
loudnessworkbench
```

`go.mod` 仅声明 `module loudnessworkbench` 和 `go 1.22`，没有第三方依赖。

Windows PowerShell 可执行文件存在于宿主环境，但 Windows `PATH` 中没有 Go；在 WSL 下通过 Windows PowerShell 直接执行 `go version` 返回 `CommandNotFoundException`。本环境因此无法原生复现 PowerShell 内的 Go 检查。Linux/WSL Go 1.22.12 已用于全部实际验证。

## 单元与集成测试

```text
$ go test ./...
?   	loudnessworkbench/apperr	[no test files]
ok  	loudnessworkbench/audio
ok  	loudnessworkbench/checkpoint
ok  	loudnessworkbench/dsp
ok  	loudnessworkbench/engine
ok  	loudnessworkbench/project
ok  	loudnessworkbench/samplegen	[no test files]
ok  	loudnessworkbench/workbench
?   	loudnessworkbench/cmd/loudness-wb	[no test files]

$ go test -race ./...
ok  	loudnessworkbench/audio
ok  	loudnessworkbench/checkpoint
ok  	loudnessworkbench/dsp
ok  	loudnessworkbench/engine
ok  	loudnessworkbench/project
ok  	loudnessworkbench/workbench

$ go vet ./...
(无输出)
```

覆盖内容：

- WAV float32 精确往返、内容哈希、损坏样本 `E_DECODE`。
- 48 kHz K 加权参考系数、1 kHz 立体声响度、静音门限、参数 `E_PARA`。
- 真峰值插值、压缩器形状与平滑性、限幅器 dBTP 上限和 `E_CLIP` 检查。
- 带限幅约束的节目归一目标收敛。
- 三版写出/加载往返、`DecisionParamsHash` 一致、首个 `E_LOSSY` 路径、`E_VER` 与 `E_SCHEMA`。
- 并行/实时/完整恢复/部分恢复逐样本一致。
- 检查点输出哈希失配导致整体作废和全量重算。
- 首个差异采样及所属段定位、状态机非法迁移保持原状态。
- Web `/api/state`、静态页面和三版互操作展示。

## 样本生成

```text
$ time go run ./cmd/loudness-wb generate -out demo -seed 20261124
...
real    0m1.554s
```

生成内容：

- 48 kHz，6 个节目段，共 840,000 帧。
- 3 个单声道段、3 个立体声段。
- 每段 float32 WAV、内容 SHA-256、积分响度、真峰值、门限统计、全局目标增益。
- v1/v2/v3 三份语义等价工程。

## 自动验收

```text
$ time go run ./cmd/loudness-wb verify -data demo -out artifacts/verification.json -parallel 4
verified 6 runs, 3 interoperability pairs, 5 stream comparisons, 2 checkpoint cases
mix sha256: da0e880475c4498dd1c1575ad2dcc6f459dff50a853c3b6100f6609fef4b9a40
max loudness error: 0.000000000 LU, max true-peak error: 0.000000000 dB
report: artifacts/verification.json

real    0m11.188s
```

### 跨版本互操作

```text
v1 <-> v2: equivalent
v1 <-> v3: equivalent
v2 <-> v3: equivalent
```

三版 `parameters_hash` 均为：

```text
9e81f5a9ebeab0599f41c58378c9314477e5cb29c2cdd9762e5210a3dcba4e45
```

报告保留每版的降级映射痕迹，例如 v3 的 `energy_ratio` 到内部 `relative_lu` 换算。

### 路径一致性

| 比较 | 逐样本一致 | 增益决策一致 |
| --- | --- | --- |
| 全量 v1 / 实时 v1 | 是 | 是 |
| 全量 v1 / 完整恢复 v1 | 是 | 是 |
| 全量 v1 / 部分恢复 v1 | 是 | 是 |
| 全量 v1 / 全量 v2 | 是 | 是 |
| 全量 v1 / 全量 v3 | 是 | 是 |

全部输出哈希：

```text
da0e880475c4498dd1c1575ad2dcc6f459dff50a853c3b6100f6609fef4b9a40
```

完整恢复复用 6/6 段；部分恢复复用 3/6 段，其余按同一全局增益决策计算。

### 检查点失配

| 场景 | 整体作废 | 使用旧检查点 | 原因 | 重算结果 |
| --- | --- | --- | --- | --- |
| 算法版本失配 | 是 | 否 | project/parameter/algorithm version mismatch | 与基线一致 |
| 输出内容哈希失配 | 是 | 否 | output hash mismatch at segment 0 | 与基线一致 |

两种场景均回到全量提交状态，mix 哈希与基线完全相同。

### 真值与目标

逐段真值核验：

```text
max loudness error: 0.000000000 LU
max true-peak error: 0.000000000 dB
gain error:          0.000000000 dB
gate frame stats:    全部一致
```

容差扫描按 `0.2 -> 0.1 -> 0.05 -> 0.01 -> 0.001` 收紧，接受段数始终为 6，未出现随容差收紧而增加的情况。

最终处理输出：

```text
目标节目响度:       -23.0000 LUFS
最终节目响度:       -23.0022 LUFS
压缩后节目响度:     -24.2988 LUFS
最终全局增益:        1.69846 dB
归一迭代次数:        6
归一收敛:            true
瞬态段输出真峰值:    -1.0000086 dBTP
限幅上限:            -1.0000000 dBTP
```

导出保持原始帧数：153600、134400、144000、124800、163200、120000，总计 840000 帧；单声道/立体声通道数逐段保持。

## 生成产物

```text
demo/
  manifest.json
  audio/seg-*.wav
  projects/program.v1.json
  projects/program.v2.json
  projects/program.v3.json

out/final/
  mix.wav
  report.json
  segments/seg-*.wav
  checkpoints/

artifacts/verification.json
```

`out/final/report.json` 包含逐段真值、输入/压缩后/输出统计、压缩增益曲线、最终增益、检查点恢复信息、容差扫描和会话状态。

## 约束确认

- 未使用 Docker 或其他容器。
- 未访问未授权目标。
- 未引入网络依赖或第三方 Go 模块。
- 全部样本由固定种子生成，可重复重建。
