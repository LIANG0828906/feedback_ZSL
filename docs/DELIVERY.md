# MaskQC 交付说明

一套可集成的 Go 领域组件与本地 HTTP 诊断工作台。样本、Schema 文档、真值和损坏输入均由代码按固定种子 `42042` 确定性生成；运行期不依赖真实美术工具、Docker 或外部网络服务。

## 快速运行

```bash
go test ./... -count=1
go test -race ./... -count=1
go run ./cmd/maskqc verify --workdir artifacts/verify
go run ./cmd/maskqc serve --addr 127.0.0.1:8080
```

浏览器打开 `http://127.0.0.1:8080/`。页面提供图标切换、积算/候选/真值/连通域 PNG、笔画编辑、失效分块、依赖图 JSON、域统计、方向投票、提交历史、预算等待和损坏诊断入口。

SQLite 驱动已经写入 `go.mod`：`github.com/mattn/go-sqlite3 v1.14.24`。该驱动使用 CGO，构建机需要 `gcc`；当前环境已验证 Go 1.22.12 与 gcc 可完成构建。数据库 DSN 使用 `_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on`，并设置 `SetMaxOpenConns(1)`，配合 Store 互斥锁保证提交串行。

## 代码地图

- `model.go`：内部 `Project/Icon/Stroke/Mask/Rect`、稳定排序与分块坐标。
- `generate.go`：固定种子生成 32×32、48×32、40×40 三个图标；候选掩码包含内缩、外溢点和空洞，并生成分块真值哈希。
- `schema.go`、`schema_load.go`：V1/V2/V3 导出、统一加载、必填字段路径、真值块哈希校验、缺省补齐痕迹和 `E_LOSSY` 清单。
- `convert.go`：绝对坐标/笔序偏移、千分比/万分比的纯整型换算、余数、溢出操作数和转换轨迹。
- `raster.go`：纯整型点线段距离、硬度剖面、权重覆盖分量和确定性 alpha-over 合成。
- `topology.go`：4 邻接连通域、重心、外接框、孔洞、Manhattan 距离环带宽度、边界法向投票和逐域真值比对。
- `cache.go`：缓存键、依赖图、编辑模型、旧/新几何并集的精确失效范围。
- `scheduler.go`：最大并行度、每线程像素缓冲上限、在途/队列预算、阻塞等待记录、异常隔离。
- `checkpoint.go`：SQLite meta、提交、分量、分块结果表；单事务笔画检查点和恢复身份校验。
- `pipeline.go`、`verify.go`：裁决、分块计算、增量复用，以及实时/恢复/全量三路径和 `E_DIFF` 定位。
- `web/`：`net/http` 工作台、HTML 模板、PNG 渲染和 HTTP 冒烟测试。
- `cmd/maskqc/main.go`：`verify` 与 `serve` 命令。

## 需求落点

| 要求 | 实现与验证 |
| --- | --- |
| 确定性样本与三版本加载 | `GenerateSample`；`ExportProject/LoadProject`；`TestSchemaVersionsAndRoundTrip`、`TestSchemaErrorPaths` |
| 字段命名/坐标/刻度/硬度/方向编码差异 | V1 蛇形绝对坐标、千分比、无硬度；V2 camelCase 笔序偏移、百分比硬度；V3 嵌套绝对坐标、万分比。方向分别为数字码、大写字串、对象 class |
| 缺省补齐与 E_LOSSY | `DefaultHardnessPermille=700`、`DefaultDirection=unknown`、`DefaultCompositionOrder=100`；`ConvertReport.Lossy` 记录分组结构和低版本方向细节降级 |
| 整型换算与往返 | `ScalePermyriadToPermille`、`ScalePermilleToPermyriad`、`AbsoluteToSequenceOffset`；非 10 对齐权重 `1234` 也通过随文档保存的余数恢复；`TestIntegerConversionsRecordRemainderAndOverflow` |
| E_SCHEMA/E_OVF/E_PARA | 结构化 `maskqc.Error` 含 `Code/FieldPath/Operands/Details`；非法编辑先副本校验，不污染活动模型 |
| 单笔栅格化与 alpha 合成 | `RasterizeStrokeRect`、`AlphaOver`；无 map 迭代或时间随机进入像素计算，输出单通道原分辨率 |
| 唯一合成次序 | 工序优先级升序 → 笔序号升序 → 稳定 ID；`Adjudicate` 和每分块 `TileAudit` 记录笔 ID、笔序号和依据 |
| 拓扑与方向 | `AnalyzeMask`、`CompareTopology`；输出面积、固定千分重心、外接框、孔洞数、环带宽度、方向类别和法向投票；`TestTopologyHoleMismatchReturnsETop` |
| 依赖图与精确失效 | `BuildDependencyGraph`、`PlanInvalidation`；坐标编辑覆盖旧/新 bounds，权重编辑覆盖当前 bounds，插入覆盖相交块；未变分块命中 SQLite tile result |
| 缓存键 | `BuildCacheKey`：tile 内容哈希、stroke ID/hash、次序 rank、`OrderRulesVersion`、`ConversionParamsVersion`、算法版本和排序后的上游键 |
| SQLite 检查点 | `Store.CommitStroke` 在一个事务写 commit + 全部分量；分量含摘要、键、输入哈希、残差摘要、版本；`TestCheckpointCorruptionRejected` |
| 强杀恢复边界 | 三路径中的 recovery 先提交首笔后关闭数据库，再重开、校验 meta/分量哈希并从 `LastCommit` 继续；未提交事务由 SQLite 回滚 |
| 三路径一致 | `RunThreePaths`；`TestThreePathsPixelAndSequenceConsistent` 校验事件序列、分块笔序、逐 tile 像素和域统计；`ComparePathResultsLocatesEDiff` 验证首个坐标/字段 |
| 预算与隔离 | `Scheduler`；`TestBudgetQueueBlocksRecordsWaitAndIsolatesFailures`、`TestEngineFailedTileIsolatedAndRetryPersists` |
| 工作台 | `web.Server`；`TestDashboardHTTPAndEditFlow`；人工 HTTP 核验记录见 `artifacts/final-http-validation.txt` |

## 集成示例

```go
package main

import (
    "context"
    "maskqc"
)

func main() {
    project, _ := maskqc.HarnessProject(maskqc.DefaultSeed)
    store, _ := maskqc.OpenStore("artifacts/app-checkpoint.db")
    defer store.Close()

    meta := maskqc.RecoveryMeta{
        ProjectHash: maskqc.ProjectIdentityHash(project),
        OrderVersion: maskqc.OrderRulesVersion,
        ConversionVersion: maskqc.ConversionParamsVersion,
        AlgorithmVersion: maskqc.AlgorithmVersion,
    }
    _ = store.ValidateRecovery(meta) // 不匹配时返回 E_CORRUPT，调用方应 Rebuild

    engine := maskqc.NewEngine(store, maskqc.DefaultBudgetConfig(), project.ConvertTrace)
    result, _ := engine.BuildIconResult(context.Background(), project.Icon(maskqc.HarnessIconID), true)
    _ = result.Accumulated
    _ = result.Topology

    report, err := maskqc.RunThreePaths(context.Background(), maskqc.ThreePathOptions{
        WorkDir: "artifacts/three-path",
    })
    _, _ = report, err // err 为 *maskqc.Error，Code 可能是 E_DIFF
}
```

## 错误码

- `E_SCHEMA`：未知 schema、必填字段缺失、真值块哈希不符；`FieldPath` 指向如 `images[0].strokes[2].coverage.permille`。
- `E_LOSSY`：不是加载失败，而在 `ConvertReport.LossyError()` 中返回降级清单。
- `E_OVF`：整型换算溢出；`Operands=[2]int64` 给出两个操作数。
- `E_PARA`：硬度、权重、半径等越界；`Details.min/max/value` 给合法区间。
- `E_TOP`：孔洞数、环宽或其他拓扑/方向超容差；`Details.issues` 列域编号、字段、期望值和实测值。
- `E_DIFF`：三路径不一致；`Details` 含首个 tile、`tile_x/tile_y`、全局 `x/y`、字段名以及 expected/actual。
- `E_CORRUPT`、`E_BUDGET`：恢复身份/摘要损坏和缓冲预算类运行错误。

## 验证产物

- `artifacts/final-go-validation.txt`：普通测试与 vet 实际输出。
- `artifacts/final-race-validation.txt`：`go test -race ./...` 实际输出。
- `artifacts/final-tests-verbose.txt`：全部测试名称与结果。
- `artifacts/final-verify.txt`、`artifacts/verify/three-path-report.json`：固定种子三路径报告。
- `artifacts/final-http-validation.txt`：HTTP 页面、PNG、编辑、失败/重试、损坏输入和预算演示核验。

样本中的拓扑 `E_TOP` 是质检发现，不是三路径一致性失败：固定候选掩码包含外溢，标准编辑序列还会插入一笔真值清单中没有的笔画。三条计算路径得到完全相同的问题和统计，这正是诊断台需要展示的结果。
