# 因果演算内核技术说明

## 1. 分层结构

| 模块 | 职责 |
|---|---|
| `causalkernel/scenario.py` | 解析 JSON、标记系数、构建事件图、全图校验 |
| `causalkernel/precision.py` | 定点量化、舍入方向、binary64 单位舍入误差 |
| `causalkernel/engine.py` | 条件求值链、区间传播、归因、效果、推进、轨迹、快照 |
| `causalkernel/injection.py` | 仅在剧本加载入口注入五类故障 |
| `causalkernel/comparison.py` | 定点/浮点差异和逐条件归因报告 |
| `causalkernel/samples.py` | 确定性样本生成 |
| `causalkernel/experiment.py` | 脚本化验收与结果落盘 |
| `causalkernel/cli.py` | `generate/run/compare/replay/experiment` 命令 |

推进入口在任何执行前完成整图校验：事件 ID 唯一、起点存在、所有分支目标存在、分支条件引用存在、前置条件引用存在、效果路径存在且类型匹配、表达式深度与节点数受限、系数有限、事件图无环且深度不超过 64。

## 2. 推进语义

一个决策步包含：

1. 当前事件的所有出边条件全部求值，记录表达式、模式、阈值、点值、上下界、真值上下界和完整求值链。
2. 目标事件的前置条件求值。
3. 分支按优先级降序、声明顺序升序稳定排序。
4. 选择第一条满足条件且目标前置条件为真的分支；若提供操作序列，则优先选择同 ID 分支，否则记录未匹配并回退到稳定排序第一条。
5. 触发目标事件，按声明顺序执行效果。
6. 记录状态修改前后、效果链、状态哈希、模式、量化参数和内容哈希。

条件不确定时推进仍使用当前模式下的点值真值，但必须设置 `uncertain_branch=true` 并完成归因；不会静默改选分支或切换模式。

起始事件作为第 0 个 `entry` 决策步触发。终端事件没有出边，推进正常终止。达到 `max_steps` 时以 `max_steps` 终止。

## 3. 定点误差模型

固定点模式的量子为：

```text
q = 10^(-decimal_places)
round(x) = quantize(x, q, rounding_mode)
E_q(x) = |round(x) - x|
```

支持 `half_even` 与 `half_up`。每个量化节点记录 `up`、`down` 或 `none` 舍入方向。

数值节点保存三元组 `(point, lower, upper)`：

- 常量：区间覆盖原始十进制值与量化值。
- 状态变量：区间继承此前所有效果累积的误差。
- 加减：区间端点相加或相减，再扩展本次量化误差。
- 乘法：取四个端点积的最小/最大值，再扩展本次量化误差。
- 指数：展开为逐次量化乘法节点，例如 `x^3` 产生 `x*x` 与 `(x*x)*x` 两个节点，因此指数放大在节点级可见。

因此条件记录同时给出点值、`[lower, upper]`、`cumulative_error_bound = max(point-lower, upper-point)`、真值下界与真值上界。阈值也独立记录量化值、方向和量化误差。

## 4. 浮点模式

浮点模式显式使用 `numpy.float64`：

- 点值：binary64。
- 单次运算误差界：`u * scale`，其中 `u = eps/2`，`scale` 是该运算的输入量级上界。
- 不进行十进制定点舍入。

模式来自显式 `Precision` 对象。推进过程中不会因数值异常或比较失败切换模式；轨迹与快照都写入完整模式元数据。

## 5. 逐条件归因

求值链是一个后序 AST 节点列表。归因过程为：

1. 从根节点向前反传敏感度。加法/减法传递 `1`，乘法传递另一个操作数的绝对值，指数乘法自然累积放大系数。
2. 对每个有局部误差的节点计算 `contribution = local_error * sensitivity`。
3. 当条件区间跨过阈值时，以点值到阈值的距离 `margin` 为目标。
4. 按求值链顺序累计贡献；累计贡献首次达到 `margin` 的节点即“最早越界因子”。
5. 若区间宽度由高阶项导致而一阶总贡献不足，用区间宽度对贡献做保守缩放；若仍没有可用误差因子，返回 `E_ATTRIBUTION_FAILED`。

报告保留完整因子表和放大链，不只给出总误差。

## 6. 轨迹与哈希

每个决策步记录以下内容并计算 `record_hash`：

```text
record_hash = SHA256(canonical_json(step_without_record_hash))
```

其中 `step` 包含 `prev_hash`，首步为字符串 `GENESIS`。轨迹级哈希：

```text
trace_hash = SHA256(canonical_json(trace_without_trace_hash))
```

规范化 JSON 使用排序键、紧凑分隔符、UTF-8，并禁止 NaN/Infinity。定点数编码为规范十进制字符串；浮点数编码为 Python `float.hex()` 与十进制可读值，避免平台格式化差异。

复放时依次验证轨迹自哈希、逐步因果链、场景哈希、重新推进后的每一步结构、终局状态和终局哈希。任何不一致都抛出 `E_TRACE_INVALID`，且不返回推进结果。

## 7. 快照与恢复

每个决策步调用 `atomic_write_json`：

1. 在目标目录创建真实临时文件。
2. 写入、`flush`、`fsync`。
3. `os.replace` 原子重命名。
4. 对目录句柄 `fsync`。

快照包含完整状态区间、当前事件、已记录步骤、因果前缀哈希、模式、量化参数、操作序列和快照自哈希。恢复时选取编号最大的快照并验证：

- 格式与版本。
- 快照自哈希。
- 场景哈希、模式、操作序列和 `max_steps`。
- 每一步 `prev_hash` 与 `record_hash`。
- 状态完整哈希与链尾哈希。

实验通过子进程在第 3 个快照后执行 `os._exit(17)`，再以 `--recover` 继续；恢复轨迹与不中断轨迹逐字节一致，且不会重复前缀步骤。版本篡改和状态篡改分别返回 `E_RECOVERY_INVALID`。

## 8. 注入边界

注入只发生在 `load_scenario()` 的文档加载阶段，实际流程为：

```text
read bytes -> optional injected mutation -> JSON parse -> graph build/validation -> engine
```

求值器和推进器不包含注入分支。五类注入在写轨迹或快照前失败，并携带 `json_path`、事件或条件 ID、深度/规模、字节偏移等定位信息。

## 9. 确定性约束

- 样本生成仅使用固定整数种子和 `random.Random`。
- 不使用时间戳、PID、随机 UUID 或平台浮点格式化。
- `numpy.float64` 仅用于算术，序列化使用十六进制表示。
- 所有映射按键排序后写 JSON。
- 分支排序使用优先级和剧本声明索引，不依赖字典迭代或并发调度。

当前验收结果见 `reports/RESULTS.md` 与 `reports/validation_report.json`。
