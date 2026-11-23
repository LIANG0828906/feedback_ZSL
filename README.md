# Room Reverb Workbench

面向游戏关卡的房间混响区划编辑工作台。项目从零实现，使用 Go 1.22、标准库、纯 Go 本地 SQLite 驱动和嵌入式的无外部素材 Web 界面。样本生成、求解、检查点、HTTP 服务与测试均可离线运行。

## 交付内容

- 固定种子确定性关卡生成器：矩形房间、共享墙体、门洞、声源、听者、解析真值。
- 关卡结构校验：字段缺失、房间重叠、墙体端点、门洞归属与宽度、采样点归属。
- Q16.16 定点连通求解：房间节点、门洞边、入口门洞状态、最短门洞串联路径。
- 定点混响估计：路径声程、房间链平均 T60、混响增益、早期反射密度。
- 五态房间状态机、冻结快照、进度统计、非法迁移保护。
- 显式依赖图与增量失效：只重算结果实际变化的房间，其余复用内存和 SQLite 缓存。
- SQLite 原子检查点：逐房间稳定顺序提交、内容哈希、参数版本、算法版本、整体失效、断点恢复。
- 实时预览 / 断点恢复 / 全量重算三路径逐项自动比对，返回首个差异的 `E_DIFF` 上下文。
- 工作线程池、最大并行度、每线程路径缓冲上限、连通图缓存预算与阻塞计数。
- Canvas 诊断工作台：区划、门洞、路径、状态矩阵、失效清单、逐听者曲线、参数/门洞/边界编辑、逐房间重试、导出。

## 快速开始

需要 Go 1.22 或以上。项目不依赖 CGO、Docker、外部数据库或网络素材。

```bash
go version
go build -o reverblab ./cmd/reverblab
./reverblab generate --seed 42 --out samples
./reverblab verify --level samples/level.json --truth samples/truth.json --edits samples/edits.json
./reverblab serve --level samples/level.json --truth samples/truth.json --db workbench.sqlite --addr 127.0.0.1:8080
```

Windows PowerShell:

```powershell
go version
go build -o reverblab.exe .\cmd\reverblab
.\reverblab.exe generate --seed 42 --out samples
.\reverblab.exe verify --level samples\level.json --truth samples\truth.json --edits samples\edits.json
.\reverblab.exe serve --level samples\level.json --truth samples\truth.json --db workbench.sqlite --addr 127.0.0.1:8080
```

浏览器打开 `http://127.0.0.1:8080`。工作台默认以 3 路并行、4096 房间路径缓冲上限、2 个连通图缓存许可运行；检查点写入始终串行。

## 样本与真值

`samples/` 由 `seed=42` 生成，当前样本包含：

- 6 个互不重叠、仅共享边界的矩形房间。
- 7 条共享墙体线段和 7 个有效门洞。
- 3 个声源、6 个听者，共 18 个声源-听者对，全部连通。
- `edits.json` 中 5 次编辑：房间参数、删除门洞、再次参数编辑、新增门洞、拖动共享边界。

`truth.json` 记录每个听者所在房间，以及每个声源-听者对的房间链、门洞链、路径长度、衰减时间和早期反射能量。真值路径由生成器内独立的简单路径枚举参考实现产出，不复用生产 Dijkstra 求解器。容差为：

- 路径：`0.02 mm`
- 衰减时间：`0.02 ms`
- 早期能量：`0.000001`

## 计算契约

所有几何和声学结果使用 Q16.16 定点数（缩放因子 `65536`）。距离使用整数平方和与整数平方根；路径长度与声学量不做浮点累加。

路径搜索：

- 房间是图节点，有效门洞是连接两侧房间的边。
- 搜索状态额外记录进入当前房间的门洞，从而正确计算“入口门洞中心 → 下一门洞中心”的路径距离。
- 直接跳过刚进入的同一门洞，避免无意义回退。
- 同一代价下按房间序列、门洞序列稳定择优。

给定房间链 `R` 与定点路径长度 `d`：

```text
travel_ms = floor(d * 1000 / 343000)
avg_t60_ms = sum(room.t60_ms for room in R) / len(R)
decay_ms = travel_ms + avg_t60_ms

avg_density = sum(room.early_density for room in R) / len(R)
avg_gain = sum(room.reverb_gain for room in R) / len(R)
early_energy = avg_density * avg_gain * 1000 / (1000 + d)
```

因此收紧房间链上任一 T60 不会增加衰减时间；路径变长不会增加早期能量。

## 状态与增量失效

每个房间只能按以下合法路径推进：

```text
pending -> connectivity -> parameters -> committed
                         \-> failed
parameters --------------\-> failed
failed -> pending (retry)
committed -> pending (仅显式 Invalidate，清空冻结快照)
```

非法迁移返回 `E_STATE`，原状态和冻结快照保持不变。提交时冻结房间参数、邻接门洞、路径、逐听者结果、内容哈希和版本。

依赖图显式记录：

- 房间参数 → 使用该房间的听者结果。
- 门洞 → 经过该门洞的听者结果。
- 听者 → 所属房间提交块。

编辑后对候选房间做依赖探针，仅当房间结果实际变化时才失效并重算。未受影响房间复用内存结果，并把当前版本写入 SQLite `room_cache`。失效清单包含房间标识、原因、失效听者和复用来源。

## SQLite 检查点

默认驱动为 `modernc.org/sqlite`，纯 Go，无 CGO。数据库启用单连接和互斥写：

```text
runs(run_id, level_hash, params_version, algorithm_version, precision, completed, updated_at)
snapshots(run_id, seq, room_id, input_hash, content_hash, params_version,
          algorithm_version, precision, payload_json, created_at)
room_cache(room_id, room_input_hash, params_version, algorithm_version,
           precision, content_hash, payload_json, created_at)
```

- 每个房间参数、门洞、路径摘要、听者结果、内容哈希、参数版本、算法版本在同一事务中写入。
- `seq` 必须连续；引擎按房间 ID 排序提交，写入不交错。
- 恢复时比较运行元数据、逐行内容哈希和版本。任一项不匹配即删除整组快照并全量重算，绝不混用旧房间结果。
- 取消/模拟终止只保留已完成事务的连续前缀；恢复从该前缀继续。

## HTTP API

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/api/state` | 关卡、状态、结果、依赖、预算、最近失效清单 |
| `POST` | `/api/params` | 编辑房间混响参数并增量重算 |
| `POST` | `/api/portal` | `action=add/remove` 编辑门洞 |
| `POST` | `/api/edge` | 移动房间共享边界并重算 |
| `POST` | `/api/retry` | 重试失败房间 |
| `POST` | `/api/reset` | 复位到样本初始关卡 |
| `POST` | `/api/compare` | 自动比对三路径 |
| `GET` | `/api/export` | 导出关卡、编辑序列、依赖、结果与失效记录 |
| `GET` | `/api/health` | 本地健康检查 |

错误响应统一为 `{"ok":false,"error":{"code":...,"room":...,"listener":...,"source":...,"field":...,"message":...}}`。

## 错误码

| 错误码 | 场景 |
|---|---|
| `E_DECODE` | 字段缺失、JSON 非法、版本/单位或数值有限性错误 |
| `E_ROOM` | 矩形重叠、门洞越界/宽度非正、采样点不在唯一房间 |
| `E_CONN` | 声源与听者不连通，带两侧标识 |
| `E_PARA` | 混响参数越界，带房间和字段名 |
| `E_STATE` | 非法状态迁移，状态保持不变 |
| `E_CHECKPOINT` | SQLite 或内容哈希/版本检查失败 |
| `E_DIFF` | 三路径比对不一致，带首个差异房间、听者、声源和字段 |
| `E_RESOURCE` | 路径缓冲等资源边界被突破 |
| `E_CANCELED` | 取消或模拟硬终止 |

## 测试与验证

```bash
go test ./...
go test -race ./...
go vet ./...
./reverblab verify --level samples/level.json --truth samples/truth.json --edits samples/edits.json --json artifacts/verification.json
```

实际验证结果见 `artifacts/verification.json` 和 `artifacts/test-summary.txt`。本次交付运行结果：

- `go test ./...`：全部通过。
- `go test -race ./...`：全部通过。
- `go vet ./...`：无报告。
- 固定样本：6 房间、18 对听者结果全部可达，真值逐项吻合。
- 三路径：提交序列均为 `r0,r1,r2,r3,r4,r5`；房间链、门洞链、路径、衰减、早期能量、统计完全一致。
- 5 次编辑的实时预览最终 6/6 房间复用依赖探针结果；断点恢复从 2 个 SQLite 快照继续并完成剩余房间。
- 人工篡改任一快照 `payload_json` 后，恢复检查整体作废全部快照，数据库快照计数归零。
- 并发测试验证输出与串行结果逐字节一致，并记录队列阻塞和缓存预算等待。

## 目录

```text
cmd/reverblab/           CLI：generate / verify / serve
internal/generator/      固定种子样本与解析真值
internal/validate/       JSON 字段检查与结构校验
internal/fixed/          Q16.16 算术与整数平方根
internal/solver/         连通图、定点最短路径、声学估计、并行预算
internal/fsm/            五态状态机与冻结提交
internal/editing/        参数、门洞、边界编辑
internal/checkpoint/     SQLite 事务快照与持久缓存
internal/engine/         全量、增量、恢复、比较
internal/verify/         三路径验证与真值核对
internal/web/            嵌入式 HTTP 服务与诊断界面
samples/                 seed=42 本地样本
artifacts/               验证输出
```
