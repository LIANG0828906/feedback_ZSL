# MaskQA：笔画方向与连通拓扑一致性工作台

单一 Go 模块 `maskqa`，同时提供可集成领域包、确定性样本生成器、纯 Go SQLite 检查点、CLI 验证入口和本地诊断 Web UI。样本、真值、三版本工程与断点库均写入本地目录，不依赖 Docker、外部账号或运行时网络。

## 快速开始

```bash
go version          # 已验证 go1.22.12；go.mod 声明 go 1.21
go test ./... -count=1

# 生成样本并执行 schema、次序、拓扑、三路径、增量失效全量验证
go run ./cmd/maskqa verify --data data --artifacts artifacts --seed 20261126

# 启动本地界面
go run ./cmd/maskqa serve --data data --addr 127.0.0.1:18080
```

打开 <http://127.0.0.1:18080/>。端口可通过 `--addr` 更改；`data/` 不可写时命令会直接返回错误。

纯 Go SQLite 驱动为 `modernc.org/sqlite`，以下构建已通过：

```bash
CGO_ENABLED=0 go build ./...
go vet ./...
```

## 已记录的实际结果

最近一次完整 CLI 报告写入：

- `artifacts/verification-report.md`
- `artifacts/verification-report.json`
- `data/verification-report.json`

实际执行 `go test ./... -count=1` 的结果为所有包通过，根包验收测试 `ok maskqa`。CLI 对 3 张样本（48×48、64×48、64×64，共 334 笔施工记录）的结论为 `PASS`：

- v1/v2/v3 装载笔画数与生成器一致。
- v2 的 0..255 权重往返最大残差为 16 bp（声明容差 21 bp）；v1/v3 为 0。
- realtime、checkpoint-resume、empty-cache-full-recompute 三路径逐像素覆盖、拓扑 JSON 与提交序列一致。
- 每张样本在第 7 个笔画后设置强制终止边界，恢复后提交序列完整。
- 权重编辑只使 `tone-a`、8 个相交分块和下游拓扑失效，另外 8 个分块从 SQLite 持久化结果复用。

强制终止边界也以真实进程退出码验证：

```text
$ ./artifacts/maskqa force-kill-demo --data data --project glyph-48 --after 7
forced boundary after 7 durable commits; exiting 137 without report
FORCE_EXIT_CODE=137

$ ./artifacts/maskqa resume-demo --data data --project glyph-48
resumed and verified glyph-48: commits=92 domains=2 final_mask_bytes=2304
```

## 命令

```bash
go run ./cmd/maskqa generate --data data --seed 20261126
go run ./cmd/maskqa verify   --data data --artifacts artifacts --parallel 4
go run ./cmd/maskqa serve    --data data --addr 127.0.0.1:18080

# 模拟提交 7 笔后立即 os.Exit(137)，不写退出报告
go run ./cmd/maskqa force-kill-demo --after 7
go run ./cmd/maskqa resume-demo
```

## 本地界面与 HTTP API

界面展示：

- 原始图标、候选掩码、逐笔彩色覆盖叠加、积算 alpha、最终掩码和真值。
- 描边环带分层与 36-bin Sobel 切向方向谱。
- 连通域面积、重心、外接框、孔洞、环宽和方向余弦对照。
- realtime/resume/full 三路径首个不一致像素或域编号。
- 增量编辑后的失效节点、原因、受影响分块和实际复用分块。
- 并行度、队列预算、排队等待和异常隔离事件。

主要 API：

- `GET /api/projects`
- `GET /api/project?id=glyph-48`
- `GET /api/verify?id=glyph-48`
- `GET /api/invalidation?id=glyph-48`
- `GET /img/sample?id=...`
- `GET /img/mask?id=...&kind=candidate|reference|final|coverage`
- `GET /img/strokes?id=...`
- `GET /img/ring?id=...`

## 三版本 Schema

| 版本 | 字段风格 | 坐标表达 | 权重刻度 | 硬度字段 |
|---|---|---|---|---|
| v1 | `schema`、`stroke_id`、`start_x/start_y/...` | 绝对像素 | `coverage_pct`：0..100 | `hardness_pct` |
| v2 | camelCase：`strokeId`、`strokeOrder` | `originX/originY + deltaX/deltaY` | `weight255`：0..255 | 无，装载为声明默认 8000 bp，并保留痕迹 |
| v3 | snake_case：`stroke_no`、`start_xy/end_xy` | 绝对整数点对 | `weight_bp`：0..10000 | `hardness_bp` |

装载器在 `internal/schemaio/schema.go`。未知版本返回 `E_SCHEMA`，缺字段返回带 JSON 路径的错误，例如 `$.strokes[0].coverage_pct`；源刻度越界返回带笔画 ID 和字段名的 `E_PARA`。

权重换算只使用整数四舍五入：

```text
scaled = (value * toScale + fromScale / 2) / fromScale
```

v1 往返容差 50 bp，v2 往返容差 21 bp。超差时 `E_ROUND` 错误包含 `field`、原值、返回值和残差。坐标的绝对/相对换算仅做整数加减，因此精确往返。

## 核心包结构

- `internal/model`：领域模型、错误码、掩码/分块和报告结构。
- `internal/schemaio`：三版本探测、字段映射、整数刻度换算和往返校验。
- `internal/sample`：固定 LCG 种子生成图标、候选缺陷、环域真值、施工笔画和工作区文件。
- `internal/engine`：纯整数 Bresenham 线、笔刷印章、硬度剖面、确定性栅格化和 alpha source-over。
- `internal/topology`：8 邻接前景域、4 邻接背景洪水填洞、重心/面积/外接框、Manhattan 深度环宽和 Sobel 方向谱。
- `internal/deps`：笔画/分量/次序/分块/拓扑依赖图、缓存键和编辑失效传播。
- `internal/checkpoint`：纯 Go SQLite、单连接串行事务、笔画级原子检查点和分块/拓扑持久化。
- `internal/runner`：有界 worker 池、队列预算、异常隔离、恢复、合成、三路径比对。
- `internal/verify`：CLI/CI 可复用报告。
- `internal/server`：无外部前端依赖的诊断 UI 和 PNG/JSON API。

## 关键一致性约定

- 合成次序固定为 `process_priority ASC, stroke_no ASC, id ASC`；不遍历 Go map 决定顺序，也不读取 worker 编号。
- 每个分块缓存键包含候选分块内容哈希、分块 ID、笔画 ID/内容哈希、次序版本、参数版本、算法版本和上游键。
- SQLite 同时使用 `SetMaxOpenConns(1)` 与进程内互斥；一个笔画的序列行和所有受影响分块分量在同一个 `SERIALIZABLE` 事务提交。
- 恢复查询强制匹配 `order_version`、`algorithm_version`、`params_version`。
- 三路径比对先逐字节比较覆盖 alpha，再用拓扑规范化结果定位首个域差异，最后比较提交序列。
- 拓扑容差集中在 `topology.DefaultTolerances()`：孔洞 0、环宽 0.75 px、重心 0.75 px、方向余弦 0.94。

环宽定义为前景像素到背景的 Manhattan 深度最大值的两倍；对生成的厚环样本，这是稳定可复现的有效环带厚度。方向谱使用 Sobel 梯度方向旋转 90° 得到笔画切向，再按 36 个 5° 无向 bin 以梯度强度加权。

## 数据目录

```text
data/
  manifest.json
  samples/<project>/
    image.png
    candidate.png
    reference.png
    project.internal.json
    truth.json
    project.v1.json
    project.v2.json
    project.v3.json
  checkpoints/*.db
```

删除 `data/` 后重新运行 `verify` 或 `generate` 即可完整重建。
