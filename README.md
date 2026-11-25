# Edge QoS Shaper

可在本地独立运行、可嵌入集成的出向 QoS 队列整形组件。实现以 Go 1.22 编写，
通过 SQLite 持久化提交点，不依赖容器、数据库服务、外部账号或运行时网络。

## 快速开始

前置条件：

- Linux / WSL Ubuntu
- Go 1.22+
- 依赖已 vendor，离线构建无需下载模块；SQLite 使用纯 Go 驱动

```bash
go version
go test -mod=vendor ./...
CGO_ENABLED=0 go test -mod=vendor ./...
go run -mod=vendor ./cmd/qosdemo demo \
  --seed 42 --out artifacts/demo
```

`demo` 会生成确定性工程、三版真值、逐时刻轨迹、增量失效报告、SQLite
检查点恢复对照和异常注入报告。

只运行异常注入：

```bash
go run -mod=vendor ./cmd/qosdemo inject \
  --seed 42 --out artifacts/demo/injection-work
```

## 三版 Schema

三版 JSON 描述同一份规范化内容，装载后 `RatePermyriad`、`CapacityBytes`、
通道顺序、报文流摘要完全一致。`qos.v1`、`qos.v2`、`qos.v3` 分别覆盖不同的
字段命名、参数表达、单位与时间基准。

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 参数表达 | 平坦 `shaping` 表 | `channels.profiles[].qos` 参数树 | 平坦 `shaping_table` |
| 通道字段 | `queue` | `id` / `channel_id` | `channel` |
| 速率 | `rate_permille` | `rate.permyriad` | `rate_value` + `rate_scale` |
| 桶容量/水位 | 包数 | 字节 | `burst.packets` / `burst.bytes` |
| 时间 | tick | 毫秒 | 毫秒 |
| 队列年龄 | `queue_age_ticks` | `queue_age_ms` | `queue_age.remaining_ms` |
| 权重 | 千分比 | 万分比 | `weight_scale=1000` |
| 真值的到期时刻 | 绝对 tick | 绝对毫秒 | 剩余毫秒 |
| 优先级提升 | 字段缺失，默认 0 | `priority_boost` 可存在 | 可选字段 |

规范化内部模型：

- 速率：`RatePermyriad`，单位是 `1/10000 byte/ms`。千分比乘 10，万分比不变。
- 容量/水位：整数字节。包数乘声明的 `NominalPacketBytes`。
- 时间：整数毫秒。tick 乘 `TickMS`；绝对时刻和剩余时长互转。
- 权重：`WeightPermyriad`。
- 优先级提升：缺失即为 0；DSCP 匹配时叠加到有效优先级。

装载器对别名同时出现且归一后不一致的情况返回 `E_FIELD_CONFLICT`，错误对象含
JSON 路径和冲突字段上下文。

更完整的字段映射见 `docs/schema-contract.md`。

## 整形语义

时间按 `TickMS` 离散推进：

1. 每通道使用整数分子补桶：
   `(RatePermyriad * TickMS + Remainder) / 10000`。
   余数保留到下一 tick，长时间运行不会累积截断误差。
2. 桶内令牌被钳制在 `[0, CapacityBytes]`，饱和时显式记录 `Saturated`。
3. 到期延迟报文先重放，新到报文后入队。
4. 通道内按报文优先级、就绪时刻、声明顺序排队。
5. 共享出口按有效优先级、权重 credit、通道声明顺序裁决。
   - 有效优先级 = 通道优先级 + 报文优先级 + 可选 DSCP 提升。
   - 每次裁决输出稳定 `WeightBasisID`。
6. 每次发出同时检查令牌和出口预算，二者任一不足都不会发出。

延后到期时间：

```text
deficit = packet_bytes - current_tokens
exact_ms = ceil(deficit * 10000 / rate_permyriad)
expire_at = now + ceil_to_quantum(exact_ms, queue_age_ms)
```

策略与原因编号：

| 编号 | 原因 | 含义 |
|---|---|---|
| 1 | `TOKEN_SHORTAGE` | 丢弃策略下桶内令牌不足 |
| 2 | `EGRESS_BUDGET` | 丢弃策略下共享出口预算不足 |
| 3 | `QUEUE_EXPIRED` | 计算到期超过通道最大队列年龄 |
| 4 | `QUEUE_DEPTH` | 通道未完成队列达到声明深度 |

延迟队列按 `(expire_at, packet_seq, packet_id)` 排序，同一报文以报文 ID 去重。
到期报文分配单调递增的 `ReplaySeq`，再次参与令牌消费；重放后仍不足时会重新
计算到期时刻，但队列中始终只有一条记录。

## 增量失效

`qos.Incremental` 在每次 step 后保存引擎快照。编辑速率、容量、权重或报文流时：

1. 取最早受影响的逻辑时刻，恢复其前一 step 的快照。
2. 前缀 step 原样复用，保留原 `CacheToken`。
3. 只重算从受影响 step 到结束的后缀。
4. 因共享出口调度会产生跨通道传导，闭包显式包含所有通道。
5. 报告 `StartStep/EndStep`、`ReusedSteps/RecomputedSteps`、根通道、闭包通道、
   传播边、变化 step 与重算后未变化 step。

## SQLite 检查点

检查点使用 `journal_mode=WAL`、`synchronous=FULL`。提交顺序固定为：

1. `checkpoint_channels` 通道令牌、余量、credit、ready 队列；
2. `checkpoint_delays` 延迟队列；
3. 最后更新 `checkpoint_meta` 全局 step/ms 与提交标记，然后 commit。

一笔事务未 commit 时，重启看不到任何中间状态。`checkpoint_meta.state_checksum`
同时覆盖工程摘要和快照事件；恢复仅接受工程 `Version + CanonicalDigest` 匹配的
检查点。版本不匹配返回 `E_CHECKPOINT_VERSION`，调用方可从 step 0 全量重算。

`qosdemo run` 每个完整 step 后提交，支持：

```bash
# 第 5 个提交点后 SIGKILL
go run -mod=vendor ./cmd/qosdemo run \
  --schema 1 --seed 42 --out artifacts/demo \
  --checkpoint artifacts/demo/run.db --crash-after 5

# 从最后完整提交点恢复
go run -mod=vendor ./cmd/qosdemo resume \
  --schema 1 --seed 42 --out artifacts/demo \
  --checkpoint artifacts/demo/run.db
```

`SIGINT`/`SIGTERM` 会在循环边界把当前已完成快照再次落盘后退出。

## 集成 API

```go
raw, _ := os.ReadFile("artifacts/demo/sample_v2.json")
project, err := qos.LoadProject(raw)
if err != nil {
    // qos.CodeOf(err) 返回稳定业务码
}

eng, err := qos.NewEngine(project)
if err != nil {
    return err
}
result, err := eng.Run(ctx)

trace, err := qos.ExecuteTrace(ctx, project)
inc, err := qos.NewIncremental(ctx, project)
newRate := int64(90000)
newTrace, invalidation, err := inc.Apply(ctx, qos.Edit{
    SourceID: "rate-edit", AtMS: 40, ChannelID: "gold",
    RatePermyriad: &newRate,
})

store, err := qos.OpenCheckpoint("qos.db")
err = store.Save(ctx, project, eng.Snapshot())
snapshot, err := store.Load(ctx, project)
resumed, status, err := qos.ResumeEngine(ctx, store, project)
```

## 确定性样本

`qos.GenerateSample(ctx, seed, dir)` 使用本地 xorshift64 生成器，产物：

- `sample_v1.json` / `sample_v2.json` / `sample_v3.json`
- `truth_v1.json` / `truth_v2.json` / `truth_v3.json`
- `stream.json`
- `critical_cases.json`
- `manifest.json`

固定种子 42 的基准结果：

- 34 个逻辑时刻
- 15 次发出
- 32 次延后事件（含重复延后）
- 14 次丢弃
- 3 次队列过期淘汰
- 三版工程摘要均为
  `58d10311c9e6b320f5501ef33e1e206e1ef91b753f4bc3a5c4cc73def66d5966`

## 业务错误码

| 错误码 | 场景 |
|---|---|
| `E_JSON` / `E_SCHEMA` | JSON 损坏或不支持的 schema |
| `E_FIELD_MISSING` / `E_FIELD_TYPE` | 必填字段缺失/类型错误 |
| `E_FIELD_CONFLICT` | 别名或单位表达归一后冲突 |
| `E_RATE_NEGATIVE` / `E_RATE_ZERO` | 负速率/defer 使用零速率 |
| `E_RATE_SCALE` / `E_RATE_OVERFLOW` | 非法刻度或位宽溢出 |
| `E_BUCKET_TOO_SMALL` | 桶容量小于所属报文 |
| `E_CAPACITY_NEGATIVE` / `E_WATERMARK_RANGE` | 容量或初始水位非法 |
| `E_OVERFLOW` | 声明位宽下整数溢出 |
| `E_TIME_ALIGNMENT` / `E_TIME_NEGATIVE` | 时间未对齐 tick 或为负 |
| `E_EXPIRY_ORDER` | 真值到期时刻倒序 |
| `E_DUPLICATE_PACKET` / `E_UNKNOWN_CHANNEL` | 报文重复或通道未知 |
| `E_THREAD_RANGE` / `E_QUEUE_BUDGET` | worker 或队列预算越界 |
| `E_WEIGHT_RANGE` / `E_PRIORITY_RANGE` | 权重/提升值越界 |
| `E_POLICY` / `E_DELAY_QUEUE` | 策略或延迟队列非法 |
| `E_DSCP_RANGE` | DSCP 不在 `[0,63]` |
| `E_CHECKPOINT_TRUNCATED` | SQLite 文件截断或不可读 |
| `E_CHECKPOINT_VERSION` | 工程版本/摘要不匹配 |
| `E_CHECKPOINT_CORRUPT` | 检查点结构或校验和损坏 |
| `E_CHECKPOINT_EMPTY` | 尚无已提交检查点 |
| `E_EDIT_TARGET` | 增量编辑目标不存在或顺序非法 |
| `E_HORIZON` | 到达 horizon 仍有未处理报文 |

所有错误均带可选 `path` 与 `context`，不会修改输入工程字节或已缓存快照。

## 验证入口

```bash
go test -mod=vendor ./... -count=1
go test -race -mod=vendor ./qos -count=1
go vet ./...
go run -mod=vendor ./cmd/qosdemo demo --seed 42 --out artifacts/demo
```

本次实际验证结果记录在 `docs/acceptance.md`，完整演示输出在
`artifacts/demo/demo_stdout.txt`。

## 目录

```text
cmd/qosdemo/      本地演示和检查点运行入口
qos/              可集成领域组件、schema、引擎、增量、SQLite、生成器
docs/             schema 契约与验收记录
artifacts/demo/   固定种子样本、真值、报告与演示输出
vendor/           离线构建所需依赖
```

## 范围说明

- 当前版本是单节点、确定性、整数时间的参考实现，不连接真实网关。
- 出口预算是每逻辑 tick 的字节预算。
- 共享调度闭包保守地覆盖所有通道，因为任一份额的发出都会影响同一出口预算。
- vendor 目录针对验收目标 Linux/amd64 精简；在其他平台构建时先执行
  `go mod vendor` 恢复对应平台文件。
