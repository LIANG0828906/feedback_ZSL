# 实际验证结果

验证时间：2026-11-25（Asia/Shanghai）。环境：`go version go1.22.12 linux/amd64`，
SQLite 驱动 `modernc.org/sqlite v1.29.10`（纯 Go）。下列输出均为本工作区实跑得到，
完整日志见 `artifacts/go-test-verbose.txt` 与 `artifacts/acceptance-output.txt`，
机器可读报告见 `artifacts/verification-report.json`。

## 1. 编译与静态检查

```text
$ go vet ./...
VET: clean            # 退出码 0，无任何告警

$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
WIN_BUILD_OK          # Windows 纯 Go 交叉编译通过

$ GOPROXY=off go test ./...   # 断网/仅模块缓存亦可通过
```

## 2. 单元测试：31/31 通过

```text
$ go test -count=1 ./...
ok  replayaudit/internal/audit
ok  replayaudit/internal/engine
ok  replayaudit/internal/formats
ok  replayaudit/internal/store
ok  replayaudit/internal/timeconv
（errs/model/gen/cmd 无测试文件）
```

用例清单（`-v` 中 31 个 `--- PASS`，0 失败）：

- timeconv：`TestTurnMSStartAnchor`、`TestMSToTurnFloor`、`TestRoundTripAlignedIdentity`、
  `TestFloorQuantizationIsStable`、`TestOverflowTaggedWithEventID`、`TestCeilRounding`
- formats：`TestGenerateLoadAndMergeAllPackages`、`TestUnknownFormatCode`、
  `TestMissingRequiredFieldPath`、`TestSequenceSemanticSwapOnDecode`、
  `TestHighLowHighOrderPreserved`
- engine 结算：`TestAllSamplesMatchTruth`、`TestSettlementDeterministicAndPooledEqual`、
  `TestCommitSequenceStable`
- engine 依赖图：`TestGraphRecordsExplicitDependencies`
- engine 增量：`TestPreciseInvalidationSet`、`TestTimeEditDirtiesOldAndNewSegments`、
  `TestFlagEditPropagatesToConditionalEvent`、`TestAddRemoveSyncManifestAndParity`、
  `TestIncrementalParityAcrossSamples`、`TestCompareReportsLocatesField`
- engine 并发：`TestPoolNoLossAndParallelBound`、
  `TestPoolBackpressureRecordedAndBlocks`、`TestPoolConcurrentSubmitSafety`
- engine 断点：`TestCheckpointResumeParityAllSamples`、
  `TestCheckpointPurgeOnVersionChange`、`TestCheckpointConflictCodeMatchesFull`
  、`TestCheckpointEmptyLeadingSegment`
- store：`TestConcurrentCommitsAreSerialAndIntact`、`TestCheckCompatibleLifecycle`
- audit：`TestRunAllAcceptance`

并发与 SQLite 额外跑了竞态检测：

```text
$ go test -race -count=1 ./internal/engine/ ./internal/store/ ./internal/audit/
ok  replayaudit/internal/engine
ok  replayaudit/internal/store
ok  replayaudit/internal/audit
```

## 3. 本地示例 / 端到端验收：23/23 PASS

```text
$ go run ./cmd/replayaudit all
== replay audit verification ==
  r01_basic        events=9  segments=4  winner=blue  truth=true three_paths=true
  r02_conditional  events=8  segments=4  winner=blue  truth=true three_paths=true
  r03_mutex        events=8  segments=5  winner=blue  truth=true three_paths=true
  r04_full         events=9  segments=4  winner=blue  truth=true three_paths=true
  r05_chain        events=5  segments=3  winner=blue  truth=true three_paths=true
  r06_seeded       events=15 segments=6  winner=blue  truth=true three_paths=true
checks: 23/23 passed
ACCEPTANCE: OK        # 进程退出码 0
```

覆盖的验收点：

- 6 份多格式样本经 `format_1/2/3.json` 加载合并后，逐事件生效时刻、状态、
  同步点、胜者与生成器真值一致（容差 0）。
- 全部 54 个事件的 ms→turn→ms 整型往返严格恒等；溢出返回带事件 ID 的 `E_OVF`。
- 三格式语义互换验证：F1 `chain→casters`、F2 `chain→targets`；F1/F2 归一后的
  `(at,id)` 次序完全一致。
- 连续（含 worker pool）、增量、中断后续算三条路径，在相同编辑序列下
  `CompareReports` 全部为 nil（生效时刻/状态快照/同步集/提交哈希序列逐项相等）。
- 精确失效样例：改 `e1` 数值只重算 `[e1 e3]`（同 `hp:x1` RAW 链），
  `e2/e4/e5/e6` 全部命中复用；时间编辑同时置脏旧/新区段。
- 冲突裁决码：优先级裁决成功并留明细；强制型互斥被压制 → `E_CONFLICT event=e2`；
  同优先级矛盾标志 → `E_CONFLICT event=d1`；同步点前失败事件 →
  `E_CONFLICT event=e1`。
- 检查点：提交 2 段后注入中断，重开 SQLite 续算结果与全量一致；参数版本或
  内容哈希不符时旧区段被整体清空后全量重算。
- 并发边界：300 任务零丢失、峰值并行 ≤ 配置上限；单 worker+1 缓冲场景确认
  缓冲耗尽时记录 `worker_buffer_full` 并阻塞，排空后继续，不丢任务。

## 4. 复现实验步骤

```bash
# 1) 依赖（已提交 go.sum；离线环境可直接 GOPROXY=off）
go mod download

# 2) 静态检查 + 全量测试
go vet ./...
go test -count=1 ./...

# 3) 生成样本并执行全部验收（样本写入 ./samples，SQLite 写入 artifacts/checkpoint-dbs）
go run ./cmd/replayaudit all

# 仅重新生成样本
go run ./cmd/replayaudit gen samples
```

样本目录示例：`samples/r01_basic/{format_1.json,format_2.json,format_3.json,truth.json,params.json}`。
