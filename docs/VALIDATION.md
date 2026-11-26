# 实际验证记录

验证时间：2026-11-27 06:08–06:15（Asia/Shanghai）。

环境：

- Go `go1.22.12 linux/amd64`
- Linux/WSL Ubuntu 环境
- SQLite 运行库 `libsqlite3.so.0`，Python `sqlite3` 模块报告 SQLite `3.46.1`
- 未使用 Docker、第三方 Go module 或网络依赖

## 命令与结果

```text
$ gofmt -l .
# 无输出

$ go test -count=1 ./...
?       quantforensics/cmd/forensics    [no test files]
ok      quantforensics  0.091s

$ go test -race -count=2 ./...
?       quantforensics/cmd/forensics    [no test files]
ok      quantforensics  2.078s

$ go vet ./...
# 无输出

$ go run ./cmd/forensics diagnose
diagnostic report: artifacts/diagnostics/diagnostic-report.json
diagnostic panel:  artifacts/diagnostics/index.html
schema interop:    3/3 semantic round trips equal
three paths:       baseline=true edited=true
blocks/images:     48/3
invalidation palette_entry_orange affected=12 reused=36 computed=12
invalidation per_block_dither   affected=1 reused=47 computed=1
invalidation block_coordinate   affected=1 reused=47 computed=1
fault retry ok:    true
```

详细测试均通过：

- `TestBitDepthIntegerQuantizationAndTriage`
- `TestConfigurableDistanceMetrics`
- `TestCompareLocatesFirstInconsistency`
- `TestSchemaRoundTripAndLosses`
- `TestSchemaErrorsHaveFieldPath`
- `TestHighLowHighPaletteAndCoordinateIdentity`
- `TestFixedPointAndCoordinateRoundTrip`
- `TestQuantizeBlockDeterministic`
- `TestTightToleranceDoesNotChangeMeasurements`
- `TestStateMachineIllegalTransitionKeepsState`
- `TestExactPaletteInvalidationAndReuse`
- `TestFaultRetryIsolation`
- `TestSmokeThreePaths`

## 三版本互操作实测

诊断命令导出的 showcase 工程额外包含一个分组调色板和一个逐块抖动覆盖：

| Schema | JSON 字节 | 加载转换痕迹 | 降级损失项 | 语义往返 |
| --- | ---: | ---: | ---: | --- |
| `forensics.v1` | 25847 | 108 | 2 | equal |
| `forensics.v2` | 25689 | 108 | 2 | equal |
| `forensics.v3` | 24907 | 109 | 0 | equal |

v1/v2 的两个损失分别是分组调色板树扁平化和逐块抖动参数降级。未知版本用例返回 `E_SCHEMA`，字段路径为 `$.schema`。

## 三路径实测

基线工程：

| 路径 | 结果块 | 提交阶段 | 新计算块 | 持久化复用块 | 排队事件 | Worker |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| realtime | 48 | 192 | 48 | 0 | 10 | 4 |
| recovery-partial | 48 | 6 后中断 | 2 | 0 | 15 | 2 |
| breakpoint-recovery | 48 | 192 | 46 | 1 个完整块 + 部分阶段 | 11 | 3 |
| full-recompute | 48 | 192 | 48 | 0 | 14 | 1 |

一致性结果：

- 基线：`consistent=true`，`commitSequencesEqual=true`，三个完整路径提交序列长度均为 192。
- 同一编辑序列（调色板 + 逐块抖动）：`consistent=true`，提交序列长度均为 192。
- 比对粒度覆盖索引、逐像素误差、重建像素、R/G/B 最大偏差、加权色差和容差结论；如有差异会给出块坐标和字段路径。

队列预算为 2、每 worker 像素缓冲上限为 256；队列满时记录 `pending_block_queue_budget_exhausted`、块 ID 和等待纳秒。诊断 JSON 中保留前 20 条等待样本。

## 块类别与取证分级

声明容差为 R/G/B 最大偏差 56、加权色差 72 时：

| 类别 | 块数 | 通过 | 不通过 | 结论 |
| --- | ---: | ---: | ---: | --- |
| `foreground_shape` | 15 | 15 | 0 | all_pass |
| `dense_gradient` | 12 | 6 | 6 | mixed_under_declared_tolerance |
| `translucent_stroke` | 9 | 0 | 9 | all_fail_under_declared_tolerance |
| `embedded_noise` | 9 | 9 | 0 | all_pass |
| `mixed` | 3 | 3 | 0 | all_pass |

收紧到容差 1 后，所有块的实测最大分量偏差和加权色差保持不变，只改变判定结论。

三级取证归类器的三个注入样例分别得到：

- `block_coordinate_conversion`
- `palette_quantization`
- `bit_depth_quantization`

## 精确失效实测

- 编辑橙色调色板条目：12 个实际受影响块失效，36 个未受影响块复用持久化结果。
- 编辑 `shot-panel-b10` 的逐块抖动：仅 1 个块失效，47 个块复用。
- 编辑 `shot-atlas-b09` 的块坐标：仅 1 个块及其 load/index/recon/verify 阶段失效，47 个块复用。
- 故障注入 `shot-main-b00` 首次量化失败：状态为 `failed`；同一 fault policy 第二次调用放行，重试后无失败块，总尝试次数为 2，其他块和已提交结果未被污染。

## SQLite 产物

诊断生成 7 个 SQLite 文件。Python `PRAGMA integrity_check` 全部返回 `ok`：

- `artifacts/checkpoints/realtime.sqlite`：192 条阶段记录。
- `artifacts/checkpoints/recovery.sqlite`：192 条。
- `artifacts/checkpoints/full-recompute.sqlite`：192 条。
- `artifacts/checkpoints/invalidation.sqlite`：235 条（初始 192 + 三次精确失效后的重提交）。
- `artifacts/checkpoints/edited-sequence/{realtime,recovery,full-recompute}.sqlite`：各 192 条。

数据库中提交顺序为稳定的块行优先顺序，每块依次为 `load`、`quantize`、`reconstruct`、`verify`。

## 诊断界面

已验证静态文件：

- `artifacts/diagnostics/index.html`
- `artifacts/diagnostics/diagnostic-report.json`
- `artifacts/diagnostics/screenshots/shot-main.png` 等三张 `32×32` PNG（`file` 验证为 8-bit RGB PNG）。

HTTP 验证：

```text
$ ./artifacts/forensics serve --addr 127.0.0.1:18765
$ curl -fsSL http://127.0.0.1:18765/ | head
<!doctype html>
<html lang="zh-CN">
...
<title>量化色差取证诊断面板</title>
```

## 复现建议

```bash
go test -race ./...
go run ./cmd/forensics diagnose
go run ./cmd/forensics serve
```

然后打开 `http://127.0.0.1:8088/`。
