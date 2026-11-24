# Spatial Acoustic Reconstruction Workbench

面向游戏与互动媒体音频素材的本地桌面工作台，实现多通道脉冲响应样本生成、分帧声学分析、声场参数重建、干声卷积渲染、逐帧提交/冻结、SQLite 断点恢复，以及实时、恢复、持久化参数全量复算三条路径的逐样本一致性比对。

项目不依赖外部音频文件或网络服务。样本由固定种子的确定性生成器在本地构造，真值直接来自生成器解析描述，不从分析结果反推。

## Quick Start

### Windows

1. 安装 Go 1.22+，并确认 `go version`、`go env` 正常。
2. 安装含 CGO 的 MinGW-w64 gcc，并确认 `gcc --version` 可用。
3. 设置可写目录并启用 CGO：

```powershell
go env -w CGO_ENABLED=1
go env -w GOPATH=D:\work\gopath
go env -w GOCACHE=D:\work\gocache
go mod download
```

4. 运行测试与命令行诊断：

```powershell
go test ./...
go run ./cmd/workbench generate -out samples/workbench_samples.json -seed 20261124
go run ./cmd/workbench verify -sample samples/workbench_samples.json -index 0 -resume-at 7
```

5. 启动 Fyne 桌面面板（Windows 默认构建包含 Fyne）：

```powershell
go run ./cmd/spatialworkbench
```

Windows 构建标签会自动启用桌面入口；Linux/CI 无图形环境下核心测试不需要 Fyne 的 C 依赖。Fyne v2.4.4 与纯 Go SQLite 驱动 `modernc.org/sqlite` 已锁定在 `go.mod`/`go.sum`。

### Linux

需要 X11/OpenGL 开发包（Ubuntu 示例）：

```bash
sudo apt install build-essential pkg-config libgl1-mesa-dev xorg-dev
go test ./...
go test -tags fyne ./...
go run -tags fyne ./cmd/spatialworkbench
```

## Source Map

| 路径 | 职责 |
|---|---|
| `internal/generator` | 固定种子生成多通道 IR、干声、解析真值 |
| `internal/loader` | 清单 JSON 解析、字段/点数/单调性/采样率校验 |
| `internal/acoustics` | 分帧估计、参数重建、误差上界、截断卷积渲染 |
| `internal/session` | 帧状态机、冻结边界、工作线程池与缓冲预算 |
| `internal/checkpoint` | SQLite 原子逐帧检查点、版本识别、整体作废 |
| `internal/verify` | 三路径运行、首个差异定位、逐样本比对 |
| `internal/audio` | 32-bit float 多声道 WAV 导出 |
| `internal/desktop` | Fyne 诊断面板与平台的试听/暂停控制 |
| `cmd/workbench` | 无显示环境的诊断、验证、渲染 CLI |
| `cmd/spatialworkbench` | Fyne 桌面入口 |

详细设计见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)，实际验证记录见 [docs/VALIDATION.md](docs/VALIDATION.md)。

## Core Semantics

### Sample and truth

生成器按 `seed` 构造 5.1、quad、stereo 等布局。每通道由以下部分叠加：

- 解析直达脉冲；
- 若干离散反射脉冲；
- 指数衰减尾音与确定性 LCG 噪声。

真值字段包括 `directArrival`、`directEnergy`、`reflections[]`、`drrDb`、`itdSamples`、`tailEnergy`，全部在构造阶段写入。

### Estimation and reconstruction

- 直达检峰只在全局采样点中按 `quantStep` 的整数倍网格搜索。
- 早期反射能量窗口从直达点之后开始，严格裁剪在分析帧 `[start,end)` 内，不跨帧补零。
- 通道间时差以布局首通道为参考；DRR 由直达能量与早期能量导出。
- 早期能量重建对已提交前缀使用固定平滑权重，形成跨帧状态演进。
- 渲染时按重建参数合成每帧 IR，与对应干声段卷积，只输出 `[0,len(dryChunk))`，输出总长度严格等于干声长度。

### Frame states

```text
pending -> estimated -> committed
   |           |  ^
   v           v  |
 failed <------+  |
   |              |
   +--> pending --+
```

合法迁移之外的操作返回 `E_STATE` 且保持原状态。已提交帧结果通过快照冻结；重新估计或提交已提交帧返回 `E_FROZEN`。

### Error bounds

每帧记录四项当前值与累计上界：

- `direct_peak_quantization`: `quantStep/2`；
- `early_energy_integration_truncation`: 被帧边界裁掉的窗口比例；
- `inter_channel_time_difference`: `quantStep`；
- `convolution_interpolation`: `2^-(interpOrder+1)`。

累计值只由前一帧状态与当前参数推导。当前值或累计值超出 `errorTolerance` 返回 `E_ERR`，并携带帧号与误差项名称。上界计算不读取真值或渲染结果。

### Checkpoints

SQLite 每提交一帧执行一次事务写入：

- 重建参数快照；
- 直达延迟、早期能量、ITD、DRR；
- 当前/累计误差上界；
- 内容哈希、参数哈希、算法版本、布局哈希；
- 渲染采样 BLOB。

数据库使用单连接、互斥锁与严格 `frame_index/seq` 连续检查，写入不会交错。加载时若内容哈希、参数版本、算法版本或布局哈希任一不一致，在同一事务内删除所有帧和元数据，再从第 0 帧全量重算。

### Three-path equality

`internal/verify` 实现：

1. **realtime**：逐帧估计、渲染、提交；
2. **resume**：提交前 N 帧，关闭数据库，重启后从 N 继续；
3. **persisted-recompute**：读取已持久化参数，在并行估计批次中重算，再按帧序重建/渲染/提交。

三路输出比较提交序列、逐通道延迟/能量/ITD/DRR、四项误差上界和全部渲染采样。首个差异返回 `E_DIFF`，包含帧号、通道、字段名及左右值。

## CLI

```bash
# 生成固定种子清单
go run ./cmd/workbench generate -out samples/workbench_samples.json -seed 20261124 -samples 3

# 查看样本解析真值
go run ./cmd/workbench inspect -sample samples/workbench_samples.json -index 0

# 逐帧推进并写入检查点
go run ./cmd/workbench diagnostics -sample samples/workbench_samples.json -index 0 \
  -db .workbench/state.sqlite -frames 4

# 同一数据库重启续跑
go run ./cmd/workbench diagnostics -sample samples/workbench_samples.json -index 0 \
  -db .workbench/state.sqlite -frames 8

# 三路径比对
go run ./cmd/workbench verify -sample samples/workbench_samples.json -index 0 -resume-at 7

# 并行估计批次 + 缓冲预算
go run ./cmd/workbench diagnostics -sample samples/workbench_samples.json -index 0 \
  -db .workbench/batch.sqlite -batch -workers 2 -buffer 4096

# 导出多声道 32-bit float WAV
go run ./cmd/workbench render -sample samples/workbench_samples.json -index 0 \
  -out artifacts/render.wav
```

异常注入：

```bash
go run ./cmd/workbench diagnostics ... -inject-order FL
go run ./cmd/workbench diagnostics ... -inject-clip FL
go run ./cmd/workbench diagnostics ... -inject-bad-frame 1
go run ./cmd/workbench diagnostics ... -inject-missing FL
```

## Error Codes

| 代码 | 场景 |
|---|---|
| `E_DECODE` | JSON 字段缺失、IO/解析失败、通道点数与 `irSamples` 不符 |
| `E_RATE` | 采样率超出 `[8000,192000]`，携带实际值、最小/最大值 |
| `E_PARA` | 帧长、帧移、窗长、量化步长、插值阶数等参数越界 |
| `E_LAYOUT` | 路由缺失、目标索引越界、输出声道冲突 |
| `E_ERR` | 数值误差上界超容差，携带帧号与误差项 |
| `E_STATE` | 非法状态迁移、乱序提交、失败帧未重试 |
| `E_FROZEN` | 改写已提交帧或其持久化代际 |
| `E_DIFF` | 三路径输出不一致，携带首个差异上下文 |
| `E_CHAN` | 通道缺失、重复、布局不一致 |
| `E_ORDER` | 直达/反射到达点不单调 |
| `E_CLIP` | 增益溢出 |
| `E_FRAME` | 坏帧、帧内处理异常或缓冲上限错误 |

## Verification

```bash
go test ./...
go test -race ./...
go test -tags fyne ./...
```

当前仓库保存了固定种子样本 `samples/workbench_samples.json` 与验证导出 `artifacts/render.wav`。实际命令输出见 [docs/VALIDATION.md](docs/VALIDATION.md)。
