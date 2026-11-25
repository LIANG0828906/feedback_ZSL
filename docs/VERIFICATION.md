# 实际验证结果

验证时间：2026-11-25（Asia/Shanghai）。

## 环境

- `go version go1.22.12 linux/amd64`
- 依赖：`modernc.org/sqlite v1.29.10`（纯 Go，无需 CGO）
- Windows 交叉编译：

```text
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o terrainpatch.exe ./cmd/terrainpatch
WINDOWS_BUILD_OK 14M
```

## 自动化测试

命令：

```bash
go test -v -count=1 ./...
go test -race -count=1 .
go vet ./...
```

结果：9 个验收测试全部通过，race 检测通过，`go vet` 无输出。完整详细输出保存在 `docs/test-output.txt`。

```text
--- PASS: TestSampleVerificationAcceptance (0.14s)
--- PASS: TestMeshDeterminismAndBudgetCode (0.05s)
--- PASS: TestTightBudgetIncreasesErrorAndReducesBoundaryVertices (0.00s)
--- PASS: TestLoaderSchemaAndMetaErrors (0.00s)
--- PASS: TestEditParameterAndImpactPropagation (0.03s)
--- PASS: TestFailureIsolationAndRetryMatchesCleanPath (0.03s)
--- PASS: TestMemoryBudgetBlocksAndRecordsQueue (0.14s)
--- PASS: TestSQLiteVersionMismatchResetsWholeSnapshot (0.02s)
--- PASS: TestHTTPDiagnosticAPI (0.04s)
ok  	terrainpatch
```

这些测试覆盖：

- 固定种子样本生成、加载和全部真值；
- 32 个 goroutine 中同一地块摘要一致；
- `E_BUDGET`、`E_SCHEMA`、`E_META`、`E_PARA`；
- 编辑只失效本块和邻接块，非邻接块复用；
- 单块注入失败不删除其他块缓存，重试结果与干净路径逐项一致；
- 小内存预算下 `memory-budget` 阻塞排队；
- SQLite 参数版本不匹配时整体作废快照；
- `net/http` 健康检查、状态和编辑 API。

## CLI 样本验收

命令：

```bash
go run ./cmd/terrainpatch generate -dir samples/local -seed 42
go run ./cmd/terrainpatch verify -samples samples/local -db data/final-acceptance2.db
```

精简机器可读结果：`docs/verification-summary.json`；完整 JSON：`docs/verification-report.json`。

```json
{
  "ok": true,
  "checks": [
    { "name": "triangulation-budget", "ok": true },
    { "name": "analytic-expected-heights", "ok": true },
    { "name": "declared-max-triangles", "ok": true },
    { "name": "shared-boundary-aligned-sequence", "ok": true },
    { "name": "fault-attribution-truth", "ok": true },
    { "name": "three-path-consistency", "ok": true }
  ],
  "threeWay": {
    "equal": true,
    "firstDifference": null,
    "commitSequence": ["T00", "T10", "T01", "T11"],
    "recoveryReset": false,
    "recovered": ["T00", "T01", "T10", "T11"]
  }
}
```

20 个解析期望高程点全部匹配；4 个块的三角形数均未超过声明上限。样本声明上限与实测值为：

| 块 | 声明上限 | 基准实测三角形 |
| --- | ---: | ---: |
| `T00` | 64 | 32 |
| `T10` | 512 | 512 |
| `T01` | 512 | 512 |
| `T11` | 512 | 512 |

## 边界归因实测

基准全量路径的四条边界：

| 边界 | 最大折角 | 归因 | A/B 高度误差 | 定位 |
| --- | ---: | --- | ---: | --- |
| `E01` | 9.9938° | `undersampling` | 140 / 0 | `16000,8000` |
| `E02` | 9.9565° | `undersampling` | 140 / 0 | `8000,16000` |
| `E13` | 2.5671° | 未超限 | 0 / 0 | `17000,16000` |
| `E23` | 2.6668° | 未超限 | 0 / 0 | `16000,17000` |

另有两个生成器探针：

- `P-ALIGN`：低高度误差但边界点顺序交换，归因 `alignment_mismatch`，真值位置 `Y=4000`。
- `P-ATTR`：精确采样但存在锐折角，高度判据和对齐判据均不成立，返回 `E_ATTR`，实测位置 `X=32000,Y=8000`。

`fault-attribution-truth` 检查确认 4 条真值故障/探针全部匹配。

## 三路径提交序列

- 快照块顺序在草稿增量、断点恢复、全量路径中均为 `T00,T10,T01,T11`。
- 自动比对结果 `equal=true`，`firstDifference=null`。
- 中断恢复场景中，`T00` 的新版本已提交，其余块的版本匹配检查点被恢复；随后完成邻接块重对齐。
- 每轮 `writtenSequence` 只包含实际失效块；全量路径写入全部 4 块。快照顺序与实际写入顺序的区别在 `docs/TECHNICAL.md` 中说明。

## HTTP 实测

共享沙箱的 `127.0.0.1:8080` 当前被无关进程 `battlekernel`（PID 242669）占用，因此未停止该进程，改用 `127.0.0.1:18081` 实测：

```text
GET  /             -> 200, 14567 bytes
GET  /healthz      -> {"status":"ok"}
GET  /api/state    -> 4 tiles, 4 boundaries, commitSequence=[T00 T10 T01 T11]
POST /api/edit     -> ok=true; T11 编辑使 T10/T01/T11 失效，T00 缓存命中
POST /api/verify   -> ok=true, equal=true, recoveryReset=false
GET  /api/export   -> 200, 172727 bytes
```

在干净 Windows 环境中，`scripts\serve.bat` 默认监听 `:8080`；若端口被占用，可按 README 改用 `-addr :18080`。
