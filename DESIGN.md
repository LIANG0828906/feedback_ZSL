# 转发精度诊断组件设计

## 交付范围

本仓库实现一个纯 Go 标准库、可离线重复运行的转发规则诊断组件与终端工作台。它不发起真实网络请求，不依赖真实上游、外部账号、Docker 或第三方 Go 依赖。

```text
cmd/proxydiag      命令行与交互式工作台
bizerr             稳定 E_xxx 业务码与结构化上下文
domain             规范化转发模型、真值结构、域校验
fixed              声明位宽定点乘除、加法和饱和检测
schema             v1/v2/v3 严格解析与归一
engine             匹配、改写、权重选择、重试、逐跳追踪、增量缓存、并发准入
checkpoint         追加式 prepare/commit 检查点
samplegen          固定种子工程生成器与独立参考真值构造
diag               验收编排：三版核验、增量、检查点、异常注入
samples            已生成的本地样本与真值
```

## 三版 schema 与归一

三个文件描述同一份规范工程，核心指纹（监听入口、路由、上游、权重、请求）完全一致。

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 字段命名 | snake_case | camelCase / 分组对象 | snake_case |
| 路由表 | 顶层扁平 `routes` | 监听入口内嵌 `routes` | 监听入口内嵌 `routes` |
| 超时 | `timeout_ms` | `timeout.ticks * clock.tickMs` | `timeout_ms` |
| 在途预算 | `inflight_limit` | `budget.requestsPerSecond` 换算 | `max_inflight` |
| 权重 | 千分比 `weight_permille` | 万分比 `weightBasisPoints` | 万分比 `weight_per_myriad` |
| 重试刻度 | 毫秒 + 千分比 | tick + 万分比 | 毫秒 + 千分比 |
| 健康探测 | 无 | 可选 tick 字段 | 可选毫秒字段 |
| 请求到达 | 毫秒 | tick | 毫秒 |

加载器先读取 `schema` 判别版本，再以 `DisallowUnknownFields` 严格解码。未知 schema 返回 `E_SCHEMA_UNKNOWN`；混入其他版本字段或字段类型不兼容返回 `E_SCHEMA_FIELD_COMBO`，不会静默忽略。

规范化后权重统一为 PPM，超时与到达时间统一为整数毫秒，退避倍率统一为千分比。可选健康探测是 schema 扩展信息，因此不参与核心等价指纹；其 tick/ms 换算仍会归一。

## 匹配、改写与选择

1. 监听入口按最长前缀胜出，同长时取声明较早者，依据码 `D01`，同长裁决记录 `D02`。
2. 在入口内按“入口前缀 + 路由相对前缀”比较最长前缀，依据码 `D03`，同长裁决记录 `D04`。
3. 路径改写支持 `strip_prefix`、`add_prefix`、头删除与头覆盖；查询串在路径改写后原样保留。
4. 只从路由引用且健康的上游中按权重选择。哈希槽只依赖工程版本、请求序号、路由与改写后请求行，不使用 Go map 顺序。
5. 选中上游后，其 `max_retries` 决定总尝试数；所有尝试保持同一路由和同一改写后请求形态。

超时判定使用整数毫秒：名义耗时 `>=` 入口超时即判超时。退避为：

```text
base = retry_tick_ms * backoff_permille / 1000
delay(n) = base * (1000 + (n-1)*250) / 1000
```

每次尝试输出 `start_ms/end_ms/outcome/trigger_reason/retry_scheduled`，终态为 `T_SUCCESS`、`T_RETRY_EXHAUSTED`、`T_NO_HEALTHY_UPSTREAM`、`T_NO_ROUTE`、`T_NO_LISTENER` 或 `T_BACKPRESSURE`。

## 定点与逐跳真值

工程声明路径/权重宽度、小数位与容差。`fixed.MulDiv` 和 `fixed.Add`：

- 使用非负定点整数；
- 结果超出声明位宽时显式标记 overflow/saturation；
- 运行链路发现饱和时返回 `E_FIXED_OVERFLOW`，不静默截断；
- 权重总和、候选归一权重、选择目标、退避、尝试窗口与路径重组指标都进入 `trace`。

真值由 `samplegen.BuildTruth` 从规范构造独立计算，而不是调用运行时引擎。校验时每个 hop 输出 `value/truth/delta/tolerance/within_tolerance`；高层字段另做监听入口、路由、请求行、头集合、上游、尝试与终态逐项比对。40 个样本在三个版本上均为 `max_abs_delta=0`。

## 增量失效

`engine.Engine` 按请求序号保存转发结果。编辑按逻辑序号施加：

- 健康/权重编辑：只重算该序号之后、实际引用该上游的路由所命中的请求；
- 路径改写编辑：只重算该序号之后命中目标路由的请求；
- 请求增删：重算编辑序号之后的后缀区间；
- 未受影响结果原样复用，并标记 `cache.reused=true`；
- 每步输出失效来源 `I_*`、传播闭包、重算序号和复用序号；
- 成功编辑后才整体替换快照，失败编辑不改变活动快照或已提交缓存。

整份快照替换由 `ReplaceSnapshot` 处理：版本变化即丢弃全部缓存并全量重算；同版本但内容变化返回 `E_SNAPSHOT_VERSION_MISMATCH`，且旧缓存继续可用。绝不按配置内容长度判断快照身份。

## 并发与背压

并发以“同一逻辑到达时刻的请求批”为确定性的准入单位，批内任务可并行：

- 工作线程预算限制单批实际执行的任务数；
- 每监听入口缓冲上限可被诊断参数缩小，但不能放大；
- 超过入口缓冲返回 `E_BACKPRESSURE_BUFFER`；
- 超过线程预算返回 `E_BACKPRESSURE_WORKERS`；
- 每个输入序号在结果中恰好出现一次，背压请求以显式失败结果返回，不静默丢弃；
- 结果按请求序号排序，进入评估的请求不读取调度顺序；
- 记录每个入口实际观测的 `tasks/max_elapsed_ns/total_ns`。

## 检查点协议

环境只允许标准库。为避免引入不受仓库控制的 SQLite 驱动，本实现采用题目允许的纯文本追加日志，保持与 SQLite 检查点相同的提交顺序语义：

```text
prepare 记录：工程版本、请求序号、监听入口判定、上游健康快照、SHA-256
commit 记录：同一 txid、全局请求序号、prepare 校验和、SHA-256
```

写入顺序固定为 `prepare -> fsync -> commit -> fsync`。只有 commit 会推进 `LastCommittedSeq`；prepare-only 记录在恢复时被忽略。恢复：

- 版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH` 并拒绝整段缓存；
- 末尾截断返回 `E_CHECKPOINT_TRUNCATED`，但此前已提交状态仍返回并可用；
- 校验和损坏返回 `E_CHECKPOINT_CHECKSUM`，此前 commit 仍可用；
- prepare/commit 不配对或序号倒退返回 `E_CHECKPOINT_ORDER` / `E_CHECKPOINT_SEQUENCE`。

诊断编排在版本不匹配时会丢弃检查点状态，并对当前匹配快照全量重算；演示报告给出重算条数。

## 稳定业务码

主要异常码：

| 业务码 | 场景 |
|---|---|
| `E_SCHEMA_UNKNOWN` | 未知 schema |
| `E_SCHEMA_FIELD_COMBO` | 版本不兼容字段组合 |
| `E_LISTENER_OVERLAP` | 监听入口前缀重叠 |
| `E_UPSTREAM_MISSING` | 路由引用缺失上游 |
| `E_REWRITE_EMPTY` | 改写产生空/非法请求行 |
| `E_REWRITE_STRIP` | 改写剥离前缀不匹配 |
| `E_WEIGHT_RANGE` | 权重或退避倍率越界 |
| `E_INFLIGHT_RANGE` | 在途预算越界 |
| `E_TIMEOUT_ORDER` | 超时/重试/时间顺序倒置 |
| `E_FIXED_OVERFLOW` | 声明位宽定点溢出或饱和 |
| `E_BACKPRESSURE_BUFFER` | 入口缓冲耗尽 |
| `E_BACKPRESSURE_WORKERS` | 工作线程预算耗尽 |
| `E_CHECKPOINT_TRUNCATED` | 检查点尾部截断 |
| `E_CHECKPOINT_CHECKSUM` | 检查点校验失败 |
| `E_CHECKPOINT_VERSION_MISMATCH` | 检查点版本不匹配 |
| `E_SNAPSHOT_VERSION_MISMATCH` | 快照内容变化但版本未变 |

## 本地验收

```bash
make build
make test
make race
make generate
make verify
make demo
```

交互工作台：

```bash
./bin/proxydiag workbench -samples samples
```

支持 `load/run/show/hops/health/weight/rewrite/delete/add/compare/checkpoint/inject/verify`。
