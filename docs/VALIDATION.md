# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）。完整命令输出另存于 `artifacts/verification.log`。

## 环境

```text
$ go version
go version go1.22.12 linux/amd64
```

SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.29.10`，不需要 CGO 或系统 `libsqlite3-dev`。

## 已执行命令与结果

| 命令 | 实际结果 |
| --- | --- |
| `go mod tidy` | 成功拉取/整理 `modernc.org/sqlite` 及依赖，`go.sum` 完整。 |
| `go vet ./...` | 无输出、退出码 0。 |
| `go test -count=1 ./...` | `internal/sample` 与 `tests` 均 `ok`。 |
| `go test -race ./...` | `internal/sample` 与 `tests` 均 `ok`，无数据竞争。 |
| `go test -count=5 ./...` | 全部通过，未观察到偶发调度差异。 |
| `go run ./cmd/tlscheck gen -out samples` | 生成 3 个 schema 文件和 `truth.json`；重复生成后 `sha256sum -c` 全部 `OK`。 |
| `go run ./cmd/tlscheck verify` | JSON 中 `ok=true`，5 项内置检查全部 `ok=true`。 |
| `go run ./cmd/tlscheck demo ...` | JSON 中 `ok/recovery_outcome_equal/incremental_equal=true`。 |

## 确定性样本观测值

`go run ./cmd/tlscheck verify` 的关键计数：

```json
{
  "schema_hashes": {
    "probe.project.v1": "43a2435e3f8c3b62",
    "probe.project.v2": "43a2435e3f8c3b62",
    "probe.project.v3": "43a2435e3f8c3b62"
  },
  "endpoint_count": 7,
  "chain_count": 9,
  "event_count": 19,
  "result_count": 17,
  "status_counts": {
    "COMPLETED": 14,
    "REJECTED": 3
  },
  "queue_count": 6,
  "backpressure_count": 1,
  "recovery_outcome_equal": true,
  "incremental_equal": true
}
```

关键准入时刻：

- `B-2`：`3000ms`；`B-3`：`5000ms`。
- `G-3/G-4`：预算周期重置后的 `10000ms`。
- `D-1`：冻结解除、链更新提交后的 `4000ms`。
- `H-2`：并行槽释放后的 `2000ms`；`H-3` 得到 `E_BUFFER_FULL` 背压。

## 验收清单映射

| 验收项 | 自动化测试 |
| --- | --- |
| 三版装载后的端点集合、链顺序、请求序列和 canonical hash 一致 | `TestThreeSchemaVersionsLoadEquivalent`、`TestGeneratedSampleFilesReload` |
| 逐请求握手/链逐级结论与真值一致；失败后级别 skipped | `TestDeterministicJudgmentsAndTruth` |
| 预算/并行约束、约束编号、排队序号和最终准入时刻 | `TestDeterministicJudgmentsAndTruth` |
| 任意 worker 数、反向端点顺序不改变判定；线程预算耗尽错误 | `TestParallelOrderAndConcurrencyDoNotChangeJudgment` |
| SQLite 保存/恢复、请求分步中断续做、不重复预算 | `TestCheckpointInterruptResumeAndRecoveryErrors` |
| 链更新提交前中断、冻结和恢复 | `TestChainUpdateFreezeRecovery` |
| 增量有效期/签发关系/周期/追加删除与全量一致 | `TestIncrementalEditsMatchFullRecompute` |
| 未知 schema、字段冲突、越界刻度、成环链 | `TestStableBusinessErrors` |
| 截断检查点、检查点版本不匹配、快照哈希替换 | `TestCheckpointInterruptResumeAndRecoveryErrors` |

## 异常命令示例

```bash
go run ./cmd/tlscheck load -file not-project.json
# stderr JSON: error.code = E_FIELD_INVALID；进程退出码 1

go run ./cmd/tlscheck unknown-command
# stderr JSON: error.code = E_INVALID_ARGUMENT；进程退出码 1
```

强制终止场景由 SQLite 单事务和故障注入测试覆盖；恢复只读取已提交数据库状态，不生成或依赖退出报告。
