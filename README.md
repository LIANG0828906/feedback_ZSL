# 开放世界地形材质内核

这是一个不依赖容器、外部服务或素材的 Go 1.22 地形材质首版内核。它包含确定性样本生成器、三版本材质工程加载/导出、整型单位换算、逐像素混合遮罩烘焙、精确依赖失效、增量重烘焙、并行资源边界以及 SQLite 版本化检查点。

## 交付内容

- `cmd/terrain`：生成样本、执行全量/恢复/增量三条路径并自动比对。
- `internal/gen`：按固定种子生成三版本工程、升级痕迹和逐块解析真值。
- `internal/schema`：v1/v2/v3 wire 格式、加载、升级、导出、单位换算和损失清单。
- `internal/fixed`：带溢出检测的 `int64` 定点算术。
- `internal/terrain`：确定性高度、坡度、噪声推导。
- `internal/bake`：权重求值、确定性归一、遮罩合成、依赖图、并行引擎、结果哈希与差异定位。
- `internal/store`：`database/sql` + `modernc.org/sqlite` 纯 Go SQLite 检查点。
- `internal/kerr`：稳定错误码与字段路径。

## 快速运行

```bash
go mod download
go test ./...
go run ./cmd/terrain -workdir work -workers 2 -buffer-budget-px 96
```

`-seed` 默认使用当前 UTC 日期 `YYYYMMDD`。也可以用 `-seed 20261125` 固定样本。

输出：

- `project_v1.json`
- `project_v2.json`
- `project_v3.json`
- `upgrade_traces.json`
- `ground_truth.json`
- `loss_report.json`
- `checkpoint.db`

`-stop-after N` 会先提交前 N 个分块后关闭检查点，下一次运行重新打开数据库并续跑缺失分块。`-1` 表示不提前停止。

## 三版本约定

| 项目 | v1 | v2 | v3 |
|---|---|---|---|
| 版本字段 | `format_version` | `formatVer` | `FormatVersion` |
| 分层表达 | 扁平常量表/斜率表 | 分段线性点列 | 分段线性点列 |
| 高度单位 | `cm` | `mm` | `m`，固定 6 位小数 |
| 坡度单位 | `rad`，内部微弧度整数 | `deg`，毫度整数 | `deg`，毫度整数 |
| 归一化 | 项目字段 | 项目字段 | 项目字段 |
| 噪声 | 无 | 无 | 可选 `Noise` |
| 坡度上限 | 无 | 项目级 `slopeCapDeg` | 逐层 `SlopeCapDeg` |

v3 的 `HeightUnit:"m"` 采用 1,000,000 个 wire 单位表示 1 米；加载时换算为内部毫米。`deg` 采用毫度表示，避免在曲线点列中使用浮点。

v1 的高度扁平常量表使用 10 cm（100 mm）bin，斜率表使用 5,000 微弧度 bin。无法完整表达的曲线点列、噪声和坡度上限会返回 `E_LOSSY`，同时在 `LossItem` 中记录路径、类型、降级动作和说明；导出字节仍可用于验收。

## 内部模型与精度

- 高度：毫米 `int64`
- 坡度：微弧度 `int64`
- 权重/遮罩：百万分之一 `int64`，即 `1_000_000 == 1.0`
- 归一化：全局最大余数法或逐层 anchor 归一后最大余数法，均保证权重和精确等于 `1_000_000`

`internal/fixed` 对加减乘除、`MulDiv` 和定点缩放使用 `math/big` 中间值并检测 `int64` 溢出；越界统一返回 `E_OVF` 和字段路径。

验收测试中的往返容差为每层权重最大 2 个微单位。v2 噪声与逐层坡度上限无法表达时按损失处理，不伪装为无损往返。

## 烘焙与真值

真值完全由生成器内置的解析构造产生：

1. 由种子和绝对像素坐标推导高度、坡度与可选噪声。
2. 对每层曲线求值并应用坡度上限屏蔽。
3. 按项目归一化约定做最大余数归一。
4. 按 `order` 合成 tint 加权的混合遮罩采样值，并取首个最大权重层为主导层。

`ground_truth.json` 为每个分块保存 4 个指定像素的高度、坡度、各层归一权重、主导层和遮罩采样值。测试直接比较这些解析真值与 `BakePixel`/并行烘焙结果。

同一工程在 1/2/3/4 个工作线程、16/32/96 像素预算、预览路径、SQLite 恢复路径和一次性全量路径下产物一致。提交序列固定为分块编号升序，不随完成顺序变化。

## 依赖图与增量失效

依赖图节点包括：

- 每层曲线控制点
- 每层派生参数（噪声、坡度上限、tint 等）
- 归一化约定
- 分块权重节点
- 分块遮罩合成节点

编辑某层时，`Graph.Propagate` 在该层旧/新定义下逐像素比较派生因子，只有实际产生权重差的分块才失效；归一化变化会使所有分块的合成节点失效。`Invalidation` 输出分块编号、层标识和原因，并说明下游遮罩合成阶段。

增量路径复用未失效结果，只重烘焙失效分块。`CompareResults` 自动定位首个不一致的分块、像素坐标、字段和值，失败文本包含 `E_DIFF`。

## SQLite 检查点

实现使用纯 Go 驱动 `modernc.org/sqlite`，DSN 启用 WAL、`busy_timeout` 和 immediate transaction。

`chunks` 表：

```sql
chunk_id INTEGER PRIMARY KEY,
weight_summary TEXT NOT NULL,
blend_summary TEXT NOT NULL,
dependencies TEXT NOT NULL,
content_hash TEXT NOT NULL,
norm_version TEXT NOT NULL,
param_version TEXT NOT NULL,
algorithm_version TEXT NOT NULL,
commit_seq INTEGER NOT NULL,
dominants BLOB NOT NULL,
blends BLOB NOT NULL,
weights BLOB NOT NULL
```

`weights` 是完整权重行的 varint 编码，用于恢复时直接复用而不重烘焙；摘要和内容哈希分别覆盖权重、遮罩以及“权重+主导层+遮罩”。

检查点语义：

- 每次提交一个分块使用独立事务，写锁串行化，提交顺序稳定。
- 恢复时验证全局指纹、每块归一化/参数/算法版本、依赖集合、摘要和内容哈希。
- 任一块损坏或不匹配会删除整库快照并返回空恢复集，随后全量重烘焙，禁止混用旧块。
- 增量编辑先原子删除受影响块，再提交新结果；未受影响块保留原检查点记录。
- 全局指纹排除可编辑曲线/噪声/坡度上限，但包含工程 ID、种子、尺寸、归一化、层 ID/顺序，因此全局变化仍会整体作废。

查看表结构：

```bash
sqlite3 work/checkpoint.db '.schema chunks'
sqlite3 -header -column work/checkpoint.db \
  'SELECT chunk_id, commit_seq, param_version FROM chunks ORDER BY commit_seq;'
```

本次环境无免密 sudo，因此从 SQLite 官方 3.46.1 amalgamation 编译到 `~/.local/bin/sqlite3`，仅作为查看工具；内核本身不依赖该 CLI。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_VER` | 未知/冲突的格式版本、非法升级目标 |
| `E_SCHEMA` | 必填字段缺失、JSON 形状、重复层、非法曲线等 |
| `E_UNIT` | 无法识别或与版本不匹配的单位 |
| `E_OVF` | 定点换算或算术溢出 |
| `E_LOSSY` | 目标版本无法无损表达，返回损失清单 |
| `E_DIFF` | 增量/全量/恢复结果或提交序列不一致 |

错误文本包含字段路径，例如 `E_UNIT at slopeUnit: v2 expects deg, got "rad"`。

## 并发与异常

- `MaxWorkers` 限制工作线程数。
- `BufferBudgetPx` 是全局在途像素预算，并按工作线程数推导条带上限。
- 申请整条带预算时若空间不足，记录 `pixel_buffer_budget_exhausted` 并阻塞等待，不会静默丢块。
- panic 被限制在单分块内，标记 `ChunkFailure`；其他块和已提交结果不受污染。
- 再次运行失败分块会重试，成功后与全量结果逐像素一致。
- `Pause`/`Resume` 可阻塞/恢复工作线程；`-stop-after` 用于检查点暂停与进程重启续跑。

## 实际验证结果

本机环境：`go version go1.22.12 linux/amd64`，`sqlite3 3.46.1`。

```text
$ go test ./... -count=1 -timeout=120s
ok terrainkernel/cmd/terrain
ok terrainkernel/internal/bake
ok terrainkernel/internal/fixed
ok terrainkernel/internal/schema
ok terrainkernel/internal/store
ok terrainkernel/internal/terrain

$ go test -race ./... -count=1 -timeout=180s
ok 所有包

$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
$ go vet ./...
（均通过）
```

实际 CLI 样本（`-workers 3 -buffer-budget-px 32 -stop-after 2`）：

```text
[gen] project=terrain-20261125 chunks=4 chunkSize=8 layers=4 norm=global
[run1] committed chunks: [0 1]
[run2] resumed chunks: [0 1 2 3] reused: [0 1]
[verify] preview == full bake, pixel-exact
[incremental] invalidated entries: 4
[verify] incremental rebake == full rebake on edited project, pixel-exact
[done] samples, checkpoint and ground truth written under ...
```

检查点实际查询显示 `chunk_id=0..3`、`commit_seq=0..3`，每块有独立参数版本和内容哈希；损坏权重 BLOB、算法版本或依赖集合的测试均会清空整个快照。

固定 `-seed 20261125` 时，`project_v1.json`、`project_v2.json`、`project_v3.json`、`ground_truth.json`、`upgrade_traces.json` 与随任务提供的本地基准逐字节一致；`PRAGMA integrity_check` 返回 `ok`。

## Windows 控制台

代码不使用 Unix 专属 API，SQLite 驱动为纯 Go。Windows 上执行：

```bat
set CGO_ENABLED=0
go test ./...
go run ./cmd/terrain -workdir work -workers 2 -buffer-budget-px 96
```

当前验证环境为 Linux，未在真实 Windows 控制台执行；已额外使用 `GOOS=windows` 编译检查。
