# 设计说明

## 1. 分层

`protocol` 只负责规范化输入、静态校验和确定性调度；`kernel` 不接触 SQLite 文件，只依赖 `CheckpointSink`；`store` 实现该接口。这样一次性重放可以完全绕开持久化，实时与恢复路径则共享同一帧提交逻辑。

## 2. 调度

`BuildSchedule` 将事件和帧窗口归并为稳定序列：

1. 逻辑时间升序；
2. 同刻相位：`negotiate`、`topology`、`leave`、`join`、状态控制、帧；
3. 同相位按显示标识、事件序号、窗口序号稳定排序。

引擎游标只在该指令完整成功或该帧完成提交后前进。映射失败、状态非法、仲裁冲突、检查点写入失败均不会推进游标。

## 3. 帧提交原子边界

窗口提交分为两段：

1. 在局部状态中完成全部目标屏映射、结果排序、端集合快照和轨迹构造；
2. 先写 SQLite 检查点，成功后再替换内存中的屏幕状态、游标、进度与轨迹。

若进程在 SQLite 提交后、内存提交前终止，重启会从该窗口恢复；若 SQLite 事务未提交，该窗口仍可重试。相同窗口和相同快照哈希的重复写入是幂等的。

## 4. 拓扑版本

初始拓扑版本为 1。每次成功应用 `join`、`leave` 或 `topology` 事件后递增。检查点加载时根据规范化调度中截至 `NextAction` 的拓扑类事件数重算期望版本，防止仅篡改行版本或仅篡改快照版本。

## 5. 内容哈希与整体失效

内容哈希是完整会话对象规范 JSON 的 SHA-256，不包含生成器真值。检查点每行还保存快照 JSON 的 SHA-256。加载任一行时出现以下任一情况即整体失效：

- 窗口序号不连续；
- 算法版本不一致；
- 内容哈希不一致；
- 快照哈希不一致；
- 快照无法反序列化；
- 快照元数据、窗口数或 `NextAction` 不一致；
- 拓扑版本与重算值不一致；
- 端集合/屏幕快照不合法；
- 元数据表缺失或与行不一致。

失效操作在同一事务内删除该会话全部快照与元数据。

## 6. 映射公式

设受控屏逻辑原点为 `(ox, oy)`，缩放比为 `n/d`。对交集局部坐标 `(lx, ly)`：

```text
pixel_x = crop(lx * n / d)
pixel_y = crop(ly * n / d)
```

`crop` 由声明方向决定：

- `floor`：两端向下取整；
- `ceil`：两端向上取整；
- `nearest`：两端就近取整；
- `outward`：左/上向下，右/下向上；
- `inward`：左/上向上，右/下向下。

结果再裁剪到 `[0, resolution.width] x [0, resolution.height]`。所有除法使用显式整数 floor/ceil/round，不使用浮点。

## 7. 并行资源边界

目标屏按 ID 排序后连续分片到最多 `max_workers` 个 goroutine。每个 worker 顺序处理自己的队列：

- 每个屏的像素暂存量不超过 `per_worker_pixel_bytes`；
- 超过上限的区域拆为多个临时块复用同一缓冲；
- 排队屏写入 `queue_records`，包含 worker 和原因；
- 最终结果重新按 ID 排序，保证跨路径结果稳定。

## 8. 异常隔离

- `E_DECODE`：JSON 解码失败；
- `E_ORDER`：序号或时间倒置；
- `E_CAP`：能力/拓扑交叉字段冲突；
- `E_SEG`：`FailureInjector` 在指定窗口返回故障，错误携带窗口和屏，失败历史单独保存，不进入窗口轨迹；清除注入后原位重试，结果与无故障路径逐字段一致。

## 9. 恢复 API

```go
snapshot, status, err := checkpointStore.LoadLatest(session, protocol.CurrentAlgorithmVersion)
if status.Invalidated {
    // 整体快照已删除，从 NewEngine 开始全量重放。
}
engine, err := kernel.RestoreEngine(session, kernel.Config{
    Mapping:    kernel.DefaultMappingConfig(),
    Checkpoint: checkpointStore,
}, *snapshot)
trace, err := engine.Run(ctx)
```

`RestoreEngine` 还会复核会话 ID、内容哈希、算法版本、窗口数、`NextAction`、拓扑版本和轨迹窗口数，拒绝部分状态拼接。
