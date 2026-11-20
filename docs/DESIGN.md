# 技术设计说明

## 批次与列式结构

`iter_jsonl_batches` 逐行解析 JSON，并将一批文档转换为 `ColumnarBatch`。每个字段
成为一列；缺失字段使用内部 `MISSING` 哨兵保存，因此可与显式 `null`、正常值区分。
执行器按列批次消费，但分组累加在每批内先按分组和目标字段收集，再按绝对值排序。

## 状态大小与溢出协议

`MetricState` 的大小与分组数有关，与已处理的文档数无关。状态包含固定数量的浮点、
十进制、计数与来源元数据字段。状态字节阈值使用规范 JSON 序列化长度估算。

一次溢出的协议：

1. 对分组 ID 排序，轮转分配到固定数量桶；
2. 每个桶写唯一命名的 `.tmp` 文件；
3. 全部写入并 `fsync` 后，依次原子改名；
4. 任一步失败则删除本次全部临时/已改名文件，保留内存状态；
5. 成功后清空内存状态并追加溢出事件元数据。

轮转分配使同一分组在不同溢出事件中可能进入不同分区文件，从而覆盖“分组键跨分区”
的合并路径。最终文件按事件序号、分区序号排序，执行顺序完全确定。

## 误差界传播

设 `u = 2^-53`。binary64 执行通道记录 `abs_total` 与来源转换误差
`conversion_error`。补偿求和使用保守界：

```text
accumulation_bound
  = 2*u*abs_total + 8*n*u^2*abs_total
```

这是实际 Neumaier 误差上界的宽松版本，便于吸收状态本身的计算舍入。每次部分状态
合并再加入：

```text
merge_bound
  = 2*u*(|left_sum| + |right_sum|)
    + 8*u^2*(abs_total_left + abs_total_right)
    + 4*tiny
```

`avg` 将求和界除以样本数，并加入一次最终除法舍入。`min`/`max` 使用组内最大来源
转换误差作为比较重排界。`stdDev` 使用 Welford/Chan 模型界和高精度审计差值的较大者。

审计通道以 100 位十进制保存确定性高精度近似。最终声明界为：

```text
model_bound
  = conversion + accumulation + merge + operation padding

audit_bound
  = 1.01 * |binary64_result - audit_result| + audit_uncertainty

declared_bound
  = 1.01 * max(model_bound, audit_bound) + 4*tiny
```

因此估值同时具备解析传播项和独立高精度观测项。验收使用精确有理数或参考定点模拟
重新计算，而不是复用执行器的审计通道。

## 合并一致性

每个聚合状态都实现 `merge`。`sum` 合并两者总项和补偿项，并累积 `merge_events`；
`stdDev` 使用 Chan 平行合并：

```text
delta = mean_b - mean_a
mean  = mean_a + delta * n_b / n
M2    = M2_a + M2_b + delta^2 * n_a * n_b / n
```

合并后以 `distinct_groups_in_partials` 与 `distinct_groups_after_merge` 核对分组
计数。误差界只增不减。`group_count_consistent` 为最终验收字段。

## 异常隔离

错误以 `(group_id, aggregate_name)` 为作用域。某组某聚合发生缺字段、类型或标度
错误后，该聚合输出 `status: error` 与错误码，但同组其他聚合和所有其他分组继续完成。
`stdDev` 的单样本错误只在最终形成结果时判定，不影响同组 `sum`/`avg`/`min`/`max`。

溢出写失败是执行级可恢复错误：内存状态不会被丢弃，因而已完成和后续分组仍可输出。

## 确定性与验收

报告不含时间戳、随机数、临时目录或进程信息。分组按键规范 JSON 排序，错误按发生路径
和输入行序生成。JSON 使用 `sort_keys=True`。演示脚本运行完整套件两次，并逐字节比较
正常报告、参考真值、异常报告、无溢出报告和故障注入报告。
