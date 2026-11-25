# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）

环境：

```text
$ go version
go version go1.22.12 linux/amd64
```

## 自动化测试

最终命令：

```bash
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
```

实际结果：

```text
?       ballistic/cmd/diagnostic       [no test files]
?       ballistic/internal/snapshot     [no test files]
ok      ballistic/internal/diagnostic  0.150s
ok      ballistic/internal/kernel      0.013s
ok      ballistic/internal/workbench   0.342s
VET_OK

ok      ballistic/internal/diagnostic  3.129s   # -race
ok      ballistic/internal/kernel      1.076s   # -race
ok      ballistic/internal/workbench   5.859s   # -race
```

覆盖率：

```text
ok      ballistic/internal/diagnostic  coverage: 53.4% of statements
ok      ballistic/internal/kernel      coverage: 58.7% of statements
ok      ballistic/internal/workbench   coverage: 75.9% of statements
```

关键测试：

- `TestThreeSchemaVersionsLoadAndPredictIdentically`
- `TestExportV1RoundTripPreservesEndpointsAndRestitution`
- `TestHighLowHighFieldRoundTripAndIntegerConversions`
- `TestIncrementalEndpointDowngradeCreatesLossTrace`
- `TestSchemaErrors`
- `TestTruthSamplesBouncesAndAttribution`
- `TestHandCalculatedVerticalBounce`
- `TestBoundedPoolQueuesWhenExhausted`
- `TestBoundedPoolGeometryBufferLimit`
- `TestEndpointEditIncrementalMatchesFullAndReusesTiles`
- `TestRestitutionEditPropagatesToAttributionsAndMatchesFull`
- `TestForcedStopReopensCommittedSnapshotsAndResumes`
- `TestStaleContentHashIsInvalidatedAndRecomputed`
- `TestDiagnosticHTTPWorkbench`

## 确定性样本结果

固定种子：`0x42A8691E`。

HTTP `/api/state` 实际摘要：

```text
tiles 135 snapshots 135 log 135
req_vertical   first=floor_main  bounces=2  escaped=False
req_diagonal   first=floor_main  bounces=1  escaped=True
req_wall       first=wall_right  bounces=2  escaped=True
req_escape     first=            bounces=0  escaped=True
truth samples=199, bounces=5
```

无命中的 `req_escape` 显式输出 `outcome=escaped_no_hit`，不会被跳过。

## 增量/全量逐像素一致性

端点编辑实际 API 结果：

```text
edit cmp True True True True
plan invalid 4 reused 131 attrs []
```

四个布尔值依次为：

1. `pixel_equal`
2. `hash_equal`
3. `tile_metrics_equal`
4. `hit_stats_equal`

反弹系数编辑由 `TestRestitutionEditPropagatesToAttributionsAndMatchesFull` 验证：`req_vertical` 与 `req_diagonal` 归因失效，存在下游 tile，增量帧与空内存 SQLite 的全量参考帧逐像素相等。

## 强制终止与续算

自动化测试 `TestForcedStopReopensCommittedSnapshotsAndResumes` 先稳定提交前 5 个 tile，然后返回到强制终止边界；关闭并重新打开同一 SQLite 文件后：

```text
reused 5 tiles want 5
commit order: tile 0..4 in canonical order
resume/full: PixelEqual/HashEqual/TileMetricsEqual/HitStatsEqual all true
```

`TestStaleContentHashIsInvalidatedAndRecomputed` 篡改已提交 tile 的 `content_hash`，重新加载时该 current 行被整体作废，随后该 tile 被重算。

另做了真实进程验证：

1. 启动诊断服务并完成 135 个快照。
2. 调用端点编辑，使 4 个 tile 的 SQLite 内容变为编辑后版本。
3. 使用 `kill -9` 终止进程。
4. 用同一 SQLite 文件重启。

重启后 `/api/state` 实际返回：

```text
reused 131 rendered 4
rendered_ids ['tx00_ty02', 'tx01_ty02', 'tx00_ty03', 'tx01_ty03']
```

这表示 131 个有效快照被复用，4 个内容哈希不匹配分块被作废并重算。

## 异常注入

五个 HTTP 注入用例均返回 `ok=true`：

```text
E_SCHEMA field=$.level_id
E_DECODE field=$.obstacles[id=wall_right] id=wall_right
E_ORDER  field=$.truth.samples[2].logical_frame id=req_vertical
E_PARA   field=$.segments.wall_right.restitution id=wall_right
E_BLOCK  field=$.tile id=tx07_ty04
```

## 本地工作台手动验证

```bash
go build -o /tmp/ballistic-diagnostic ./cmd/diagnostic
/tmp/ballistic-diagnostic --addr 127.0.0.1:18080 --db .diagnostic/ballistic.sqlite
```

实际验证过：

- `GET /` 返回内嵌 HTML。
- `GET /api/state` 返回场景、弹道、tile、哈希、快照、版本、并发审计和 schema 转录。
- `POST /api/edit?kind=endpoint` 与 `kind=restitution` 返回失效计划和比对结果。
- `POST /api/inject?case=...` 返回结构化错误定位。
- `GET /api/frame.png` 返回 `960×576` PNG，像素缓冲来自实际 RGBA frame。
- 页面中的 SVG 展示障碍、弹道折线、反弹点、首次命中、失效/复用 tile；右侧展示归因、失效传播、SQLite、worker pool 和 schema traces。
