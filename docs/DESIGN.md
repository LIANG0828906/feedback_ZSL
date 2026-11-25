# Feature Kernel 设计说明

## 包结构

- `internal/kernel`：规范模型、稳定 `E_*` 错误、精确窗口归属、定点权重与聚合器。
- `internal/schema`：`v1/v2/v3` DTO、严格 JSON 解码、版本识别、规范化映射与反向编码。
- `internal/gen`：固定种子工程生成器、独立参考真值、文件/manifest 输出。
- `internal/engine`：单特征 Runtime、增量累积/逐窗重扫、有界 worker pool、并行 ResultSet。
- `internal/checkpoint`：纯 Go SQLite 事务存储、恢复 Runner、JSON 快照与截断/版本校验。
- `internal/incremental`：编辑克隆、失效来源、传播闭包、缓存复用与选择性重算。
- `cmd/featurectl`：`generate`、`verify`、`diagnose`、`consume`、`snapshot` 入口。

## 三版 Schema

| 版本 | 字段风格 | 时刻 | 窗口基准 | 权重 | 水位 |
| --- | --- | --- | --- | --- | --- |
| `v1` | snake_case | `event_time_ms` epoch 毫秒 | `epoch_hour` 整点 | `weight_scale=1000` 千分比 | 显式 `watermark_ms` |
| `v2` | camelCase | `observedSequence`，按 `timeBaseMs + seq*tickMs` 还原 | `first_data_point` | `weightScalePer10K=10000` 万分比 | 显式 `watermarkSequence` |
| `v3` | kebab-case | `epoch-ms` | `first_data_point` | `weight-scale-per-10k=10000` | 无水位字段；`barrier` 按 `D-WM-MAX-SEEN-LAG` 派生 |

生成器保证数据首点就是整点，因此整点锚点与首点锚点规范化为同一个 `AnchorMs`。权重统一转成 PPM：千分比乘 `1000`，万分比乘 `100`。v3 输出标记 `explicit_field_enabled=false`、`degraded=true` 和规则号 `D-WM-MAX-SEEN-LAG`，但派生出的水位与 v1/v2 相同。

严格解码器开启 `DisallowUnknownFields`。未知字段返回 `E_SCHEMA_UNKNOWN_FIELD`，不支持版本返回 `E_SCHEMA_UNSUPPORTED_VERSION`，字段组合缺失/冲突返回 `E_SCHEMA_INVALID_COMBINATION`。

## 窗口归属

窗口为半开区间 `[start, end)`，`start = anchor + index*slide`。

- 整数倍 `width % slide == 0`：规则号 `R-WIN-INT-RATIO`，`maxIndex=floor((t-a)/S)`，`minIndex=maxIndex-width/S+1`。
- 非整数倍：规则号 `R-WIN-FRACTIONAL-RATIO`，`minIndex=floor((t-a-W)/S)+1`，`maxIndex=floor((t-a)/S)`。

索引可向负方向扩展，因此锚点时刻的事件也会归属所有仍覆盖该时刻的重叠滑动窗口；`end == event_time` 的旧窗口按半开区间排除。

## 聚合与路径选择

支持 `count/sum/avg/min/max/weighted_avg`，聚合函数次序按特征声明保留。通道定点权重规范化为 PPM，加权平均为 `sum(value*weightPPM)/sum(weightPPM)`。

路径判定规则：`PATH-WIDTH-STEP-RATIO-GT-THRESHOLD`。

- `width/slide > threshold`：`incremental_accumulation`
- 否则：`full_window_rescan`
- 比值恰好等于阈值：按 `EQUAL_TO_THRESHOLD_FALLS_BACK_TO_RESCAN` 回退重扫，保证边界可复核。

重扫路径从事件流重新归属并跳过丢弃键；测试强制两条路径处理同一输入，并用 `kernel.CompareWindowResults` 在 `1e-6` 定点容差内比较。

## 水位与迟到

每个特征声明 `allowed_lateness_ms`。若当前水位为 `w`，事件时间为 `t`：

- `w <= t`：未迟到，正常计入。
- `0 < w-t <= allowed_lateness`：允许迟到；已存在且有成员的窗口记录 recomputation，并增量加入或在重扫路径重新计算。
- `w-t > allowed_lateness`：按“事件 × 归属窗口”写 `DiscardedRecord`，包含 `window_id/index/start/end/late_ms/code=E_LATE_BEYOND_ALLOWED`。

当 `w >= window.end + allowed_lateness` 时窗口标记 submitted。回退到早于已提交窗口结束的水位返回 `E_WATERMARK_BEFORE_COMMITTED_WINDOW`。

## 并行与背压

`Budget.Workers` 范围是 `1..64`，`MaxWindowsPerFeature` 范围是 `1..4096`；越界分别返回 `E_BUDGET_WORKERS_OUT_OF_RANGE`、`E_BUDGET_BUFFER_OUT_OF_RANGE`。

- `WorkerPool.TrySubmit` 在 worker 全忙时立即返回 `E_BACKPRESSURE_THREAD_EXHAUSTED`，不创建无界 goroutine。
- 事件需要新建的窗口数会先预算；若 `existing+missing > limit`，返回 `E_BACKPRESSURE_BUFFER_FULL` 且不修改窗口表。
- 每个特征状态独立，特征内严格按逻辑 seq 推进；特征间并行。worker 数、调度顺序和提交批次只影响 `FeatureDiagnostic.ElapsedNanoseconds`，不影响成员和聚合。耗时字段明确仅用于诊断。

## SQLite 续算

数据库表：

- `checkpoint_meta`
- `global_cursor(id=1, seq)`
- `window_states`（含完整累加器与成员 seq）
- `watermarks`
- `discards`
- `recomputations`

提交事务内的顺序：

1. upsert 所有 `window_states`
2. 写 meta、watermark、discard、recomputation
3. update `global_cursor.seq`
4. commit

崩溃注入点位于步骤 1 后、步骤 3 前，并发送真实 SIGKILL。未 commit 的事务由 SQLite 回滚，所以全局 seq 不会领先于窗口状态。SIGINT/SIGTERM 使用 `signal.NotifyContext`，Runner 在下一记录前停止；若事务因 context 取消失败也回滚到上一完整提交点。

检查点保存 `checkpoint.v1` 与 `canonical.v1`。版本不匹配时 `LoadSnapshot/NewRunner` 返回 `E_CHECKPOINT_VERSION_MISMATCH`；`ResetMismatchedCheckpoint` 清空旧窗口后从 seq 0 全量重算。JSON 快照使用 payload checksum，截断或 checksum 不符返回 `E_CHECKPOINT_TRUNCATED`。

## 增量失效

`incremental.Cache` 保存窗口与派生结果。编辑先深拷贝工程，失败时源工程和旧缓存均不改变。

- 宽度/聚合集合变更：失效该特征全部窗口。
- 事件增/改/删：对旧时刻与新时刻分别求归属窗口，只失效这些窗口。
- 派生指标：输入窗口进入失效集时，派生窗口进入传播闭包。
- 未失效窗口直接复用缓存，并写 `status=reused` 的 provenance。
- 重算结果写 `status=recomputed`、`invalidation_sources` 与 `propagation_closure`。

测试对宽度、聚合集合、追加、更新、删除分别执行增量结果与一次性全量引擎的逐窗口比较。
