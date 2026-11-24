# 音效素材编解码质量评估管线 — 初版交付说明

## 1. 范围与结论

本仓库在空工作区初始化为 Go module `audiocheck`，固定使用纯 Go SQLite 驱动
`modernc.org/sqlite v1.29.10`。样本生成、DCT 分帧编码、指标计算、调度、检查点、
独立验证器和诊断界面均由本仓库代码完成，不读取外部音频文件或真实账号。

已实现并验证：

- 确定性原始样本、DCT 量化重建、解析真值与真值指纹。
- `E_SCHEMA`、`E_PARA`、`E_DECODE`、`E_STAGE`、`E_BAND`、`E_BOUND`、`E_STATE` 错误链路。
- PSNR/SNR、SSIM、噪声基底残留标准差、定点输出和截断记录。
- 固定块、有界 worker、有界待处理队列、每线程采样缓冲预算和顺序提交。
- 块边界重叠边带、确定性窗口选择/统计合并，分块结果与单次整段评估逐固定字段一致。
- 系数量化误差、采样域误差、边界跃变、邻接扩散路径和仅依赖观测量化步长的上界。
- 四阶段四状态状态机、SQLite 原子串行提交、内容/版本不匹配整体作废。
- full、realtime、resume 三路径一致；独立 `cmd/audio-verify` 不导入主评估/调度包。
- 本地 HTTP 仪表盘：指标曲线、噪声/上界、差异热力图、块状态矩阵、排队明细。

## 2. 代码结构

- `internal/model`：清单、错误码、坐标、身份指纹、报告和预算结构。
- `internal/generator`：确定性合成器、脉冲标记、DCT 编码器、量化误差和解析真值。
- `internal/eval`：整型累加指标、窗口网格、块计划、重叠边带和缓冲切片。
- `internal/bound`：量化误差传播上界与 `E_BOUND` 校验。
- `internal/pipeline`：worker pool、预算排队、状态机、三路径入口和报告组装。
- `internal/store`：纯 Go SQLite schema、事务提交、状态、重启身份校验和清理。
- `internal/verify`：独立重算与报告比对；只依赖持久化 JSON payload 和真值指纹。
- `internal/dashboard`：无外部 CDN 的本地诊断页面和 JSON API。
- `cmd/audiocheck`：生成、运行、注入、比较、验证和仪表盘 CLI。
- `cmd/audio-verify`：独立验证工具二进制。
- `acceptance_test.go`：验收测试。
- `scripts/acceptance.sh`：一键本地验收脚本。

## 3. 复现实验

### 3.1 环境

要求 Go 1.22+。SQLite Go 驱动是纯 Go，建议显式关闭 CGO：

```powershell
go version
$env:CGO_ENABLED = "0"
go mod download
```

Windows 查看检查点可安装 SQLite CLI：

```powershell
winget install SQLite.SQLite
sqlite3 --version
```

也可以下载 `sqlite-tools-win-x64-*.zip` 并把 `sqlite3.exe` 加入 `PATH`。

### 3.2 一键测试与演示

Linux/macOS/Git Bash：

```bash
CGO_ENABLED=0 go test ./...
SQLITE3_BIN="$(command -v sqlite3)" bash scripts/acceptance.sh
```

手动运行：

```bash
go run ./cmd/audiocheck gen --dir artifacts/demo \
  --frames 24 --frame-size 64 --hop-size 32 --channels 1 --gear 2 --seed 424242

go run ./cmd/audiocheck run --dir artifacts/demo --mode full \
  --chunk-frames 4 --workers 2 --pending 2 --window 32 --step 8

go run ./cmd/audio-verify --db artifacts/demo/checkpoint.db \
  --truth artifacts/demo/truth.json

go run ./cmd/audiocheck serve --report artifacts/demo/report-full.json \
  --addr 127.0.0.1:8088
```

浏览器打开 `http://127.0.0.1:8088/`。

### 3.3 断点恢复

```bash
# 第一次在块 1 注入一次 E_STAGE，进程返回非零
go run ./cmd/audiocheck run --dir /tmp/ac --mode full \
  --db /tmp/ac/resume.db --report /tmp/ac/failed.json \
  --chunk-frames 2 --fail-block 1 --fail-times 1 --attempts 1

# 清除注入后恢复；块 0 从 SQLite 跳过，其余块补齐并按序提交
go run ./cmd/audiocheck run --dir /tmp/ac --mode resume \
  --db /tmp/ac/resume.db --report /tmp/ac/resume.json \
  --chunk-frames 2
```

### 3.4 异常注入

```bash
go run ./cmd/audiocheck inject --kind truncation
go run ./cmd/audiocheck inject --kind gear
go run ./cmd/audiocheck inject --kind divisibility
go run ./cmd/audiocheck inject --kind stage
```

对应链路：

| 注入 | 错误码 | 定位 |
| --- | --- | --- |
| 序列截断 | `E_DECODE` | 首个不完整帧号 |
| 质量档位越界 | `E_PARA` | `quality_gear` |
| 帧长/帧移不可整除 | `E_SCHEMA` | `frame_size`，消息含 `hop_size` |
| 单块内部失败 | `E_STAGE` | 帧号与块坐标 |

## 4. 关键设计

### 4.1 时间单位与确定性

所有采样位置、帧起点、窗口起点、边带宽度和块坐标都使用整型采样点。指标中的
sum/sum²/cross 项使用 int64 精确累加；DB、SSIM、标准差最终四舍五入到
`*_fixed = value * 10^6`。DCT 系数在量化前转换为 Q24 定点。截断位置由
`eval.TruncationLog()` 与报告中的 `truncations` 声明。

### 4.2 边带

全局窗口从样本 0 按 `step_samples` 排列；帧 `f` 选择中心落在
`[f*hop, f*hop+frame_size)` 的窗口，无窗口命中时确定性选择距帧中心最近的窗口。
块计划枚举这些窗口，得到：

- `left_required_samples`
- `right_required_samples`
- `min_band_samples = max(left,right)`

若显式 `side_band_samples < min_band_samples`，返回 `E_BAND`，错误包含块坐标和
所需最小边带。统计量按同一组窗口、同一顺序合并，因此当前实现的分块与整段结果
逐 fixed 字段相同；声明容差为 SSIM `0.001`、DB `0.001 dB`、噪声 RMS `0.001`。

### 4.3 误差上界

对 Q24 系数量化，最大系数量化误差为：

```text
|e_c| <= q/2 + 0.5 * 2^-24
```

正交 DCT 下，长度 `F` 的单帧采样误差上界为：

```text
B_sample = sqrt(F) * |e_c| + 0.5 + 0.5 * 10^-6
```

窗口能量上界为 `W * B_sample²`，RMS 上界为 `B_sample + 定点舍入项`。
整数 PCM 窗口内最小非零平方误差为 1，因此有限 SNR 上界为
`10 log10(peak² * W)`。这些公式只使用已观测 `q`、`frame_size`、
`window_size` 和定点精度，不读取真值或评估指标。

### 4.4 SQLite 与版本语义

`Store` 设置 `SetMaxOpenConns(1)`，并在 `CommitBlock` 外层加互斥锁；每个块的
block row、frame metrics、queue record、state 和 payload 在一个事务中提交。
payload 内含量化误差、重建采样摘要、manifest/params 和供独立验证器重算的采样缓冲。

严格快照身份包括：

- 原始输入内容哈希 `content_hash`
- `truth_fingerprint`
- `param_version`（参数 schema/语义版本）
- `algorithm_version`
- `schema_version`

其中任一严格身份不匹配，打开数据库时 DROP 并重建全部快照表。具体参数值序列化为
`param_hash`；同一 `param_version` 下调整窗口、滑步或质量档位时，块复用检查会比较
块参数哈希和块坐标，只重算不匹配块。全局参数实际会使所有依赖块受影响；仅增加
充足边带这类不改变指标的缓冲参数不会改变指标哈希，因此不触发重算。

### 4.5 独立验证

`cmd/audio-verify` 只打开 SQLite、读取 `truth_fingerprint` 与 `payload_json`，
并用 `internal/verify` 自带的累加/SSIM/标准差/上界公式重算。它不导入
`internal/eval`、`internal/pipeline`、`internal/bound` 或 `internal/generator`
的计算路径。首个不一致会返回帧号、块坐标和字段名。
