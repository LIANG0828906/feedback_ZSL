# 掩码笔画工作台：实现与验证记录

## 启动方式

```bash
go version           # 本记录环境：go1.22.12 linux/amd64
go test ./... -count=1
go run ./cmd/verify
go run ./cmd/workbench -addr 127.0.0.1:18090
```

浏览器打开 <http://127.0.0.1:18090>。页面为原生 HTML/JS，无 CDN、无外部账号、无运行时外网服务。
SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.29.10`，HTTP 使用标准库 `net/http`。
若在全新模块缓存中构建，Go 会按 `go.mod` / `go.sum` 获取该纯 Go 依赖；服务运行后不访问外网。

## 代码结构

- `maskqa/generate.go`：固定种子生成 3 个分辨率样本、图像噪声/渐变/色块/半透明方向笔画、候选缺陷、解析真值与边界段。
- `maskqa/schema.go`：`v1/v2/v3` 线协议、必填字段路径、源版本保留、导出与统一装载。
- `maskqa/convert.go`：绝对坐标与笔序号偏移、‰/‱ 整数换算、取整残差、截断位置、`E_OVF` 操作数。
- `maskqa/raster.go`：确定性笔刷覆盖、硬度曲线、权重 alpha、整数 source-over 合成。
- `maskqa/order.go`：`process_priority ASC, stroke_index ASC` 唯一次序、次序版本、逐分块参与笔与冲突列表。
- `maskqa/topology.go`：4 连通域、面积、重心、外接框、孔洞、稳健环带宽度与 `E_TOP`。
- `maskqa/direction_verify.go`：窗口边界点 PCA 切线、Sobel 法向、八方向类别和错配段角度。
- `maskqa/cache.go`：内容哈希、组件/积算缓存键、显式依赖图、编辑影响分块精确比较。
- `maskqa/scheduler.go`：最大并行度、每线程像素缓冲上限、分块队列预算、等待记录、失败隔离与重试。
- `maskqa/checkpoint.go`：SQLite 单连接/互斥串行写入、单事务笔画分量、提交序列、哈希/版本恢复作废。
- `maskqa/engine.go`、`maskqa/paths.go`：增量执行、持久分块复用、实时/恢复/全量三路径比对。
- `maskqa/server.go`、`maskqa/web/index.html`：本地诊断 API 与交互页面。
- `cmd/workbench/main.go`：HTTP 启动入口。
- `cmd/verify/main.go`：命令行可核验入口。

## 关键裁决口径

候选掩码按题设包含内缩、外溢和空洞，是带噪声的先验层。积算时先对候选 alpha 与笔画覆盖做整数 source-over，
再使用完全来自施工记录的解析笔画支撑域做最终裁决：支撑域外的候选外溢清零，支撑域内的候选空洞由笔画覆盖补齐。
最终二值掩码取 `alpha >= cut_alpha_bps`。因此真值只由生成器的解析几何构造，核验代码不反向从处理结果生成真值。

缓存键链包含：分块输入内容哈希、笔画 ID/内容哈希、合成次序版本、参数版本和上游键。积算键再链接前一积算键。
未受影响分块直接从 SQLite `tile_outputs` 读取；受影响分块才重新执行 worker 任务。

## 实际测试结果

完整详细输出保存在 `artifacts/go-test-results.txt`。最近一次结果：

```text
--- PASS: TestOrderAdjudicationAndDeterministicMask
--- PASS: TestRasterParametersAndDeterministicComponent
--- PASS: TestTopologyAndDirectionMismatchDetails
--- PASS: TestSchedulerBufferBudgetFailureAndRetry
--- PASS: TestSchedulerQueueBudgetWaitRecorded
--- PASS: TestDecodeAndMaskDimensionInjections
--- PASS: TestDiffLocatorsReportFirstTile
--- PASS: TestIncrementalCoordinateEditReusesPersistentTiles
--- PASS: TestIncrementalWeightEditKeepsTruthAndReusesTiles
--- PASS: TestThreePathsPixelReportAndSequenceEqual
--- PASS: TestSQLiteHashAndVersionInvalidation
--- PASS: TestThreeVersionLoadAndSchemaErrors
--- PASS: TestVersionRoundTripAndIntegerOverflow
--- PASS: TestIndexOffsetQuantizationResidual
--- PASS: TestHTTPDiagnosticEndpoints
--- PASS: TestSmokeFullRun
PASS
ok  	feedback/maskworkbench/maskqa
```

统计：16 个测试函数全部通过，0 失败；`go vet ./...` 无输出并退出成功。

## 实际 CLI 核验结果

完整输出保存在 `artifacts/verify-output.txt`。摘要：

```text
generated seed=20261126 samples=3

[compact-80x48]  brush=7 truthDomains=2 segments=10 tiles=6
  full-run: mask=EXACT topology=OK direction=OK
  three-paths: OK commits=14 recoveryBoundary=2

[standard-96x72] brush=9 truthDomains=2 segments=10 tiles=9
  full-run: mask=EXACT topology=OK direction=OK
  three-paths: OK commits=12 recoveryBoundary=2

[large-128x96] brush=9 truthDomains=2 segments=10 tiles=12
  full-run: mask=EXACT topology=OK direction=OK
  three-paths: OK commits=18 recoveryBoundary=2

[invalidation] stroke=1 dx=3 affected=[0] recomputed=[0] reused=[1 2 3 4 5] incremental=full

VERIFY RESULT: ALL CHECKS PASSED
```

三个样本的积算掩码均与解析真值逐像素相等；每个样本均有一个带孔环域和一个无孔斜笔域，10 个边界段方向全部一致。

## HTTP 冒烟

自动化测试 `TestHTTPDiagnosticEndpoints` 已覆盖 `/`、`/api/fixtures`、`/api/project`、`/api/image`、
`/api/diagnose`、编辑增量接口和 `/api/three-paths`。页面可展示：

- 叠加分块网格与方向箭头的积算掩码；
- 每分块参与笔的合成次序和裁决依据；
- 编辑后的受影响/复用分块与失效传播链；
- 拓扑域指标、方向段期望/实测角度；
- SQLite 逻辑提交序列；
- 实时、断点恢复、一次性全量三路径哈希和序列比对。
