# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）。环境为 WSL/Linux，未使用 Docker、外部账号或线上服务。

## 环境与构建

完整输出保存在 `artifacts/build-validation.txt`：

```text
$ go version
go version go1.22.12 linux/amd64
$ go env GOOS GOARCH CGO_ENABLED
linux
amd64
1
$ go build ./...
BUILD_OK
$ go vet ./...
VET_OK
```

模块直接依赖只有标准库与 `modernc.org/sqlite v1.29.10`；`go.mod` 的语言版本保持为 `go 1.22`。

## 单元与验收测试

命令：

```bash
go test ./... -count=1 -v
```

详细输出保存在 `artifacts/test-results.txt`，最终摘要保存在 `artifacts/final-test-summary.txt`。13 个测试全部通过：

- `TestSmokeGeneratedTruth`
- `TestTraceStableEmptyAndOverlapSpans`
- `TestHalfPixelTileOwnershipAndBoundaryContinuity`
- `TestBudgetGateQueuesParallelAndShadowWait`
- `TestVerifyTruthReport`
- `TestDensityUnchangedDoesNotInvalidate`
- `TestCoefficientEditCacheScope`
- `TestThreePathsPixelInvalidationAndCommitSequence`
- `TestPreviewAndFullConsistency`
- `TestStepErrorMonotonicOnTruthPixels`
- `TestExceptionChains`
- `TestBlockFailureRetryIsolation`
- `TestDiagnosticHTTPFlows`

竞态检测：

```bash
go test -race ./... -count=1
```

结果保存在 `artifacts/race-results.txt`：

```text
?       volfog/cmd/fogdiag      [no test files]
ok      volfog/diag     15.972s
ok      volfog/fog      38.931s
```

## 确定性 CLI 验收

命令：

```bash
go run ./cmd/fogdiag --verify
```

机器可读报告：`artifacts/verification-report.json`。最近一次结果的关键字段为：

```json
{
  "ok": true,
  "truth": {
    "pixel_truths": 8,
    "density_truths": 9,
    "max_trans_error": 7.771561172376096e-16,
    "max_radiance_error": 1.0408340855860843e-17,
    "max_density_error": 0,
    "monotonic_budgets": [1, 2, 4, 8, 16, 32, 64]
  },
  "three_paths": {
    "ok": true,
    "blocks": 24,
    "pixels": 1296,
    "expected_invalidation": [9, 10],
    "incremental_computed": [9, 10],
    "incremental_cache_hits": [0,1,2,3,4,5,6,7,8,11,12,13,14,15,16,17,18,19,20,21,22,23],
    "resume_recovered": [0,1,2,3,4,5,6],
    "full_sequence": [0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23],
    "incremental_sequence": [0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23],
    "resume_sequence": [0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23],
    "first_block_mismatch": null,
    "first_pixel_mismatch": null
  }
}
```

这对应验收要求：

- 生成样本的 T、辐亮度和密度积分真值均在容差内；
- 增加步进预算时误差单调不增（实现中对 8 个真值像素遍历 `1,2,4,8,16,32,64`）；
- 声明的密度编辑只失效块 `[9,10]`，其余 22 块缓存命中；
- 实时增量、断点恢复、一次性全量重算的 1296 个像素完全一致；
- 三条路径的块提交序列完全一致；
- 字段级块比较没有发现首个差异块。

## HTTP 工作台冒烟

已实际启动 `/tmp/fogdiag` 并请求：

- `GET /`：返回内嵌 HTML；
- `GET /api/state`：返回 24 个块和 `48×27` 首帧；
- `POST /api/render/preview-compare`：`ok=true`，预览/全精度在声明容差内一致；
- `POST /api/edit/density`：失效块和实际重算块均为 `[9,10]`，缓存命中 22；
- `POST /api/faults/inject {"kind":"decode"}`：返回 `E_DECODE` 与介质 ID；
- `POST /api/faults/inject {"kind":"block","block_id":7}`：返回 `E_BLOCK`，失败块为 `[7]`；
- `POST /api/faults/retry {"block_id":7}`：清理故障后块 7 重新提交成功；
- `POST /api/verify/paths`：`ok=true`，返回 24 块、1296 像素的三路径报告。

## 强制终止说明

自动化测试使用第 7 个块事务成功后的 `ErrSimulatedCrash` 模拟进程中断：SQLite 中只存在块 `0..6` 的目标修订检查点；重新打开数据库后 `TestThreePathsPixelInvalidationAndCommitSequence` 验证恢复结果与全量路径逐像素一致。

由于检查点使用单事务和 `synchronous=FULL`，对运行中的工作台使用 `kill -9` 也只会留下已提交事务；WAL 中未提交部分由 SQLite 恢复机制处理。内核不生成也不依赖退出报告。
