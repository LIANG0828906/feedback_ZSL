# TickArena 设计说明

## 1. 目标与取舍

TickArena 把“规则一致性”放在性能优化之前：交互模式和批模式共享同一个 tick 状态机，批模式只改变外层调度、事件读取 chunk 大小与检查点频率。所有规则数值使用 Python 整数；性能主要来自流式事件读取、流式回放写入和 O(players) 的活跃状态。

单局状态不保留历史 tick。历史曲线由回放 `tick` 记录承载；校验器再次推演时逐条比较，不依赖引擎的内存快照。

## 2. 推演顺序

每 tick 固定执行：

1. 把 tick 推进到 `state.tick + 1`。
2. 按玩家输入顺序处理到期复活：位置回出生点，生命/资源按基点恢复，冷却清零。
3. 按玩家输入顺序处理存活单位：冷却减一，资源增加 `passive_resource`。
4. 从事件源按原始输入顺序读取 `event.tick <= current_tick` 的事件。正常输入时只会有当前 tick；迟到事件记录 `E_ORDER`。
5. 单 tick 最多处理 `event_buffer_limit` 条事件，其余事件丢弃并写一条带 `count` 的 `E_BUFFER` 摘要。
6. 技能造成击杀后增加队伍比分。若达到 `win_score`，立即锁定结果；同一 tick 剩余事件记 `E_FINISHED`。
7. 最后一个 tick 仍未锁定时，执行 tick-limit 判胜。
8. 更新事件消费偏移，写逐 tick 记录；命中快照间隔时写完整快照。
9. 命中检查点窗口时，先 flush 回放，再原子提交检查点。

同一 tick 的事件绝不排序或并行结算。事件源序号（从 0 开始的输入顺序）也是 RNG 地址的一部分。

## 3. 数值模型

全部数值是 Python `int`。定点比例单位为 1 基点（`BPS = 10000`）。

```text
apply_bps(v, bps) = (v * bps + 5000) // 10000   # v >= 0
```

伤害：

```text
hit_roll   = derive(seed, tick, event, "skill:<id>:hit") % 10000
crit_roll  = derive(seed, tick, event, "skill:<id>:crit") % 10000
jitter_bps = derive(...jitter...) % (2*variance_bps + 1) - variance_bps
damage     = apply_bps(skill.damage, 10000 + jitter_bps)
damage     = apply_bps(damage, crit_multiplier_bps) if crit else damage
damage     = max(1, damage - defense)
```

资源、采集和回血同样只做整数加/减/乘除。任何输入树中的浮点（包括 `1.0`）都会在规则执行前被拒绝；初始表也会因此抛出带 `E_NUM` 的 `TickArenaError`。

同一 tick 内多个事件按输入顺序逐个结算，所以伤害和资源累计不依赖批大小、线程数或分组方式。

## 4. 确定性随机

```python
derive_u64(seed, tick, event_index, purpose)
```

内部使用 BLAKE2b 8 字节摘要。一次判定只由四个坐标决定：

- 对局种子。
- 当前 tick。
- 输入事件序号。
- 规则标签，例如 `gather:drop`、`skill:bolt:crit`。

因此不需要保存可变 RNG 内部状态；检查点保存 `rng_decisions` 仅作为进度和审计指标。

## 5. 状态与快照

动态单位状态：

- `x`, `y`
- `hp`, `resource`
- `alive`, `respawn_at`
- `cooldowns`
- `items`

逐 tick 记录包含位置、生命、资源、存活、冷却、比分、结果、事件摘要和事件消费偏移。完整快照额外包含最大生命/资源、玩家、阵营、物品、复活时刻、RNG 决策数和拒绝计数。

## 6. 回放写入

`ReplayRecorder` 只追加流式记录：

1. 写同目录 `.partial` 文件头。
2. 每 tick 追加 `tick`，按间隔追加 `snapshot`。
3. 更新正文 SHA-256。
4. 完成时追加 `footer`，flush/fsync，关闭文件，`os.replace` 成最终回放。

检查点保存 partial 的字节长度、记录数、正文哈希与最后 tick。恢复时先截断检查点之后未提交的尾部，再继续追加。若检查点与 partial 不匹配，回放恢复失败并走整局重推。

## 7. 独立校验

校验器的主要阶段：

1. `load_and_validate_replay()` 校验 header 版本、初始表指纹、正文 canonical JSON、正文 SHA-256、页脚和记录数。
2. 打开事件流并核对事件流指纹。
3. 使用校验器自己的 `_Verifier` 状态机重新执行规则。
4. 每读一条 `tick`，先独立推进一 tick，再做结构比较。
5. 每读一条 `snapshot`，比较完整状态。
6. 页脚比较最终 tick、最终状态和关键指标。

这里的规则实现与引擎分开，录制器完全未参与。比较由通用 `first_difference()` 输出稳定字段路径；由于正文字段遍历按 key 排序，定位结果可重复。

## 8. 检查点协议

检查点 envelope：

```text
kind = tickarena.checkpoint
format_version = 1
engine_tick
payload
```

payload 关键字段：

- `initial_fingerprint`
- `event_fingerprint`
- `replay_format_version`
- `limits`
- `state`
- `event.index`, `event.last_seen_tick`
- `replay.progress`
- `replay_finalized`

提交时按规范化路径获取进程级 `RLock`，读取磁盘上的最后 tick；不大于它则返回 `E_ORDER`。临时文件写完后 `os.replace`，因此不会出现半个 JSON 快照。Windows 上遇到 Defender/索引器造成的短暂共享锁时，只对同一个原子替换做有界重试，不使用“先删目标再改名”的非原子回退。

## 9. 并发模型

- 一个 `Engine` 是单持有者/单线程对象。
- 不同对局使用不同 Engine，可在线程池并行。
- `BoundedParallel` 同时限制 `max_workers` 与 `queue_limit`。
- 同一路径的检查点写由 `CheckpointStore` 串行化，并用 tick 单调性拒绝旧状态覆盖新状态。
- 回放记录自身要求 tick 顺序连续；引擎单线程追加。

## 10. 资源边界

- 文件事件源只保存每行偏移，正文事件按 chunk 解析。
- 单 tick 事件 chunk 上限为 `event_buffer_limit`；超额事件拒绝为 `E_BUFFER`。
- 批推进外层最多 `max_batch_ticks`，超过返回 `E_ARG`。
- 每次推进前用状态规模 + pending 事件 + 缓冲预算估算内存，超过 `memory_limit_bytes` 返回 `E_MEMORY`。
- 回放写入和校验均流式处理；校验只保存当前状态和当前记录。
- 100,000 tick 示例的回放约 46 MB，校验未将 46 MB 回放整体载入内存。

