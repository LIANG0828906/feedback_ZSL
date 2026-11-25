# 采样决策与片段归并内核说明

## 模块结构

- `internal/model`：canonical 工程、事件、决策、预算、归并与失效闭包模型。
- `internal/sample`：固定种子的确定性样本工程、三版 JSON 文档与生成器真值。
- `internal/loader`：v1/v2/v3 DTO、严格未知字段定位、版本归一化与反向渲染。
- `internal/projectvalidate`：采样率、服务引用、分摊上限、尾规则环和事件完整性校验。
- `internal/sampling`：FNV-1a 稳定桶首部采样，显式处理 0 与 10000 边界。
- `internal/tail`：按 `priority + declaration order + id` 固定顺序执行尾采样/强制保留联合裁决。
- `internal/budget`：毫秒窗口、全局跨度预算、服务分摊额度、拒绝、强制保留与越界账本。
- `internal/merge`：去重、父引用拓扑排序、乱序输入不变性、孤儿挂接/隔离。
- `internal/engine`：全量重放与带缓存的增量失效闭包。
- `internal/runtime`：有界 worker、每 trace 缓冲背压、SQLite 检查点恢复流程。
- `internal/store`：`modernc.org/sqlite` 纯 Go 驱动的检查点存储。
- `internal/exceptions`：九类异常注入与稳定业务码核验。
- `internal/verify`：CLI 与测试共用的验收断言。
- `internal/acceptance`：端到端验收测试。

## 三版 Schema 归一化

| 版本 | 命名风格 | 比率/分摊刻度 | 时刻 | 分摊结构 | 强制字段 |
| --- | --- | --- | --- | --- | --- |
| `sampling.project/v1` | `snake_case` | 千分比，装载时乘 10 | `occurred_at_epoch_ms` | `service_allocations` 扁平表 | `forced_retention` 始终输出 |
| `sampling.project/v2` | `camelCase` | 万分比/基点，原样 | `atEventOffset`，由 `clockOriginEpochMs + offset * millisPerSeqStep` 还原 | `allocationTree` 调用树嵌套表，装载时前序扁平化 | `forceKeep,omitempty`，仅 true 时出现 |
| `sampling.project/v3` | `PascalCase` | 千分比，装载时乘 10 | `EpochMS` | `ServiceShares` 扁平表 | `Forced` 始终输出 |

归一化后的采样率和分摊权重均为万分比 `[0,10000]`。三版文档由同一 canonical 工程渲染，再经各自装载器解析；验收测试用 `cmp.Diff` 验证 canonical 工程完全相等，并用同一组真值验证观测结果完全相等。

严格 JSON 校验会同时利用 DTO 的 `json` tag 反射遍历原始文档。未知字段返回：

```text
E_SCHEMA_UNKNOWN_FIELD [sampling.project/v1] $.unexpected_diagnostic_field: ...
```

## 确定性决策

首部采样桶为：

```text
fnv1a32(project_id \0 request_id \0 edge_id) mod 10000
```

- 比率为 0：`ALWAYS_OFF`。
- 比率为 10000：`ALWAYS_ON`。
- 其他：`DETERMINISTIC_HASH_KEEP` 或 `DETERMINISTIC_HASH_DROP`。

该函数无随机数、无 map 迭代依赖、无可变全局状态，因此同一请求在任意 goroutine、批次和重试中得到同一结论。

尾采样规则按优先级、声明次序和规则 ID 排序。输出包含已评估规则、命中规则、选中规则、裁决路径和证据跨度。强制保留由调用边的 `forced_retention` 触发，结论固定为 `FORCED_RETAIN`；强制跨度单独计入 `forced_spans`，不增加服务 `charged_spans`，因此不占用可分摊额度。全局额度不足时，强制追踪允许形成负的全局剩余并标记 `overshot=true`。

## 预算窗口

预算单位是“跨度权重”：一条含 N 个唯一跨度的保留追踪消耗 N。服务额度为：

```text
floor(global_limit_spans * weight_permyriad / 10000)
```

账本按 `floor(timestamp_ms / window_ms) * window_ms` 推进窗口。每个预算决策都输出：

- 窗口起点/终点和显式 `WindowBasis`，例如 `same_window ...` 或 `rollover ... prior_used=12`；
- 全局限制、已用、剩余、是否越界；
- 全部服务的额度、普通扣费、强制扣费和剩余；
- 稳定拒绝码 `E_BUDGET_GLOBAL_EXHAUSTED` 或 `E_BUDGET_SERVICE_EXHAUSTED`。

全局策略支持 `reject` 与 `allow_overshoot`；强制保留始终必须接受并可越界。

## 片段归并

归并器先按 `SpanID` 去重，重复 ID 进入 `duplicate_span_ids`，重复上报不会改变顺序。随后：

1. 保留空父引用根跨度；
2. 校验父跨度是否已到达；
3. 用父先子后的拓扑排序输出因果序列；
4. 同层按 `causal_order`、服务和 SpanID 稳定排序；
5. 缺父跨度按工程策略处理：
   - `quarantine`：进入 `unmerged_spans`，记录 `E_ORPHAN_SPAN` 与原因；
   - `attach_to_trace`：挂到追踪根层，标记 `orphan_attached_to_trace_root` 和原因。

## 增量重算

`engine.Cache.Apply(Edit)` 深拷贝工程，绝不修改基线工程或缓存。依赖边为：

```text
edge_rate -> head_decision -> tail_decision -> budget_decision -> later budget_decision(same window)
tail_condition -> tail_decision -> budget_decision -> later budget_decision(same window)
allocation_weight -> all budget snapshots
```

只有首部 Keep 结论实际翻转时才继续失效对应尾裁；只有尾裁 Keep/Forced 改变时才继续预算链。预算区间从受影响 settle 开始，继承前一预算事件后的窗口快照，再顺序重放该窗口后续 settle。版本窗口互不传导。

`Closure` 输出：

- 根变更源，例如 `edge:e-checkout.base_rate_permyriad`；
- 每个失效节点、直接来源、根来源和原因；
- 事件序号区间；
- 实际重算节点和复用缓存节点。

验收不仅比较核心报告，还使用 `reflect.DeepEqual` 比较增量结果与全量结果的逐事件 `Events` 视图。

## 并行与背压

`runtime.Options` 声明：

- `Workers`：1..64；
- `BufferCap`：每 trace 待归并跨度上限 1..4096。

越界参数分别返回：

- `E_RUNTIME_WORKER_BUDGET_INVALID`
- `E_RUNTIME_BUFFER_BUDGET_INVALID`

缓冲满时新跨度返回 `E_BACKPRESSURE_BUFFER`，不会覆盖或静默丢弃。worker 信号量耗尽时 `TryAcquireWorker` 返回 `E_BACKPRESSURE_WORKERS`；`RunAllParallel` 使用固定数量 worker 推进多 trace 归并。

## SQLite 检查点

数据库包含 `checkpoint_meta`、`trace_state`、`service_state`。`Store.SaveProgress` 在一个事务中按以下顺序执行：

1. upsert 全部 trace 归并状态；
2. upsert 全部服务预算状态；
3. 最后 upsert 全局 `last_seq` 与 `project_version`；
4. commit。

外部读取只通过 `status='complete'` 查询完成追踪，因此 `collecting` / `adjudicating` 状态不会被当作可交付追踪。测试在 seq=20 提交后注入 `E_CRASH_AFTER_COMMIT`，重开库只能读到两条 complete trace；匹配版本恢复后从 seq=21 继续并复现真值。版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`，运行器清空检查点并从 seq=1 整段重算。截断/损坏数据库返回 `E_CHECKPOINT_TRUNCATED`。

## 常用命令

```bash
go version
go mod tidy
go test -count=1 ./...
go test -race -count=1 ./internal/acceptance ./internal/budget
go run . verify
go run . demo --out artifacts/demo --db artifacts/demo/checkpoint.db
go run . inject --dir artifacts/exceptions-cli
go run . load artifacts/demo/project-v2.json
```

`demo` 会生成三版工程 JSON、真值 JSON、逐事件决策、归并序列、增量闭包、崩溃恢复对照和异常码表。
