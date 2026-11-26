# LayerForge

LayerForge 是一个只依赖 Go 标准库的分层构建缓存领域组件，同时提供一个固定种子的本地演示应用。它覆盖三版 schema 装载、确定性缓存键/层摘要、DAG 共享依赖、交互延迟预算调度、预算降级、SQLite 检查点恢复、增量编辑、缓存清理和异常注入。

默认样本由生成器按种子 `42` 产出，工程为 `fixture-atlas-service`，包含 3 个基础层、8 个步骤和 11 个层。三版文档的语义哈希均为：

```text
sha256:7e81a33b1f5e1d8241aae12708ef24c71ee2d60d4c81caa2de507f6adc969396
```

## 快速开始

环境：Linux / WSL Ubuntu、Go 1.22+、`go`、`gofmt`、`go test`。项目不使用 Docker、第三方 Go 包、外部服务或网络。

```bash
go test ./...
gofmt -l .
go run ./cmd/layerforge generate -seed 42 -out artifacts/generated
go run ./cmd/layerforge verify -seed 42 -dir artifacts/generated
go run ./cmd/layerforge demo -seed 42 -workdir .layerforge-demo
```

`demo` 会在工作目录内重建完整演示，依次验证：

- 三版装载一致性与生成器解析真值；
- 冷构建 6 轮完成，并输出每轮已建层、延后层、延迟余量、线程和峰值内存；
- 重启后 8 层全部命中；
- 修改第 4 步命令指纹后，仅重建第 4、5、6、8 步，且与一次性全量重建逐层一致；
- 共享层清理冲突被拒绝，终止层清理后仅重建被清理层；
- 第 2 轮后中断并从 SQLite 最后完整轮续建；
- 字段类型错误、未知字段、环、缺失基础层、共享冲突、越界线程/内存、缓冲背压、清理冲突、截断检查点和版本不匹配快照的稳定业务码。

成功时最后输出：

```text
source_unchanged=true workspace=.layerforge-demo
PASS
```

## 命令

```text
layerforge generate [-seed 42] [-out artifacts/generated]
layerforge verify   [-seed 42] [-dir artifacts/generated]
layerforge build    -manifest file -db file [budget flags]
layerforge demo     [-seed 42] [-workdir .layerforge-demo] [-json]
```

`build` 可用参数：

```text
-reset
-interrupt-after N
-latency-ms N
-max-rounds N
-threads N
-memory-bytes N
-buffer N
-json
```

可通过环境变量在指定轮次和提交阶段注入故障，用于验证原子提交：

```bash
LAYERFORGE_FAULT_ROUND=3 \
LAYERFORGE_FAULT_STAGE=before-rename \
go run ./cmd/layerforge build \
  -manifest artifacts/generated/project.v1.json \
  -db .layerforge-demo/checkpoint.db
```

可处理退出信号由 `SIGINT`/`SIGTERM` 转为 `context` 取消。引擎只在完整轮次提交后检查取消，因此不会把半轮写入检查点。

## 代码结构

| 路径 | 职责 |
| --- | --- |
| `internal/model` | 规范层图模型、预算、语义哈希 |
| `internal/generator` | 固定种子样本、三版文档与真值生成 |
| `internal/loader` | 三版严格装载与单位归一 |
| `internal/build` | DAG 校验、共享关系、拓扑、缓存键和层摘要 |
| `internal/engine` | 预算调度、并行构建、降级、增量、清理、恢复 |
| `internal/checkpoint` | SQLite 检查点状态、事务顺序与完整性校验 |
| `internal/sqlitefile` | 标准库实现的最小 SQLite 3 文件读写 |
| `internal/apperr` | 稳定业务码与步骤/层/路径上下文 |
| `pkg/layerforge` | 面向外部模块的稳定集成门面 |
| `cmd/layerforge` | 生成、校验、构建和完整演示入口 |
| `artifacts/generated` | 种子 42 的三版工程与解析真值 |

## 集成方式

外部模块通过 `pkg/layerforge` 使用组件，不需要依赖 `internal` 路径：

```go
package main

import (
	"context"
	"log"

	"layerforge/pkg/layerforge"
)

func main() {
	manifest, err := layerforge.LoadManifestFile("artifacts/generated/project.v3.json")
	if err != nil {
		log.Fatal(err)
	}
	builder, err := layerforge.NewBuilder(manifest.Project, ".cache/layers.db")
	if err != nil {
		log.Fatal(err)
	}
	result, err := builder.Build(context.Background(), layerforge.BuildOptions{})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("built=%d hits=%d rounds=%d", result.BuildCount, result.HitCount, len(result.Rounds))
}
```

`Derive` 可独立获取图与真值，`EditStep` 在副本上编辑，`Cleanup`、`LoadCheckpoint` 暴露检查点操作，`ErrorCode`/`IsCode` 用于稳定错误分派。

## 三版 schema

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 判别字段 | `schema: layerforge/v1` | `schemaVersion: v2` | `schemaVersion: v3` |
| 工程字段 | `projectId/projectName` | `project.id/project.name` | `manifest.projectId/name` |
| 基础层 | `baseLayers` | `baseRefs` | `bases` |
| 步骤 | `steps` | `tasks` | `operations` |
| 依赖字段 | `dependsOn` | `dependsOn` | `requires` |
| 层元数据 | 扁平 `layers` | 嵌套 `layerTree` | 扁平 `layers` |
| 尺寸单位 | 字节 | KiB | 字节 |
| 时间单位 | 秒 | 毫秒 | 毫秒 |
| 缓存键派生字段 | 无 | 无 | `cacheKeyDerivation` |
| 共享字段 | `shares` | `sharedLayers` | `reuse` |

所有对象均按版本白名单逐字段校验。未知字段不会被 `encoding/json` 静默忽略，而是返回带 JSON 路径的 `E_SCHEMA_FIELD`。大小、内存和秒/毫秒均先换算到内部 `int64` 字节与 `time.Duration`，再用语义哈希验证三版一致。

## 确定性与缓存口径

缓存键只使用声明输入：

```text
sha256-length-prefixed(
  "layerforge-cache-v1",
  step.name,
  step.commandFingerprint,
  sorted(input.path, input.digest),
  sorted(dependency.id, dependency.contentDigest)
)
```

层摘要：

```text
sha256-length-prefixed(
  "layerforge-layer/v1",
  outputLayer.id,
  outputLayer.sizeBytes,
  step.cacheKey
)
```

基础层摘要由生成器按种子、层 ID 和尺寸确定性生成。调度线程数、装载顺序、提交批次、schema 版本、时间戳和进程状态都不进入缓存键或层摘要。

`artifacts/generated/truth.json` 逐步骤记录缓存键、层摘要、尺寸和依赖，并在测试及 `verify`/`demo` 中逐层核对。

## 预算、并行与降级

构建成本只由声明的输出尺寸派生：

```text
每层构建成本 = 2ms + 2ms * ceil(sizeBytes / 4KiB)
每层最小内存 = max(128B, sizeBytes / 2)
```

调度器每轮：

1. 只选择依赖已命中或已完成的步骤；
2. 队列容量为 `threadLimit * bufferPerThread`；
3. 将步骤贪心分配到剩余延迟预算可容纳的工作线程；
4. 单轮延迟为各线程负载最大值；
5. 未进入本轮的 ready 层记录 `latency`、`threads` 或 `buffer` 原因及本轮延后位次；
6. 使用有界 goroutine worker 并行物化，结果按步骤号稳定排序。

降级规则：

- 每线程内存小于层最小需求：切换 `serial-streaming`，单线程、流式构建，报告峰值内存不超过预算；
- 单层成本大于整个延迟预算：标记 `uninterruptible-single-layer`，该轮显式记录超出的延迟，不放弃构建；
- 声明轮数内无法完成：标记 `batch-continue`，继续按每轮延迟预算分批推进；
- 缓冲容量为零/越界：`E_BACKPRESSURE_QUEUE`；
- 调度期缓冲不足：产生可观测 `BackpressureEvent` 和延后记录，不静默阻塞。

所有降级最终仍与无预算全量构建逐层摘要一致，测试会直接比较结果映射。

## SQLite 检查点

检查点是可直接由标准 SQLite 打开的真实 SQLite 3 文件，表为：

- `meta(key, value)`：检查点版本、工程 ID、代数、最后完整轮；
- `layers`：已提交层状态；
- `cache_index`：缓存键到已提交层的索引；
- `rounds`：每轮已建/延后层、延迟消耗、线程和峰值内存。

检查点不使用第三方驱动。`internal/sqlitefile` 直接生成 SQLite 表叶页、记录和 `sqlite_master`，通过标准 SQLite 的 `PRAGMA integrity_check` 验证为 `ok`。

提交语义：

1. 先合并本轮层状态；
2. 清理该层旧缓存键，再为新层写入缓存索引；
3. `Snapshot.Validate` 校验每个索引必须指向 matching 的 `committed` 层，且每个 committed 层必须有索引；
4. 写临时文件、`fsync`、原子 `rename`、目录 `fsync`。

因此任何索引都不可能指向未完成层。版本不匹配时整库重置并全量重建，结果中 `Recovery.Reset=true` 且代码为 `E_CHECKPOINT_VERSION`；截断文件返回 `E_CHECKPOINT_TRUNCATED` 且不覆盖原文件。进程在提交前被终止时旧快照完整，在 rename 后被终止时新快照完整。

当前 SQLite 写入器针对检查点这种小表设计，每个表使用单个 64 KiB 叶页；超出一页会返回明确错误而不是写出损坏文件。

## 增量编辑与清理

编辑通过 `engine.EditStep` 在工程副本上完成，源工程不被修改。引擎比较新旧步骤的输入集合、命令指纹、依赖和输出声明：

- 直接变化步骤输出 `edit:inputs`、`edit:command` 等来源；
- 下游步骤输出 `upstream:N:edit:...`；
- 未受影响且键匹配的层标记 `hit/source=cache`；
- 最终摘要与同一编辑后工程的全量冷构建比较。

清理只允许终止层。若候选被任何步骤依赖或属于基础层，返回 `E_CLEANUP_CONFLICT`，整次清理不提交。成功时先在内存中删除索引，再删除层状态，最后原子提交；下一次构建只重建被清理层。

## 业务码

| 业务码 | 场景 |
| --- | --- |
| `E_SCHEMA_VERSION` | schema 判别失败 |
| `E_SCHEMA_FIELD` | 未识别字段或字段组合 |
| `E_SCHEMA_REQUIRED` | 必填字段缺失 |
| `E_SCHEMA_TYPE` | 字段类型错误 |
| `E_SCHEMA_UNIT` | 单位/秒毫秒换算错误 |
| `E_SCHEMA_CACHE_KEY` | 不支持的缓存键派生算法 |
| `E_LAYER_METADATA` | 层元数据重复、缺失或与步骤不符 |
| `E_INPUT_CONFLICT` | 输入路径重复或摘要冲突 |
| `E_GRAPH_MISSING_REF` | 缺失基础层/依赖 |
| `E_GRAPH_CYCLE` | 步骤环 |
| `E_GRAPH_SHARE_CONFLICT` | 共享消费者与步骤依赖不一致 |
| `E_GRAPH_SHARE_MISSING` | 多方复用但未声明共享 |
| `E_GRAPH_DUPLICATE` | 重复步骤、层或依赖 |
| `E_BUDGET_THREADS` | 线程预算越界 |
| `E_BUDGET_MEMORY` | 内存预算越界 |
| `E_BUDGET_LATENCY` | 延迟降级或延后 |
| `E_BACKPRESSURE_QUEUE` | 缓冲容量耗尽/越界 |
| `E_CLEANUP_CONFLICT` | 清理仍被引用的层 |
| `E_CHECKPOINT_VERSION` | 检查点版本不匹配 |
| `E_CHECKPOINT_TRUNCATED` | 检查点截断 |
| `E_CHECKPOINT_INTEGRITY` | 索引/层状态不一致 |
| `E_RECOVERY_MISMATCH` | 检查点属于其他工程 |
| `E_INTERRUPTED` | 可处理中断 |

错误文本同时包含 `path`、`steps`、`layers` 等可定位上下文；调用方应依赖业务码而不是文本。

## 实际验证

本次实现已在 Go 1.22.12 / Linux amd64 验证：

```text
$ go test ./...
ok layerforge/cmd/layerforge
ok layerforge/internal/engine
ok layerforge/internal/loader
ok layerforge/internal/sqlitefile

$ go run ./cmd/layerforge verify
project.v1.json schema=layerforge/v1 hash=sha256:7e81... truth=OK
project.v2.json schema=v2            hash=sha256:7e81... truth=OK
project.v3.json schema=v3            hash=sha256:7e81... truth=OK
verified seed=42 steps=8 layers=11

$ go run ./cmd/layerforge demo
cold hits=0 builds=8 rounds=6 codes=[E_BUDGET_LATENCY]
round 1 built=[step:compile step:docs] latency=14/17ms remaining=3ms
round 2 built=[step:test step:lint] deferred=[step:package(latency#1)] latency=8/17ms remaining=9ms
restart hits=8 builds=0 rounds=0
incremental edits=step-4 hits=4 builds=4
incremental_vs_full=identical
cleanup conflict_code=E_CLEANUP_CONFLICT removed=[step:release] rebuilt_hits=7
interruption persisted_rounds=2 resumed_hits=4 resumed_builds=4
source_unchanged=true
PASS
```

此外使用系统 Python 的 SQLite 3 对演示检查点做外部只读校验（仅验证，不是项目运行依赖），`PRAGMA integrity_check` 返回 `ok`，并能读取 `meta/layers/cache_index/rounds` 四张表。

跨 schema 复用也已验证：用 v2 完成冷构建后，再用 v3 打开同一检查点，结果为 8 层全命中、0 层重建。

更细的设计与验收记录见 `docs/design.md` 和 `docs/verification.md`。
