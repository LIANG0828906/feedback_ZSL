# QualityBench

本地确定性游戏截图画质劣化基准。项目从空工作区实现，包含样本生成、真值校验、可配置劣化链、分块执行、失效传播、内容哈希缓存、SQLite 检查点、断点恢复、三路径逐像素比对、独立验证器和 `net/http` 诊断页。

运行时不访问网络，不需要 Docker，不依赖系统 SQLite；SQLite 使用纯 Go 驱动 `modernc.org/sqlite`。

## 快速开始

要求 Go 1.22+，并确保 `go`、`gofmt` 在 PATH 中。

```powershell
go mod download
go test ./...
go run ./cmd/qbench accept --out artifacts/acceptance --seed 424242 --workers 4
```

验收会生成 `artifacts/acceptance/report.json` 与 `summary.txt`。也可以手动逐步运行：

```powershell
go run ./cmd/qbench generate --out samples --seed 424242
go run ./cmd/qbench validate --project samples/project.json --truth samples/truth.json
go run ./cmd/qbench run --project samples/project.json --truth samples/truth.json `
  --db artifacts/qbench.db --run-id live --mode live --workers 4
go run ./cmd/qbench run --project samples/project.json --truth samples/truth.json `
  --db artifacts/qbench.db --run-id full --mode full --workers 4
go run ./cmd/qbench compare --db artifacts/qbench.db --run-a live --run-b full
go run ./cmd/qbench verify --db artifacts/qbench.db --run-id full --truth samples/truth.json
```

诊断页：

```powershell
go run ./cmd/qbench serve --project artifacts/acceptance/samples/project.json `
  --truth artifacts/acceptance/samples/truth.json --addr 127.0.0.1:8787
```

页面提供基准图/逐步劣化图、三项指标热力图、首贡献归因、失效传播、缓存统计、三路径比对、暂停/继续/取消及逐块重试。

## 默认样本

`generate` 默认生成：

- 3 张 `192x128x3` 基准图，固定种子 `424242`
- 渐变背景、矩形/椭圆/三角形/菱形、逐帧三位水印、低对比度纹理
- 每图 5 步有序劣化链
- `16x16` 分块，共 `12x8=96` 块/图
- 每块三项指标期望区间和归因真值、每图 8 个采样点期望像素
- 3 条确定性编辑脚本

算子强度范围：

| 类型 | 强度范围 | 作用 |
| --- | --- | --- |
| `noise` | `[0,1]` | 坐标哈希驱动的确定性噪声 |
| `blur` | `[0,1]` | 半径 1–3 的盒式模糊，与强度混合 |
| `blockiness` | `[0,1]` | 全局对齐 8×8 块的均值化 |
| `chroma_shift` | `[-1,1]` | YCbCr 色度偏移 |
| `overexpose` | `[0,1]` | 局部提亮、软截断与轻度去饱和 |

每一步只写声明区域，区域外逐像素保持不变。模糊和块效应在求值时使用全图上下文，避免分块裁剪改变边界与全局对齐语义。

## 命令

| 命令 | 说明 |
| --- | --- |
| `generate` | 生成基准 PNG、工程、算链、编辑序列与真值 |
| `validate` | 校验 schema、范围、分块整除性、水印/图元冲突、采样点和基准哈希 |
| `run` | 执行 `live`、`resume` 或 `full` 路径并写入 SQLite |
| `compare` | 按步骤和块坐标比较两个 run 的持久化像素 |
| `verify` | 仅读 SQLite 分块产物与真值，重算指标和归因 |
| `serve` | 启动本地诊断页 |
| `accept` | 运行完整验收并产出报告 |

`run` 支持 `--stop-after N` 制造断点、`--persistent-cache`、`--workers`、`--thread-buffer-bytes`、`--buffer-bytes` 和 `--delay-ms`。

## 错误码

| 错误码 | 场景 |
| --- | --- |
| `E_SCHEMA` | 必填字段缺失或 JSON 结构不合法，带字段路径 |
| `E_RANGE` | 坐标、图元框、水印、采样点或配置越界 |
| `E_PARA` | 算子类型/强度/区域非法，带步骤序号和字段名 |
| `E_STAGE` | 单步执行失败，带步骤序号和分块坐标 |
| `E_BLOCK` | 分块尺寸不整除画面，`details.candidates` 给可整除候选 |
| `E_STATE` | 非法状态迁移；失败时保持原状态 |
| `E_DIFF` | 三路径/真值比较不一致，定位首个步骤、块和字段 |
| `E_CANCELED` | 暂停后取消或外部上下文取消 |

## 一致性设计

- 缓存键：`SHA256(algorithm_version, step_index, input_pixel_hash, canonical_step_params, upstream_cache_key)`。
- 输入哈希包含该步真正可能读取的支撑域：模糊按半径扩张，块效应按 8×8 对齐范围扩张。
- 编辑后从最早变化步骤开始维护脏块集合；仅当新区域/旧区域或上游脏块与当前作用域相交时继续传播。
- 未受影响块直接保留原输出、缓存键和质量评估；受影响块可命中历史版本缓存或重算。
- SQLite 每行保存像素、输入/输出内容哈希、输出摘要、缓存键、链指纹、算法版本、状态和全局单调 `commit_seq`。
- 运行完成后按块把 `step=0..N` 的全部缓存键、输入/输出哈希和摘要写入一条 `block_manifests` 事务记录。
- 每次写入使用独立事务并由存储层互斥串行化；执行器按块坐标稳定顺序提交。
- 恢复时校验 run 元数据、链指纹、算法版本、每个块的内容哈希和摘要；任一不匹配即删除该 run 的全部检查点并全量重算。
- `full` 不读缓存；`live` 读内存/SQLite 内容缓存；`resume` 从已完成块继续。

详细公式、状态迁移和表结构见 `docs/DESIGN.md`。

## 代码结构

```text
cmd/qbench/                  CLI
internal/generator/          确定性基准图、算链、真值参考构造
internal/dataset/            带路径诊断的 JSON 加载校验与 PNG 基准加载
internal/model/              工程/真值/编辑模型及校验
internal/ops/                五种逐步劣化算子
internal/quality/            分块指标与首贡献归因
internal/pipeline/           分块执行、缓存、编辑/回滚、状态机
internal/checkpoint/         持久化接口与摘要定义
internal/store/              SQLite 原子检查点实现
internal/compare/            三路径逐像素比较
internal/verify/             独立指标/归因验证器
internal/server/             诊断页
internal/accept/             一键验收
```

## 实测结果

当前环境：Go `1.22.12 linux/amd64`，`GOOS=windows GOARCH=amd64 go build ./...` 通过。

```text
go test ./...                                      PASS
go test -race ./internal/pipeline ./internal/server ./internal/store
                                                   PASS
go vet ./...                                       PASS
```

`artifacts/acceptance/summary.txt` 是 `go run ./cmd/qbench accept --seed 424242 --workers 4` 的真实输出：

- 24/24 验收检查通过。
- `live`、`resume`、`full` 三组两两比较均为 `1728` 个块、`0` 差异。
- 独立验证对三条路径各检查 `288` 个块：指标 `288/288`、归因 `288/288`、采样点 `24/24`。
- 编辑验收：`566` 条失效记录，`316` 个块重算，`644` 个块复用，`333` 次缓存命中，编辑后与直接全量重算逐像素一致。
- 回滚后最终哈希与初始哈希一致；重复回滚/重放仍与直接构造同链结果一致。
- 资源探针：4 workers，`21` 次像素缓冲等待、`268` 次队列等待，最长等待约 `79.648 ms`，无静默丢弃。
- SQLite 中 `1344` 条块级 manifest，提交序列全局唯一。
- 异常链实测：
  - 缺失字段：`E_SCHEMA path=schema_version`
  - 图元越界：`E_RANGE path=images[0].shapes[0].bbox`
  - 强度越界：`E_PARA step=1 field=strength`
  - 分块不整除：`E_BLOCK`，候选 `1,2,4,8,16,32,64`
  - 非法状态：`E_STATE` 且原状态保持
  - 注入步骤失败：`E_STAGE step=2 block=2@32,0`
  - 篡改像素：`E_DIFF step=2 field=pixels block=14@32,16`
  - 检查点哈希损坏：整体作废并自动全量重算成功

完整验收证据在：

- `artifacts/acceptance/report.json`
- `artifacts/acceptance/summary.txt`
- `artifacts/acceptance/acceptance.db`
- `artifacts/acceptance/samples/`

