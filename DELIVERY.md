# 触觉反馈跨分支确定性内核 — 交付说明

首个可运行版本使用 Go 1.22+，仅直接依赖纯 Go SQLite 驱动 `modernc.org/sqlite`，不需要 Docker、外部服务或真实账号。

## 快速开始

```powershell
# Windows PowerShell：modernc 为纯 Go 驱动；如本机 C 工具链不完整，建议显式关闭 CGO
$env:CGO_ENABLED="0"
go test ./... -count=1
go run . generate
go run . verify
go run . serve --addr 127.0.0.1:8088
```

Linux/macOS：

```bash
CGO_ENABLED=0 go test ./... -count=1
CGO_ENABLED=0 go run . verify
./artifacts/haptic serve --addr 127.0.0.1:8088
```

验收命令会重新生成：

- `samples/branch_1.json`、`samples/branch_2.json`、`samples/branch_3.json`
- `samples/device_capabilities.json`
- `samples/truth.json`
- `samples/export_losses.json`
- `artifacts/demo/report.json`、`artifacts/demo/verification.md`
- `artifacts/demo/realtime.db`、`artifacts/demo/resume.db`

## 分支语义规则

三个分支使用相同扁平容器和字段名，差异在分支语义表：

| 原始字段 | 分支 1 | 分支 2 | 分支 3 |
| --- | --- | --- | --- |
| `p` | `strength` | `duration_samples` | `strength` |
| `q` | `duration_samples` | `strength` | `duration_samples` |
| `seq` | `type_sequence` | `channel_sequence` | `type_sequence` |
| `aux` | `channel_sequence` | `type_sequence` | `channel_sequence` |
| `at` | 10ms tick | 毫秒 | 5ms 快速 tick |
| `flag` | `0/1` 直编码 | bit0 真值 + bit2 有效位 | `0=true,1=false` 反极性 |

内部统一周期单位为设备采样点（每点 10ms）。时间换算使用有符号/有界整型运算，按规则声明的 `floor` 方向取舍；溢出返回 `E_OVER` 并带字段、帧号、通道和左右两侧值。

每次映射都会生成 `model.Trace`，包含帧号、分支、内部语义名、`$.frames[i].field` 源路径、原始值、内部值和换算规则。诊断台“语义重映射痕迹”表直接展示这些记录。

## 合成与分包

- 条件事件仅在 `conditional=true && flag=true` 时参与合成。
- 同一通道按 `arrival_order`、`frame_no` 稳定排序。
- 重叠事件按“禁止打断 > 高优先级 > 同优先级先到先得”裁决；相同 `merge_key` 的重叠事件合并区间并取最大强度。
- 生效事件使用确定性 permille 模板叠加；Q8 定点量化，量化单位来自设备能力清单。
- 抖动由 SplitMix64 子随机源从 `(stream_seed, frame_no)` 派生，不读取墙钟、 goroutine 调度顺序或全局随机源。
- 包边界按通道固定采样上限切分；全局提交顺序按 `(start_sample, channel, packet_index)` 稳定排序。
- 包状态仅允许 `pending -> quantizing -> split -> committed`、`quantizing -> failed -> quantizing`、未提交包 `split -> quantizing`；非法迁移返回 `E_STATE` 且保持原状态。

## 检查点与三路径

SQLite 表 `packets` 串行保存：

- Q8 帧 JSON 与 `frame_digest`
- 起止/数量/时间窗等切分边界
- 已消费事件偏移、包内事件数
- 流种子、子随机源进度
- 语义指纹与逐包指纹增量
- 内容哈希
- 规则版本、算法版本
- 分支来源、冻结规则快照、裁决明细和事件快照

`Store` 使用 `MaxOpenConns(1)` 与互斥锁，单包记录和 `meta` 在同一事务提交。重启时逐包重算摘要/内容哈希和版本；任一不匹配即清空检查点并全量重算，不会混用旧包。

三条路径为：

1. `full_recompute`：一次性纯内存构建。
2. `realtime_sqlite`：工作线程池合成、状态机推进、逐包事务提交。
3. `resume_sqlite`：先提交前缀，进程强杀后重开 SQLite，仅恢复版本和哈希匹配的前缀，再补齐后续包。

`reference` 包不导入合成主路径；它从包内冻结事件和规则快照自行裁决、渲染、量化，复核帧值、事件数、边界、时间窗、摘要、哈希和真值语义指纹。不一致时返回带 `packet_seq`、`item`、`sample_offset` 的 `E_DIFF`。

独立 CLI 复核：

```bash
go run . review --db artifacts/demo/resume.db --truth samples/truth.json
```

## 异常注入

```bash
go run . inject --kind corrupt      # E_DECODE，定位 frame=3 channel=ch_left
go run . inject --kind order        # E_ORDER，定位 frame=3 channel=ch_left
go run . inject --kind flag --branch 2   # E_FLAG，定位 frame=6 channel=ch_left
```

`verify` 还会注入单包内部失败 `E_STAGE`（默认 `packet_seq=2`、`frame=6`、`ch_left`），诊断台可逐包重试。

## 诊断界面

启动 `go run . serve` 后打开 `http://127.0.0.1:8088`：

- 包状态矩阵和提交进度
- 两通道 SVG 包络曲线、Q8/level 悬停值和包边界
- 60 条字段级语义重映射痕迹
- `E_LOSSY` 损失清单
- 三路径比对和独立复核结果
- `E_DECODE/E_ORDER/E_FLAG/E_STAGE` 注入按钮
- 失败包重试、状态重置和三分支导出下载

## 主要包

- `internal/haptic/errs`：`E_VERSION/E_OVER/E_FLAG/E_DECODE/E_ORDER/E_STATE/E_STAGE/E_DIFF/E_LOSSY/E_HASH`
- `internal/haptic/model`：内部事件、包、指纹、哈希和规则快照
- `internal/haptic/branches`：三分支规则、加载器、导出器、语义比较
- `internal/haptic/envelope`：裁决、模板叠加、Q8 量化
- `internal/haptic/reference`：独立复核与三路径包比较
- `internal/haptic/packetstate`：包状态机和进度
- `internal/haptic/boundary`：包边界和时间窗
- `internal/haptic/concurrency`：最大并行度和每线程采样缓冲预算
- `internal/haptic/checkpoint`：SQLite 原子提交和校验
- `internal/haptic/pipeline`：计划、实时/恢复/全量路径
- `internal/haptic/samplegen`：固定种子样本和真值生成
- `internal/haptic/verify`：一键验收报告
- `internal/haptic/diag`：本地 HTTP/HTML 诊断台
