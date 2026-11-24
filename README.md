# 房间混响区划增量声学求解器

一个可本地运行的首版实现：从固定种子确定性生成平面关卡，求解门洞串联房间链、定点衰减曲线与遮挡后声压级，支持精确失效传播、并发合成、SQLite 原子检查点、断点恢复和本地诊断页面。

## 快速运行

环境：Go 1.22+，纯 Go SQLite 驱动 `modernc.org/sqlite v1.34.5`，无需 CGO、Docker、音频文件、账号或网络服务。

```bash
make build
make test
make validation
```

`make validation` 会依次执行：

1. 固定种子生成样本与解析真值；
2. 从空缓存全量求解并写 SQLite；
3. 应用参数、门洞、拖动边界编辑；
4. 在最终编辑中模拟只提交 1 个房间后中断；
5. 新进程恢复已提交房间并完成剩余房间；
6. 独立比较“解析真值 / 空缓存全量 / 增量缓存”三路径；
7. 启动 `127.0.0.1` 诊断页并请求页面与状态 API；
8. 交叉编译 Windows `amd64` 可执行文件并运行全部测试。

手工命令：

```bash
export CGO_ENABLED=0 GOTOOLCHAIN=local
go build -o bin/acoustic ./cmd/acoustic

./bin/acoustic generate --out data/validation/sample.json --edits data/validation/edits.json --seed 42
./bin/acoustic validate --input data/validation/sample.json
./bin/acoustic solve --input data/validation/sample.json --db data/validation/acoustic.db --out artifacts/validation/01-solve.json

./bin/acoustic edit \
  --input data/validation/sample.json \
  --edits data/validation/edits.json \
  --db data/validation/acoustic.db \
  --project-out data/validation/edited.json \
  --out artifacts/validation/02-edit-interrupted.json \
  --crash-after 1

./bin/acoustic resume --input data/validation/edited.json --db data/validation/acoustic.db --out artifacts/validation/03-resume.json
./bin/acoustic verify --base data/validation/sample.json --edits data/validation/edits.json --db data/validation/acoustic.db --out artifacts/validation/04-verify.json

./bin/acoustic serve --input data/validation/edited.json --db data/validation/acoustic.db --project-out data/validation/served.json --addr 127.0.0.1:18123
```

## 目录

```text
cmd/acoustic/              CLI：generate/validate/solve/edit/resume/verify/serve
internal/model/            数据契约、错误码、结构/几何校验、状态枚举
internal/generator/        固定种子样本、独立解析真值枚举
internal/geom/             门洞图、折线入室校验、门洞序列几何
internal/acoustics/        定点衰减/声压级公式与逐步截断记录
internal/solver/           Dijkstra 链、状态机、失效、并发池、调度引擎
internal/store/            SQLite 单写者检查点、历史日志、元数据
internal/verify/           三路径比较、容差扫描、E_DIFF 定位
internal/diag/             本地诊断页面与编辑/重试端点
scripts/run_validation.sh  完整可重复验收流程
```

## 样本设计

固定种子生成 6 个矩形房间：

- 主分区 `R1–R4` 以共享墙段和门洞 `D1/D3/D4` 构成连通网络；
- 独立声学分区 `R5–R6` 共享墙段 `W56`，初始只有门洞 `D5`；
- 每个房间有 1 个声源和 2 个听者采样点，共 6 个声源、12 个听者、72 个声源-听者对；
- 跨分区无门洞，因此对应真值明确标记为不可达，求解器与全量路径也一致返回不可达。

内置增量编辑序列：

1. 修改 `R6` 混响参数；
2. 在 `W56` 新增门洞 `D6`；
3. 将 `R5/R6` 共享边界从 `x=800` 拖到 `x=840`；
4. 删除旧门洞 `D5`。

独立分区使验收能明确观察到：`R5/R6` 的局部编辑不影响主分区 `R1–R4`，后者必须复用缓存，而不是随全量求解一起重算。

## 几何与门洞链

房间、墙、门洞和采样点均使用整型坐标。门洞表示为所属墙段上的区间 `[start, start+width]`，宽度必须为正且完全落在墙段内。

求解器以 `(当前房间, 上一个门洞)` 为 Dijkstra 状态，以门洞中心作为折点，精确累加：

```text
source -> door_1.center -> ... -> door_n.center -> listener
```

每一步使用 `sqrt(dx²+dy²)` 的定点开方并保存舍入余数。相邻房间必须由真实门洞连接；每段折线必须落在对应矩形内部。真值侧不调用求解器，而是对全部门洞的非重复序列做独立枚举，再按“路径长度、房间链、门洞链”字典序取最短。

路径长度超出声明容差返回 `E_CHAIN`，并给出声源、听者、字段和首个分歧房间。房间链本身必须逐项一致。

## 定点声学公式

所有声学值使用 `Scale = 1,000,000` 的定点整数：

- 路径长度为微坐标单位；
- 时间参数以微秒表示；
- 声压级与衰减值为微 dB。

对声源-听者对：

```text
gainDB     = average(20 * log10(room.reverb_gain))
densityDB  = average(10 * log10(1 + room.early_density))
distanceDB = 20 * log10(max(path, 1m) / 1m)
occlusion  = 1.5 dB * crossed_door_count
L0         = 94 + gainDB + densityDB - distanceDB - occlusion
L(t)       = L0 - 60 * t / RT60(listener_room)
expectedSPL = L0
```

`RT60` 取听者所在房间；增益与早期反射密度沿房间链取平均，因此链上房间参数变化会传播到下游听者。对数只在进入定点域时截断一次，之后所有加法、平均和衰减除法均为整数定点运算。

每个听者结果保存 `Truncations`，包括：

- 每段路径开方的平方余数；
- 每次 `log10` 转换的截断余数；
- 每次曲线采样衰减除法的截断余数。

容差扫描从严格到宽松计算失败听者数；失败数必须单调不增。本次样本在 `0 / 1 / 10 / 100 / 1000 / 5000 / 1000000` 微 dB 容差下均为 0 个失败。

## 状态机与提交边界

房间状态：

```text
pending -> analyzing_connectivity -> solving_parameters -> committed
    |                 |                       |
    +-----------------+-----------------------+-> failed -> pending
```

非法普通迁移返回 `E_STATE`，状态保持不变。`committed` 结果快照本身不被编辑；结构性编辑开启新修订，仅将被依赖变更实际影响的房间通过专用 `InvalidateForRevision` 重置为 `pending`。旧快照要么原样复用，要么由新快照原子替换，不会原地混合。

进度始终显示为“已提交房间数 / 总房间数”。

## 精确失效传播

已提交房间检查点显式保存：

- 混响参数快照；
- 实际使用的门洞快照；
- 声源与听者快照；
- 每个声源-听者对的最短房间链、门洞链、分段长度；
- 曲线、声压级、遮挡值和截断记录。

编辑后重新计算连通摘要，但只为以下情况标记失效：

- 房间链、门洞链或路径长度实际变化；
- 链上房间参数变化；
- 声源/听者/已使用门洞依赖变化；
- 对者集合变化。

其余房间保持 `committed` 并复用内存与 SQLite 缓存。失效报告列出房间编号、原因、受影响对、是否复用及前后状态。

本次样本的实测失效：

```text
set_room_param    invalidated=[R5 R6]  reused=[R1 R2 R3 R4]
add_door          invalidated=[R5]     reused=[R1 R2 R3 R4 R6]
drag_boundary     invalidated=[R5 R6]  reused=[R1 R2 R3 R4]
remove_door       invalidated=[R5 R6]  reused=[R1 R2 R3 R4]
```

## SQLite 检查点与恢复

每个房间一次提交，使用单写者 goroutine、`BEGIN/COMMIT` 事务和稳定房间序：

```text
room_checkpoints(room_id PK, revision, payload, content_hash,
                 parameter_version, algorithm_version, commit_order, ...)
checkpoint_log(seq PK, room_id, content_hash, commit_order, ...)
app_meta(key PK, value)
```

内容哈希覆盖参数、门洞、声源/听者、连通摘要和全部听者结果。启动时：

- 任一内容哈希、参数版本或算法版本不匹配：整批检查点事务性清空，全量重算；
- 检查点完整但依赖已被合法编辑改变：只将该房间标为 stale，其余房间继续复用；
- 中断后只存在部分新检查点：新行复用，旧修订的 stale 行重新计算，禁止部分混用。

本次强制中断在第 5/6 个房间后发生。重启报告：

```text
rows=6 reused=[R1 R2 R3 R4 R5] stale=[R6] discarded=false
resume recomputed=[R6] reused=[R1 R2 R3 R4 R5]
```

## 并发与资源边界

- 最大工作线程数由 `--workers` 限制（默认 4）；
- 采样缓冲池数量由 `--buffers` 限制（默认 2）；
- 每个缓冲最大采样数由 `--buffer-samples` 限制（默认 64）；
- 曲线样本超过单缓冲时按块合成，不丢弃数据；
- 缓冲/工作线程耗尽时任务阻塞排队，记录作业编号、原因、微秒等待值和缓冲样本数；
- 听者曲线可并行，检查点写入始终由单写者队列串行完成，房间顺序稳定。

## 本地诊断页面

`serve` 只允许绑定回环地址，例如 `127.0.0.1:18123`。页面包含：

- 关卡俯视 SVG：房间、墙、门洞、门洞连通虚线、声源与听者；
- 逐听者多声源衰减曲线；
- 房间状态矩阵、参数、门洞快照和监听对；
- 编辑后的失效传播清单；
- 工作线程/缓冲排队事件；
- SQLite 检查点写入历史。

页面提供参数编辑、增加/删除门洞、拖动边界、增量求解、全量重置和 failed 房间逐房间重试。编辑成功后会保存项目文件并自动触发增量重算。

主要 API：

```text
GET  /                  HTML 诊断页
GET  /api/state         当前项目、结果、恢复与检查点状态
POST /api/solve         增量求解
POST /api/full          清空缓存并全量求解
POST /api/edit          参数/门洞/边界编辑
POST /api/retry?room=R6 逐房间重试
```

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_SCHEMA` | 字段缺失、解码失败、真值契约不完整 |
| `E_GEOM` | 房间重叠、墙/门洞/采样点几何冲突 |
| `E_CHAIN` | 房间链、可达性或路径长度与真值不符 |
| `E_STATE` | 非法房间状态迁移 |
| `E_DIFF` | 三路径逐字段差异，含房间/听者/字段定位 |
| `E_RESOURCE` | 并发缓冲/工作池资源错误 |
| `E_CHECKPOINT` | 检查点损坏、版本不匹配或恢复错误 |
| `E_INTERRUPTED` | 按检查点提交上限模拟中断 |

## 实际验证结果

环境：

```text
Go 1.22.12 linux/amd64
CGO_ENABLED=0
modernc.org/sqlite v1.34.5
```

`make validation` 实际结果：

```text
base solve:        6/6 committed, 72 listener jobs
edit invalidation: R1-R4 reused on every R5/R6-local edit
interrupted:       5/6 committed
restore:           reused R1-R5, stale R6
resume:            recomputed R6, reused R1-R5, final 6/6
three paths:       truth = full = incremental
digest:            0610721824097709e54ef2c9a66af9a5d4606519d14d8fb0e2ccebd08e7649b3
tolerance sweep:   monotonic, 0 failures at all tested tolerances
HTTP smoke:        GET / and GET /api/state returned 200
Windows build:     GOOS=windows GOARCH=amd64 CGO_ENABLED=0, result=ok
go test ./...:     all packages passed
go vet ./...:      passed
```

保存的产物位于 `artifacts/validation/`：

- `01-solve.json`
- `02-edit-interrupted.json`
- `03-resume.json`
- `04-verify.json`
- `05-diagnostic.html`
- `05-state.json`
- `05-state-after-http-edit.json`
- `06-windows-build.txt`
- `07-tests.txt`
- `08-vet.txt`

## 设计取舍

- 允许矩形在角点相接（面积重叠为 0）；有正面积重叠时返回 `E_GEOM`。
- 提交序列使用稳定房间 ID 序，因此“全量”和“增量”即使实际重算批次不同，逻辑提交序列仍可逐项比较。
- 参数版本指参数契约/模式版本；房间参数改动本身由内容哈希和依赖快照追踪，以便增量求解。
- 当前折线模型以门洞中心为折点，适用于矩形房间的区划级求解；真实声学工程可替换门洞中心采样与传播模型，而不改状态/失效/检查点边界。
