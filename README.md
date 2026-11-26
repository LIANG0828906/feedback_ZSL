# SLI / SLO 错误预算内核

这是一个纯本地、确定性的 SLI/SLO 计算内核，用于在接入真实监控与告警平台前固化计算口径。输入由固定种子生成器产出，不依赖外部监控、账号或网络资源。

- Go 1.22+，当前验证环境：`go1.22.12 linux/amd64`
- 唯一非标准库运行依赖：纯 Go SQLite 驱动 `modernc.org/sqlite v1.29.10`
- 不依赖 Docker、外部服务或线上数据

## 快速运行

```bash
go test ./...
go run ./cmd/slikernel -mode demo -seed 20261127 -out artifacts
go run ./cmd/slikernel -mode crash-demo -seed 20261127 -out artifacts
```

`demo` 会生成三版本工程、全量报告、增量报告和检查点；`crash-demo` 会执行真实子进程信号实验。两者写入：

- `artifacts/verification.json`：本次核验摘要
- `artifacts/signal-verification.json`：SIGTERM / SIGKILL 对照结果

## 代码结构

- `model.go`：统一内部模型、只读校验、深拷贝、规则版本
- `loader.go`：v1/v2/v3 JSON 装载、单位/时间基准/字段别名归一化
- `schema_codec.go`：同一内部工程的 v1/v2/v3 确定性编码
- `generator.go`：固定种子样本生成器
- `truth.go`：无缓存、逐窗口扫描的独立真值计算
- `engine.go`：窗口聚合、三类 SLI、预算、告警、并行执行
- `incremental.go`：规则/样本编辑后的失效传播与缓存复用
- `checkpoint.go`：SQLite 检查点保存、严格加载和恢复策略
- `cmd/slikernel`：本地演示与信号实验入口

## 三版本 Schema 映射

| 语义 | v1 | v2 | v3 |
|---|---|---|---|
| 版本字段 | `schema_version: "1"` | `schema: "2"` | `schema_version: "3"` |
| 时间 | epoch ms | 相对窗口序号 + 窗口内偏移 | epoch ms（也支持相对序号） |
| 时长 | 毫秒 | 秒 | 毫秒 |
| SLO 目标 | ppm | 基点 | 百分比 |
| 分位数 | ppm | 千分比 | 万分比/基点 |
| 告警阈值 | ppm | 基点 | 百分比 |
| 预算耗尽字段 | 可选，加载时丢弃 | 可选 `exhausted`，加载时丢弃 | 可选，加载时丢弃 |

装载后统一为：

- 时间：`TimestampMs`
- 时长：`WindowMs`
- 目标与分位：0..1,000,000 的整数定点 ppm
- 服务顺序、SLI 声明顺序、观测 `Seq` 序列保持声明顺序

同一语义工程的三种编码装载后，服务集合、SLI 顺序和观测序列完全一致。歧义字段冲突、未知单位、非法时间基准等返回稳定业务码，不做静默猜测。

## 计算口径

### 窗口

每个 SLI 声明 `WindowMs` 与滚动窗口数 `RollingWindows`。基窗口从逻辑原点按声明窗口长度切分，区间为半开区间 `[start, end)`：

```text
base start = origin + k * window_ms
eval start = origin + (k - rolling_windows + 1) * window_ms
eval end   = origin + (k + 1) * window_ms
```

正好落在边界上的样本属于后一个窗口。空窗口策略为 `include_empty`：

- 可用性和延迟 SLI：空窗口贡献 0 个观测，不伪造样本，也不把 0 填进分位样本集。
- 吞吐量 SLI：空窗口仍计入期望吞吐量，因此会产生吞吐缺口。

### SLI

所有比率使用 0..1,000,000 的定点 ppm，除法采用四舍五入：

- 可用性：`good_events / total_events`
- 延迟：`samples <= latency_threshold_us` 的达标率
- 吞吐量：`observed_events / (expected_events_per_window * rolling_windows)`，上限为 1,000,000 ppm

延迟分位使用离散最近秩算法，不做插值：

```text
rank = ceil(sample_count * quantile_ppm / 1,000,000)
```

分位与预算的整数乘除使用 128 位中间值，避免事件数扩大后溢出。

### 错误预算

```text
allowed_errors = floor(total * (1,000,000 - target_ppm) / 1,000,000)
consumed_ppm   = round(failures * 1,000,000 / allowed_errors)
remaining      = max(0, allowed_errors - failures)
exhausted      = failures >= allowed_errors
```

当 `allowed_errors == 0` 时，`failures > 0` 才算耗尽。耗尽标记只由失败量和允许错误量推导，不读取输入中的可选标记。

### 告警与门禁

每个阈值独立保存连续窗口计数：

- 当前窗口 `consumed_ppm >= threshold.burn_ppm` 时，计数加一；否则清零。
- 计数达到该阈值的 `consecutive_windows` 后触发。
- 相等边界按 `>=` 判定，结果可逐窗口复核。
- 输出当前匹配阈值、已触发阈值、最高声明顺序的触发阈值、SLI 值、消耗比例和 `gate_pass`。

## 增量失效传播

编辑类型包括 SLO 目标、聚合窗口长度、延迟阈值、分位数、滚动窗口数、阈值集合和追加观测。

依赖分为三级：

1. `aggregate`：服务/路由/状态码/窗口长度/延迟规则决定的基窗口聚合。
2. `evaluate`：滚动窗口 SLI 与预算。
3. `alert`：连续窗口告警与门禁。

失效规则：

- SLO 目标变化：复用聚合，只重算该 SLI 的预算和告警。
- 窗口长度/延迟规则变化：重算该 SLI 的聚合、预算和告警。
- 阈值变化：复用聚合和预算，只重算告警。
- 追加观测：按时间定位受影响基窗口，只重算覆盖该基窗口的评估窗口；连续告警从最早受影响窗口向下游传播。

每个结果输出 `provenance`，标明失效来源、聚合/预算/告警是否来自缓存，并直接附带该窗口命中的基窗口、评估和告警依赖路径。每个失效源同时输出完整传播闭包。测试逐项比较增量结果与同一编辑后工程的全量重算结果。

## 并行与背压

工作线程预算为 1..64，每线程窗口缓冲为 1..4096。任务按确定性顺序提交到有界队列，结果写入索引槽位，因此完成顺序不影响输出。

当待处理窗口数超过 `workers * per_worker_buffer`：

- 默认产生可观测的 `E_BACKPRESSURE` 信号并分批推进；
- `FailOnBackpressure=true` 时直接返回 `E_BACKPRESSURE`，不提交部分结果；
- 不越界分配、不静默跳过窗口。

每窗口耗时会写入诊断字段，但所有正确性比较均忽略耗时。

## SQLite 检查点

检查点保存：

- `rule_version`：规则版本哈希
- `observation_seq`：最后提交观测序号
- 每个基窗口聚合状态
- 每个评估窗口结果
- 单调递增的 `committed_seq`

保存顺序在同一 SQLite 事务中固定为：

1. 写基窗口聚合
2. 写评估窗口结果
3. 更新全局 `committed_seq`
4. COMMIT

进程在 COMMIT 前被终止时事务回滚。加载时校验：

- SQLite `quick_check`
- 格式版本和规则版本
- 行数与元数据计数
- 是否存在超前于 `committed_seq` 的孤儿行
- JSON 状态是否完整

规则版本不匹配时，严格 `Load` 返回 `E_CHECKPOINT_VERSION`；恢复流程整段重算，不复用旧聚合。

## 稳定业务码

| 业务码 | 场景 |
|---|---|
| `E_JSON` | JSON 语法/类型错误 |
| `E_SCHEMA` | 不支持的 schema 版本 |
| `E_AMBIGUOUS_FIELD` | 互斥字段同时出现、时间基准冲突 |
| `E_UNIT` | 未知时长/比率/分位单位 |
| `E_DUPLICATE_SERVICE` | 重复服务定义 |
| `E_DUPLICATE_SLI` | 同一服务内重复 SLI |
| `E_RATIO_RANGE` | 比率目标/阈值越界 |
| `E_QUANTILE_RANGE` | 分位刻度越界 |
| `E_WINDOW_LENGTH` | 负值、零值或越界窗口 |
| `E_THREAD_BUDGET` | 线程预算越界 |
| `E_BUFFER_BUDGET` | 缓冲预算越界 |
| `E_BACKPRESSURE` | 窗口缓冲容量不足 |
| `E_OBSERVATION` | 观测字段非法 |
| `E_OBSERVATION_SEQUENCE` | 观测序号非严格递增 |
| `E_OBSERVATION_RANGE` | 观测时间不在声明评估区间 |
| `E_CHECKPOINT_CORRUPT` | 截断、计数不符、孤儿提交、JSON 损坏 |
| `E_CHECKPOINT_VERSION` | 检查点版本不匹配 |
| `E_NO_CHECKPOINT` | 尚无检查点 |
| `E_EDIT` | 编辑参数或依赖目标非法 |

所有业务错误包含 `Op` 和稳定定位上下文（服务、SLI、序号、字段等）。

## 固定种子验收结果

本次实际运行 `seed=20261127` 的结果：

- 服务数：3
- SLI 定义数：9
- 观测样本数：104
- 评估窗口数：90
- 三版本装载一致：通过
- 逐窗口 SLI、预算、耗尽标记、告警与独立真值一致：通过
- 目标改动增量结果与全量重算一致：通过；重算 10 个窗口，复用 80 个窗口和 108 个基窗口
- 1/4/8 线程、不同批次与提交顺序结果一致：通过
- SQLite 检查点保存/加载一致：通过
- 稳定异常码：8 类已实际注入
- SIGTERM：退出码 0，生成优雅退出报告
- SIGKILL：退出码 -1，不生成退出报告，已提交检查点仍可加载

详细结果见 `artifacts/verification.json` 和 `artifacts/signal-verification.json`。

## 口径约束

- 评估区间起点和终点必须相对逻辑原点按各 SLI 的 `WindowMs` 对齐。
- `RollingWindows` 不得超过评估区间包含的基窗口数。
- 延迟分位采用最近秩而非线性插值；这是内核的显式离散口径。
- 吞吐 SLI 以声明的 `ExpectedEventsPerWindow` 为期望量。
