# SyncForge

面向多人协作对战游戏的本地实时状态同步演示与结果验证工具。项目从固定种子的确定性样本出发，同时验证：

- 权威 tick 推进与输入结算；
- 客户端本地预测、权威帧回滚和未确认输入重放；
- 输入响应延迟与权威校正帧数之间的可解释取舍；
- SQLite 串行原子检查点、身份校验、崩溃后窗口恢复；
- 实时推进、断点恢复、全量重放三条路径的逐 tick 一致性；
- 单线程与并行推进状态一致、提交序列稳定；
- 有界工作线程池、每线程队列和状态缓冲的阻塞记录；
- 本地回环 HTTP 演示页。

Go module 为 `syncforge`，Go 版本为 1.22，SQLite 驱动为纯 Go 的 `modernc.org/sqlite`，不需要 CGO。

## 快速开始

```bash
go mod tidy

# 生成固定种子样本（默认 seed=0x5eedc0de，4 名玩家，180 tick）
go run ./cmd/syncgen -out samples/demo.json

# 运行全部测试与竞态检测
go test ./...
go test -race ./...

# 三路径自动比对，输出 JSON 报告；失败时退出码非 0
go run ./cmd/syncserver -sample samples/demo.json -verify

# 启动本地演示页（仅 127.0.0.1）
go run ./cmd/syncserver \
  -sample samples/demo.json \
  -db artifacts/demo-checkpoints.db \
  -addr 127.0.0.1:8080
```

打开 `http://127.0.0.1:8080`。页面支持切换 `direct/low/medium/high` 时延档位、调整播放速度、全量重演、三路径比对，并按帧显示：

- 权威单位坐标、朝向、生命、资源、击杀和技能冷却；
- 每个客户端当前可见状态；
- 该帧采用 `predicted` 还是 `authority` 状态及取舍依据；
- 回滚次数、最大回滚深度和修正量曲线；
- 已提交窗口及不可改写标记；失败窗口可单独重试。

服务启动时可用 `-generate` 重新生成样本。依赖首次由 `go mod tidy` 拉取；缓存完成后可用 `GOPROXY=off go build ./...` 离线构建。

## 样本与真值

`Generate` 是本项目的本地确定性样本生成器，不读取外部账号、数据集或网络资源。样本包含：

- 玩家初始属性、技能定义、出生点和复活间隔；
- 每个 tick 的移动、技能、采集、道具意向；
- 客户端与观测时延档位；
- 由同一权威引擎和客户端模拟器生成的解析真值：
  - 关键 tick 的权威状态；
  - 每个客户端完整可见帧序列；
  - 对局锁定 tick、胜者和每名玩家的生命/资源/击杀结算。

加载入口：

- `ParseSample([]byte)`：严格 JSON 结构与字段存在性校验；
- `ValidateSample(*Sample)`：枚举、数值边界、tick 连续性等语义校验；
- `ValidateTruth(*Sample)`：重新模拟并逐字段核对样本中的解析真值。

校验失败统一返回 `*GameError`。字段缺失、重复玩家、意向流乱序/跳号、非法时延档位等返回 `E_TICK`，并携带首个冲突的 `tick`、`player_id` 和 `field`。

默认样本指纹：

```text
seed        1592639710 (0x5eedc0de)
algorithm   sync-v1
final tick  180
```

## 权威推进

权威引擎按固定阶段推进，不依赖 map 迭代顺序：

1. 对所有单位按玩家 ID 排序；
2. 并行处理各单位的冷却递减和复活；
3. 并行预计算移动/采集/道具动作；
4. 串行按规范顺序写入自修改动作；
5. 基于同一快照并行预计算技能合法性和伤害；
6. 串行应用技能消耗、冷却、治疗、伤害、死亡和击杀奖励；
7. 检查资源目标、淘汰或时间上限，锁定胜负。

所有状态量使用整型定点值。随机判定只由 `(对局种子, tick, 玩家, 用途)` 经哈希派生，不使用共享随机游标，因此单线程、并行、恢复和重放路径不会因调度不同产生随机差异。

技能前置校验包括存活、技能存在、冷却为零、资源足够、目标合法和距离范围。死亡单位保留在状态表中，达到 `respawn_at` 后以半血在出生点复活。

入口：

- `RunAuthoritative(sample, parallel, workers)`；
- `Engine.Step(...)` 单线程参考路径；
- `Engine.StepParallel(..., workers)` 并行路径。

## 预测、回滚与延迟

`SimulateClient` 模拟一个客户端渲染循环：

- 未收到权威帧时，基于最近权威状态和本地立即输入外推；
- 收到权威 tick 后回滚到该权威状态；
- 丢弃其后的推测状态，并重放尚未确认的本地输入；
- 对本地玩家逐字段比较预测与权威，记录 `x/y/health/resource/skill_cooldown/...` 修正项；
- 权威回滚帧标记为 `authority`，本地外推帧标记为 `predicted`，每帧都有 `basis`；
- 记录每帧修正量曲线、回滚深度、连续回滚次数；
- 超过 `max_rollback_depth` 或 `max_consecutive_rollbacks` 返回 `E_ROLLBACK`，带触发 tick、帧号和首个偏差字段；
- 预测帧输入响应超过 `max_input_latency_frames` 返回 `E_LAT`，带观测帧号、实测延迟和上限。

延迟与一致性冲突策略是“一致性优先，但受输入延迟保护”：

- 回滚校正窗口不超过 `max_correction_frames`；
- 同时不超过 `max_input_latency_frames - 1`，避免校正显示长期阻塞输入反馈；
- 实际采用的窗口和依据写入每帧 `basis`，不做静默截断。

## SQLite 检查点与恢复

默认每 20 tick 形成一个提交窗口；对局提前锁定时提交最终窗口。每个 `Checkpoint` 包含：

- 权威状态及完整已提交轨迹；
- 子随机源进度 `random_cursor`；
- 输入消费偏移 `input_offset`；
- 客户端已确认帧号；
- 回滚事件；
- 种子、算法版本、输入流指纹；
- 快照内容 SHA-256。

写入由 `Store` 互斥锁和 SQLite 串行事务保护，单连接、不交错：

- 同窗口相同哈希重试是幂等操作；
- 同窗口不同内容返回 `E_CONFLICT`，已提交窗口不可静默改写；
- 窗口必须按索引和 tick 递增提交，否则返回 `E_ORDER`；
- `RecordWindowFailure` 只标记单个失败窗口，`RetryWindow` 只允许重试失败窗口；
- `LoadLatest` 校验所有已存窗口的身份和哈希；种子、输入流指纹或算法版本任一不匹配时，整局快照全部删除并返回 `E_SNAPSHOT`；
- `RunRecovered` 在此情况下执行全量重放，不混用任何旧窗口。

强制终止后只能从最后一个已持久化窗口恢复。恢复结果包含该窗口之前已提交的轨迹，并从检查点继续推进，不会伪造未持久化的中间窗口。

## 三路径一致性

`VerifyThreePaths` 执行：

1. 实时路径：从 tick 0 推进到锁定；
2. 全量重放：全新引擎从 tick 0 重放；
3. 断点恢复：持久化到中间窗口，关闭并重新打开 SQLite，从最新检查点继续。

三条路径使用同一份样本、同一观测时延和 `sync-v1` 算法。自动比较覆盖：

- 每个 tick 的权威状态；
- 每个客户端每一渲染帧的可见状态、模式和 tick；
- 回滚次数；
- 最终结算。

首个差异返回 `E_DIVERGE`，同时包含 `tick`、`player_id`、`field`、`left`、`right` 实测值。权威比较也检查 `finished/winner/locked_tick`。

## HTTP API

服务只允许绑定 `127.0.0.1`、`localhost` 或 `::1`，否则拒绝启动。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 本地演示页面 |
| GET | `/api/sample` | 样本、单位、客户端、时延档位和真值摘要 |
| GET | `/api/demo?tier=low` | 实时推进 + 客户端模拟，写入/读取检查点 |
| POST | `/api/replay` | `{"tier":"low"}` 全量重演 |
| POST | `/api/verify` | 三路径逐 tick 比对 |
| GET | `/api/windows?tier=low` | 已提交/失败窗口列表 |
| POST | `/api/windows/{index}/retry?tier=low` | 只重试失败窗口 |

错误响应为：

```json
{
  "error": {
    "code": "E_TICK",
    "message": "...",
    "tick": 37,
    "player_id": "p2",
    "field": "intents.tick"
  }
}
```

## 并发与资源边界

- 全局最大并行度为 `MaxParallelism = 8`；
- `WorkerPool` 每个 worker 有独立有界事件队列；
- 批次结果缓冲受 `StateBufferSize` 限制；
- 队列或结果缓冲满时阻塞等待，分别记录 `queue_full:*`、`result_buffer_full:*` 和阻塞耗时；
- 不使用丢弃式 `select`；
- 独立 `BoundedBuffer[T]` 用于状态/渲染缓冲演示，满时记录 `state_buffer_full` 等原因；
- 检查点写入仍由存储层串行化，并行推进不会改变窗口提交顺序。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_TICK` | 字段缺失、tick 乱序/跳号、非法枚举或数值边界 |
| `E_ROLLBACK` | 连续回滚或回滚深度超限 |
| `E_LAT` | 预测输入响应延迟超限 |
| `E_DIVERGE` | 三路径或真值比较不一致 |
| `E_SNAPSHOT` | 快照身份/哈希/顺序无效，整局快照作废 |
| `E_ORDER` | 检查点窗口或 tick 未按序提交 |
| `E_CONFLICT` | 试图改写已提交窗口 |
| `E_BOUNDS` | 请求参数越界 |
| `E_UNSUPPORTED` | 不支持的方法或请求 |

## 测试覆盖

主要测试文件：

- `validate_test.go`：缺失字段、tick 乱序、重复玩家、非法时延、数值边界、真值往返；
- `engine_test.go`：串并行逐 tick 一致、技能消耗/冷却、伤害、死亡、复活、锁定结算；
- `client_test.go`：回滚修正字段、权威帧逐字段一致、`E_ROLLBACK`、`E_LAT`；
- `store_test.go`：重启恢复、身份不匹配整局失效、损坏快照失效、窗口顺序、不可改写、失败窗口隔离、串并行提交序列一致；
- `compare_test.go`：三路径一致，`E_DIVERGE` 定位 tick/玩家/字段和双侧值；
- `pool_test.go`：队列阻塞记录、无丢弃状态缓冲、panic 转错误；
- `http_test.go`：页面、实时 API、自动比对、非回环绑定拒绝。

本机实际验证结果（Go 1.22.12）：

```text
$ go test ./...
ok  syncforge
?   syncforge/cmd/syncgen    [no test files]
?   syncforge/cmd/syncserver [no test files]

$ go test -race ./...
ok  syncforge

$ go vet ./...
(无输出)

$ go run ./cmd/syncserver -sample samples/demo.json -verify
passed=true
interrupted_tick=80
recovered_from_tick=80

$ curl -sS http://127.0.0.1:18080/api/demo?tier=low
mode=realtime states=181 clients=4 windows=9 rollbacks=38

$ curl -sS -X POST -H 'Content-Type: application/json' \
    -d '{"tier":"low"}' http://127.0.0.1:18080/api/verify
passed=true
```

## 已知边界

- 演示样本使用 1:1 的 tick/render 映射；客户端模拟器支持不同 `tick_rate`/`render_fps` 比例，测试中覆盖了 60 FPS 的延迟超限场景。
- 检查点保存完整已提交轨迹，便于演示和逐 tick 比对；真实大型对局可改为状态增量或轨迹分表。
- SQLite 使用单连接串行写，优先保证确定性和无交错；吞吐不是本演示的优化目标。
- 本工具只监听回环地址，不访问外部目标。
