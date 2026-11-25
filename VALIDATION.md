# 实际验证结果

最近一次完整运行：2026-11-26 02:35:08 +08:00，WSL/Linux，
`go version go1.22.12 linux/amd64`。完整控制台记录见
`artifacts/validation-run.txt`，结构化报告见
`artifacts/verification-report.json`。

## 命令与结论

| 命令 | 结果 |
| --- | --- |
| `go test ./...` | 通过 |
| `go test -race ./...` | 通过，无数据竞争 |
| `go vet ./...` | 通过 |
| `GOPROXY=off go build ./...` | 通过；仅标准库，无 `go.sum` |
| `go run ./cmd/hckernel demo -seed 42` | `ok=true`，31/31 项检查通过 |

## 自动化测试覆盖

- `TestSchemaVersionsLoadEquivalent`：三版文档装载后的规范指纹、实例集合和逐轮结果一致。
- `TestUnknownSchemaCode`：未知 schema 返回 `E_SCHEMA_VERSION_UNSUPPORTED`。
- `TestGeneratedSchedulingBranches`：应发/抑制/排队/丢弃和四态迁移分支均出现且与真值一致。
- `TestIncrementalEditsMatchFull`：阈值、周期、权重、机架四类编辑的增量结果等于全量重算，并输出来源。
- `TestWeightScaleInvariant`：千分比、万分比、PPM 的整数预算分配相同。
- `TestParallelBackpressure`：worker 预算、worker 占用和 buffer 上限返回稳定背压码。
- `TestZeroBudgetRejectsInsteadOfDispatching`：全局预算为 0 时零放行并返回 `E_BUDGET_ZERO`。
- `TestSlotPriorityIsDeterministic`：`deferred_first` 与 `periodic_first` 裁决可复现，乱序提交不改变结果。
- `TestCheckpointResumeAndExceptionCodes`：第 3 轮状态后、游标前模拟崩溃，重启只读到第 2 轮，续算与全量第 3 轮之后一致。
- `TestInputExceptionCodes`：零周期、负超时、阈值越界、机架缺失返回正确业务码。

## 种子样本规模

固定种子 `42` 的主样本包含：

- 6 个初始实例、3 个机架；第 14 轮新增 1 个实例并执行迁移/删除事件；
- 14 个逻辑轮次；
- 31 条健康轨迹/调度派生观测；
- 29 个实际发出的探测；
- 延后队列峰值 3；
- 1 个超过最大延后时长的确定丢弃：第 6 轮 `be-06` / `E_DEFER_TIMEOUT`。

一次复验中 14 个并行批次的实际 `duration_ns` 总和由
`artifacts/verification-report.json` 记录；该数值是诊断观测值，会随机器负载变化，不参与确定性真值比较。

## 异常夹具

`samples/seed-42/invalid/` 包含：

- `unknown-schema.json` → `E_SCHEMA_VERSION_UNSUPPORTED`
- `negative-timeout.json` → `E_NEGATIVE_TIMEOUT`
- `threshold-out-of-range.json` → `E_THRESHOLD_OUT_OF_RANGE`
- `missing-rack.json` → `E_RACK_REFERENCE_MISSING`
- `zero-interval.json` → `E_PROBE_INTERVAL_ZERO`
- `truncated-checkpoint.bin` → `E_CHECKPOINT_TRUNCATED`

版本不匹配检查由验证器写入临时检查点并以错误指纹重启载入，稳定返回
`E_CHECKPOINT_VERSION_MISMATCH`。
