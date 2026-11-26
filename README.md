# Wardrobe Batch Kernel

角色换装部件合批渲染数据内核与装配冲突诊断组件。项目只依赖 Go 标准库和仓库内代码，提供：

- 工程加载与 `E_SCHEMA` 字段级结构校验；
- 与输入顺序、并发调度无关的装配合法性判定；
- 确定性合批、子网格偏移和预算校验；
- 材质槽争用、预算挤压的可解释仲裁与 `E_ARB`；
- 部件、材质槽、挂点、预算、批次和偏移依赖图；
- 精确缓存失效、增量重划分及 `E_DIFF` 首差异定位；
- 仓库内纯 Go 写出的真实 SQLite 3 检查点文件；
- SIGKILL 后的批次边界恢复与 `E_VER` 版本边界；
- 有界并行实时路径、断点恢复路径、一次性全量路径的逐项一致性。

## 快速运行

```bash
go version          # go1.22+
make verify         # 生成样本、跑测试外的完整验收（含真实 SIGKILL 子进程）
go test -count=1 ./...
```

常用命令：

```bash
go run ./cmd/wardrobe generate --dir samples --seed 20261127
go run ./cmd/wardrobe validate --project samples/project.json --truth samples/truth.json
go run ./cmd/wardrobe demo --checkpoint artifacts/checkpoint.sqlite
go run ./cmd/wardrobe incremental-demo
go run ./cmd/wardrobe resume --checkpoint artifacts/checkpoint.sqlite
go run ./cmd/wardrobe diff-demo
go run ./cmd/wardrobe verify
```

也可先构建：

```bash
make build
./artifacts/bin/wardrobe verify
```

## 作为 Go 组件集成

公共 facade 在根包 `wardrobe.go`：

```go
import wardrobe "wardrobekernel"

project, _ := wardrobe.LoadProject("samples/project.json")
assembly, _ := wardrobe.EvaluateAssembly(project, project.CandidateAssembly)
plan, _ := wardrobe.FullPlan(project)

store, _ := wardrobe.ResumeCheckpoint("checkpoint.sqlite",
    wardrobe.AlgorithmVersion, wardrobe.ConstraintVersion)
resumed, _ := wardrobe.ResumeAndContinue(ctx, project, store, wardrobe.SchedulerOptions{})
```

## 固定样本口径

固定种子：`20261127`。生成器在 `internal/samplegen/generator.go`，样本文件：

- `samples/project.json`：角色、10 个挂点、15 个部件、材质/子网格、互斥规则和预算；
- `samples/truth.json`：真值冲突、合法集合、批次、偏移、决策链和提交序列；
- `samples/invalid_schema.json`：覆盖重复挂点、悬空引用、区间越界/重叠、互斥悬空和非法预算；
- `samples/ambiguous_arb.json`：严格同优先级争用，用于核对 `E_ARB`。

首版样本的合法装配为 9 个部件，真值批次如下：

| 批次 | 部件顺序 | 顶点占用 | 索引占用 | 材质槽 |
|---:|---|---:|---:|---:|
| 1 | `face_base`, `chest_plate`, `hair_long` | 1060 | 1848 | 4 |
| 2 | `cape_red`, `pauldron_l`, `boot_pair`, `gauntlet_pair` | 1080 | 1710 | 3 |
| 3 | `waist_belt` | 140 | 210 | 1 |
| 4 | `greave_l`（挤压后延后重试） | 300 | 480 | 1 |

预算为每批 4 个材质槽、1100 顶点、2200 索引，且不允许跨皮肤组混批。

冲突明细覆盖：

- `ATTACH_POINT_CONTENTION`：例如 `hair_long` 对 `hair_short` 的挂点争用；
- `EXPLICIT_MUTEX`：`mutex_gauntlet_covers_ring` 使 `ring_left` 败给 `gauntlet_pair`；
- `SKIN_INCOMPATIBLE`：`sash_arcane` 的 `mage` 蒙皮组与 `human` 主装配不相容。

## 关键规则

- 裁决序固定为 `arbitration_priority DESC, id ASC`。
- 默认同优先级用部件 ID 字典序打破平局；`tie_policy=strict` 时返回 `E_ARB`。
- 合批按同一裁决序处理候选，依次尝试已有批次，再决定重映射、拆新批或延后。
- 相同 `(slot_id, material_id, material_version)` 共享批内材质槽；同逻辑槽但材质版本不同会记录争用并把候选材质重映射到新槽。
- 子网格顶点批内偏移为 `part_vertex_base + source_vertex_start`；索引目标区间紧凑追加，同时保留 `source_index_start` 和顶点重基址 `vertex_base`。
- 缓存键由算法版本、约束版本/参数哈希、部件内容哈希、材质槽版本和所有上游键共同哈希得到。
- 示例编辑 `waist_belt` 材质版本或顶点规模 `+24` 时，仅失效批次 `3` 及其 offset/commit 下游，批次 `1、2、4` 从持久化缓存复用；增量结果与空缓存全量结果逐项相等。

## SQLite 检查点

仓库没有引入第三方 SQLite 驱动。`internal/sqlitefile` 是一个最小纯 Go SQLite 3 KV 文件生成/读取器，写出的文件具有官方 `SQLite format 3` 文件头，包含真实的：

```sql
CREATE TABLE records(k TEXT, v TEXT)
```

每个已提交批次写入 `batch:000001` 等键，值中包含部件列表、子网格偏移、预算占用、依赖集合、部件哈希、缓存键、约束版本、算法版本和提交记录。提交由 `checkpoint.Store.Update` 的互斥事务串行化，通过同目录临时文件、`fsync`、`rename` 和目录 `fsync` 原子替换。

可用外部 SQLite 实现检查：

```bash
python3 - <<'PY'
import sqlite3
c=sqlite3.connect('artifacts/verify-final/kill-boundary.sqlite')
print(c.execute("select k from records where k like 'batch:%' order by k").fetchall())
PY
```

强制终止边界由 `wardrobe verify` 实际启动子进程验证：子进程在第 2 批提交后收到 SIGKILL，重启文件中恰有批次 1、2；resume 重算尾部并与全量路径一致。算法版本不匹配返回 `E_VER` 和受影响批次。

## 错误码

| 码 | 含义 | 验收入口 |
|---|---|---|
| `E_SCHEMA` | 输入结构/区间/引用/预算违规，含部件与字段路径 | `wardrobe validate --project samples/invalid_schema.json` |
| `E_ARB` | strict 策略下无法裁决同优先级争用，含双方和依据链 | `wardrobe demo --project samples/ambiguous_arb.json --truth=` |
| `E_DIFF` | 增量与全量结果不一致，含首个批次/部件/字段 | `wardrobe diff-demo` |
| `E_VER` | 检查点算法或约束版本不匹配，含受影响批次 | `wardrobe version-check --checkpoint <db> --algorithm-version wrong` |

## 目录结构

```text
wardrobe.go                    公共集成 facade
cmd/wardrobe/                  本地 demo / 诊断 / verify CLI
internal/model/                稳定领域模型与 JSON 契约
internal/xerr/                 E_SCHEMA/E_ARB/E_DIFF/E_VER
internal/schema/               加载与结构校验
internal/assembly/             顺序无关装配合法性纯函数
internal/batching/             确定性合批、偏移、仲裁
internal/depgraph/             依赖图、哈希键、精确传播、拓扑排序
internal/cache/                原子 JSON 持久化缓存
internal/scheduler/            有界并行准备、队列阻塞等待、失败隔离
internal/checkpoint/           SQLite 批次检查点与版本恢复
internal/sqlitefile/           纯 Go SQLite 3 文件读写
internal/compare/              逐项一致性比对与首差异定位
internal/pipeline/             三路径、增量和恢复编排
internal/samplegen/            固定种子样本与真值生成器
samples/                       已生成并入库的确定性样本
docs/VALIDATION.md             实际验证记录
docs/ACCEPTANCE.md             八条要求追踪矩阵
```
