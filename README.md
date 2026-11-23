# PadWeave Studio

面向跨平台输入与手柄映射的确定性桌面实验工具。项目从本地生成的设备样本出发，把设备布局、轴响应、绑定规则和逻辑时间事件流构建成可缓存依赖图，输出动作状态流，并提供增量重算、全量一致性比对、原子快照、设备热切换和 PySide6 仪表盘。

当前验收结果：**12/12 端到端检查通过，38/38 单元与集成测试通过**。详细机器可读结果见 [`artifacts/validation_report.json`](artifacts/validation_report.json)，摘要见 [`artifacts/validation_summary.md`](artifacts/validation_summary.md)。

## 实际环境

- Python：`3.14.4`
- NumPy：`2.5.5`
- PySide6：`6.12.0`
- 虚拟环境：`.venv/`
- 桌面后端：实际 `DISPLAY=:0` 启动成功并以退出码 0 关闭；自动化截图使用 Qt `offscreen` 后端
- 不使用 Docker 或其他容器，不连接真实手柄，不访问未授权目标

本工作区的系统 Python 没有 `ensurepip`，因此虚拟环境使用本地 `virtualenv` 引导后执行：

```bash
python -m pip install numpy PySide6
python -m pip install -e .
```

若目标机器具备标准 `venv`：

```bash
./scripts/bootstrap.sh
. .venv/bin/activate
padweave app --sample samples/dual_stick
```

## 快速使用

```bash
# 生成三套确定性样本
.venv/bin/padweave generate --out samples

# 校验样本并比对真值
.venv/bin/padweave validate --sample samples/dual_stick

# 全量重放并输出动作帧
.venv/bin/padweave replay --sample samples/dual_stick \
  --output artifacts/dual_stick_frames.json

# 修改死区，按依赖图增量重算，并自动和空缓存全量重放比对
.venv/bin/padweave incremental --sample samples/dual_stick \
  --axis left_x --deadzone 0.20

# 快照写入、加载
.venv/bin/padweave snapshot --sample samples/dual_stick \
  --path artifacts/snapshots/state.json --action write
.venv/bin/padweave snapshot --sample samples/dual_stick \
  --path artifacts/snapshots/state.json --action load

# 热切换与冲突演示
.venv/bin/padweave switch --source samples/dual_stick \
  --target samples/hybrid_trigger_gyro --at 2000
.venv/bin/padweave switch-conflict --source samples/dual_stick \
  --target samples/hybrid_trigger_gyro

# 启动桌面应用
.venv/bin/padweave app --sample samples/dual_stick

# 全部验收检查，包含 UI 冒烟渲染
.venv/bin/padweave selfcheck --samples samples --artifacts artifacts

# 测试
PYTHONPATH=src .venv/bin/python -m unittest discover -s tests -t . -v
```

`scripts/run_validation.sh` 会依次执行单元测试、自检和 UI 证据截图：

```bash
./scripts/run_validation.sh
```

## 确定性样本

生成器位于 `src/gamepad_studio/samples.py`，不使用随机数或系统时间，输出稳定：

| 设备 | 轴 | 按键 | 规则 | 事件 | 真值检查 |
|---|---:|---:|---:|---:|---:|
| `dual_stick` / Atlas Dual Stick | 7 | 8 | 8 | 47 | 46 |
| `single_stick` / Nano Single Stick | 3 | 3 | 6 | 16 | 14 |
| `hybrid_trigger_gyro` / Vector Trigger + Gyro | 6 | 5 | 8 | 28 | 27 |

样本覆盖：

- 双摇杆、单摇杆、双扳机与陀螺仪变体
- 反向轴、不同死区、扳机递增/递减方向、不同轴范围
- 按钮按下/释放与 `repeat=true` 自动连发重触发
- 组合键窗口内成功与窗口外过期
- 长按在阈值边界精确触发、阈值前释放不触发
- 双击窗口内成功、窗口过期、双击释放恰好落在闭区间边界
- 摇杆 power 曲线、触发轴与真值区间

目录格式：

```text
samples/
  index.json
  dual_stick/
    device.json
    bindings.json
    events.json
    truth.json
```

### 设备描述

```json
{
  "schema_version": 1,
  "id": "dual_stick",
  "name": "Atlas Dual Stick",
  "axes": [
    {
      "id": "left_x",
      "kind": "stick",
      "minimum": -1.0,
      "maximum": 1.0,
      "deadzone": 0.12,
      "inverted": false,
      "trigger_direction": "increasing",
      "curve": {"kind": "power", "exponent": 1.5, "points": []}
    }
  ],
  "buttons": ["a", "b", "x", "y", "lb", "rb", "start", "back"]
}
```

轴 `kind` 支持 `stick`、`trigger`、`gyro`。双极轴以 `max(abs(minimum), abs(maximum))` 归一化，单极轴以声明范围归一化。扳机轴必须为单极。

### 绑定表

规则类型：

- `button`：单按钮动作，按下/保持/释放；自动连发会增加重触发次数
- `combo`：所有成员按钮在 `window_ms` 内同时按下
- `long_press`：单按钮持续到 `hold_ms`
- `double_press`：单按钮在 `window_ms` 内完成两次完整按下/释放
- `axis`：某个轴经有效曲线映射为动作值；规则曲线覆盖设备轴默认曲线

规则 ID 与动作 ID 必须唯一，避免动作输出状态歧义。

### 事件与真值

```json
{
  "schema_version": 1,
  "device_id": "dual_stick",
  "events": [
    {"seq": 0, "t_ms": 0, "type": "axis", "axis": "left_x", "value": 0.0},
    {"seq": 1, "t_ms": 160, "type": "button", "button": "a", "pressed": true, "repeat": false}
  ]
}
```

`seq` 必须严格递增，`t_ms` 必须单调不减；相同逻辑时刻允许存在多条事件，并按 `seq` 顺序处理。

真值清单包含：

- `axis_samples`：指定时刻轴归一值闭区间
- `action_samples`：指定时刻动作按下态、触发次数、重触发次数和原因
- `trigger_samples`：动作触发时刻、来源事件序号和触发序号

## 错误码

| 错误码 | 触发场景 | 定位信息 |
|---|---|---|
| `E_SCHEMA` | JSON 字段缺失/类型错误、规则或动作 ID 重复、按钮事件引用非法 | `field`, `event_seq` |
| `E_AXIS` | 轴 ID 重复、未知轴/按键引用、轴值超出声明范围 | `field`, `event_seq`, `context.axis` |
| `E_ORDER` | 事件 `seq` 不递增或 `t_ms` 倒置 | `field`, `event_seq` |
| `E_PARA` | 死区、曲线、组合/双击窗口或长按阈值越界 | `field`, `context` |
| `E_MAP` | 热切换后规则引用目标设备不存在的轴/按键 | `context.source_device`, `target_device`, `axis_id`, `conflicts` |
| `E_DIVERGE` | 增量结果与空缓存全量重放不一致 | 首个 `event_seq`, `action`, 差异字段 |
| `E_CONCURRENCY` | 单线程事件缓冲超过硬上限 | `capacity` |

## 重放语义

核心模块：`src/gamepad_studio/replay.py`

1. 按逻辑时间推进输入事件；同一时刻按 `seq` 处理。
2. 轴处理顺序固定为：声明范围归一化 → 扳机方向 → 死区剔除 → 轴向反转 → 响应曲线。
3. 死区边界为闭区间：`abs(value) <= deadzone` 时归零；非零时再线性重映射到剩余范围。
4. 单极轴反转使用 `1 - value`，双极轴反转使用 `-value`。
5. 每个动作独立生成确定性 tick 时间线，再由公共事件网格合并为逐帧快照。

每个 `Frame` 包含：

- `frame_index`, `t_ms`, `source_event_seq`
- `tick_kind`: `event`, `long_deadline`, `window_deadline`
- 当前所有轴归一值 `axis_values`
- 当前所有动作快照 `actions`

动作快照包含 `pressed`, `active`, `value`, `trigger_count`, `repeat_count`, `trigger_t_ms`, `trigger_seq`, `last_reason`, `triggered_now`。

### 时间边界规则

- **长按边界闭区间**：按下后 `t = press_t + hold_ms` 触发；若在同一时刻收到释放事件，截止计时 tick 先于释放执行，因此仍触发。
- **组合/双击边界闭区间**：完成事件若恰好发生在窗口截止时刻，先处理事件，因此可以成立；截止计时 tick 在同时刻事件之后才判定过期。
- **组合过期后的阻塞**：窗口过期或取消后，在仍有成员按钮按住时不会立即重新武装；必须等所有成员释放后，新的成员按下才开启新窗口。这样“第一个按钮一直按住、第二个按钮很久后才按”不会被误判为组合。
- **双击**：窗口从第一次按下开始计时，第二次释放必须不晚于 `first_press_t + window_ms` 才能触发。
- **自动连发**：`repeat=true` 只对 `button` 规则产生重触发，增加 `repeat_count` 和 `trigger_count`，更新最后触发时刻/序号；组合、长按和双击不把 repeat 当作新的按下边沿。

## 映射管线与增量缓存

核心模块：`src/gamepad_studio/pipeline.py`

节点类型：

```text
device:<id>
axis:<axis_id>             <- device
timing:<rule_id>
rule:<rule_id>             <- device, timing, 相关 axis 节点
action:<action_id>         <- rule
output                     <- 所有 action 和 axis
```

缓存键：

```text
sha256(
  algorithm_version,
  input_event_content_hash,
  node_id,
  node_parameter_fingerprint,
  sorted(upstream_cache_keys)
)
```

设备结构节点只包含设备 ID、轴 ID/类型/范围与按键布局。单个轴的死区、反向、扳机方向和曲线属于该 `axis` 节点参数，因此修改 `left_x.deadzone` 不会误失效 `left_y` 或其他设备的轴节点。

失效规则：

- 参数指纹变化：节点及全部下游失效
- 规则依赖集合变化：依赖图变化节点及下游失效
- 上游 cache key 变化：下游失效
- 手动重试：指定节点及下游失效
- 未失效节点直接复用原缓存对象，不重跑映射逻辑

事件窗口按 `seq // 32` 划分为 `w0`, `w1`, ...。轴节点只扫描包含该轴事件的时间窗；规则节点只扫描包含该规则输入事件的时间窗。失效清单给出节点的 `window_ids`。

首次运行与每次增量都按拓扑序计算；所有脏节点成功后才对整个缓存字典做一次原子替换。失败时保留上一份完整缓存。

### 已实测增量结果

修改 `dual_stick.left_x` 死区：

```text
recomputed: 4
reused: 29
invalidated:
  axis:left_x        parameter_changed:deadzone       w0, w1
  rule:move_x        upstream_invalidated:axis:left_x w0, w1
  action:move_x      upstream_invalidated:rule:move_x w0, w1
  output             upstream_invalidated             —
```

修改 `move_x` 规则曲线：只重算 `rule:move_x`, `action:move_x`, `output`，30 个节点复用，所有轴节点复用。

修改 `sprint.window_ms`：只重算 `timing:sprint`, `rule:sprint`, `action:sprint`, `output`，29 个节点复用。

三种变更后的增量输出均与“空缓存重新全量重放”逐帧相等，比较内容包括：

- 帧时间、来源事件序号、tick 类型
- 全部轴归一值
- 全部动作按下态、值、触发次数、重触发次数、触发时刻、触发序号、原因

不一致时会生成 `DiffReport`，指出首个帧、事件序号、动作 ID 和差异字段，并抛出 `E_DIVERGE`；不会修正任何一侧结果。

## 原子快照

核心模块：`src/gamepad_studio/snapshot.py`

快照包含：

- 格式版本、算法版本
- 输入事件内容哈希
- 绑定规则版本
- 图签名、参数版本
- 每个节点的 cache key、参数指纹、上游 key、窗口 ID 和值

加载校验要求以下内容全部匹配才允许使用快照：

- `input_hash`
- `binding_version`
- `algorithm_version`
- `graph_signature`
- `param_version`
- 节点集合、节点类型、参数指纹和完整上游 cache key 链

任一不匹配则整份快照作废，不返回部分节点缓存，随后全量重放。快照整体替换时会返回版本变化字段和被作废的旧节点数量。

写入流程：

1. 进程内 `RLock`
2. 跨进程 `fcntl.flock`（可用平台）
3. 同目录临时文件写入、`flush`、`fsync`
4. `os.replace` 原子替换
5. 目录 `fsync`

因此正常写入不会与其他写入交错。若进程被强制终止，可能留下临时文件，但正式快照仍是旧的完整版本或新写入完成后的完整版本；工具不会声称强制终止后仍能生成完整诊断报告。

实测：

- 合法快照恢复 33/33 节点，重算 0，结果哈希一致
- 输入/绑定/算法任一版本不匹配，整份 33 节点作废
- 6 次并发快照写入后仍可完整加载，无 `.tmp` 残留

## 设备热切换

核心模块：`src/gamepad_studio/session.py`

- 源设备在切换时刻及之前的帧构成不可变已提交前缀，并计算 `prefix_hash`
- 目标设备在切换时刻之后的帧来自独立目标管线
- 目标后缀与直接加载目标设备的对应重放结果逐帧比较
- 目标设备后续增量更新不会改写源前缀
- 未出现在目标设备或未被目标绑定使用的物理输入写入 `unmapped` 清单
- 如果绑定规则引用目标设备不存在的轴/按键，抛 `E_MAP`，上下文包含源设备、目标设备、轴 ID 和全部冲突

实测 `dual_stick -> hybrid_trigger_gyro @ 2000ms`：

- 源前缀 33 帧，目标后缀 2 帧
- 前缀哈希在目标更新前后保持不变
- 目标后缀与直接目标重放一致
- 10 个未映射物理输入被显式列出
- 用源设备绑定强行迁移时返回 `E_MAP`，冲突轴包括 `left_x`, `right_y`

## 并发边界

- 桌面 UI 使用 `QThreadPool`，最大线程数固定为 2
- 每次重放使用 `max_event_buffer=128`（UI）或 256（核心默认）
- `BoundedEventBuffer` 对缓冲容量做硬限制，超限返回 `E_CONCURRENCY`
- `ReplayCoordinator` 最大并行度限制在 1–4
- 并行结果按任务索引收集，不以线程结束顺序决定结果
- 3 路并行重放结果哈希完全一致
- 快照写入有独立线程锁和跨进程文件锁

## 桌面界面

启动：

```bash
.venv/bin/padweave app --sample samples/dual_stick
```

界面并排包含：

- 设备布局示意（摇杆、死区、扳机、陀螺仪、按键）
- 绑定规则表与规则编辑对话框
- 逻辑事件时间轴与确定性 deadline 标记
- 动作输出波形、触发/重触发标记
- 最新动作状态表
- 失效清单
- 依赖传播图，橙色为本次重算、绿色为复用
- 增量/全量差异热力图与上一版本/当前版本变化热力图
- 节点缓存检查器与手动重试
- 快照、热切换和验证报告页

可交互调整轴死区、曲线指数、反向状态和绑定规则；更新在工作线程执行，旧任务结果不会覆盖新代结果。

截图：

- [`artifacts/ui_dashboard.png`](artifacts/ui_dashboard.png)
- [`artifacts/ui_incremental.png`](artifacts/ui_incremental.png)
- [`artifacts/ui_dependency.png`](artifacts/ui_dependency.png)
- [`artifacts/ui_diff_heatmap.png`](artifacts/ui_diff_heatmap.png)

重新生成：

```bash
.venv/bin/python scripts/capture_ui_demo.py
```

## 验证结果

命令：

```bash
PYTHONPATH=src .venv/bin/python -m unittest discover -s tests -t . -v
.venv/bin/padweave selfcheck --samples samples --artifacts artifacts
```

结果：

- `38` 个单元/集成测试通过
- `12` 个端到端自检通过
- 三套设备真值检查 `46 + 14 + 27 = 87` 项全部通过
- 映射重放、增量与全量逐帧一致
- 失效传播、节点复用与 affected windows 可观测
- 快照合法恢复和版本失效行为通过
- 热切换前缀冻结、未映射清单、`E_MAP` 和直接目标一致性通过
- 并行确定性通过
- PySide6 仪表盘 smoke 渲染和实际桌面后端启动通过

机器可读报告：[`artifacts/validation_report.json`](artifacts/validation_report.json)

## 项目结构

```text
src/gamepad_studio/
  curves.py          轴归一化、死区、反向、曲线
  models.py          设备、规则、事件、真值、动作快照模型
  validation.py      样本加载与结构化校验
  samples.py         确定性样本生成器
  replay.py          逻辑时间重放、时序判定、真值比对
  pipeline.py        依赖图、cache key、失效调度、原子缓存替换
  compare.py         逐帧一致性与差异热力图
  snapshot.py        原子快照与版本校验
  session.py         设备热切换
  concurrency.py     有界并行重放与串行快照写
  service.py         CLI/UI 共用应用服务
  selfcheck.py       端到端验收
  cli.py             命令行入口
  app.py             PySide6 应用入口
  ui/                主题、自定义绘图控件、主窗口
tests/               38 个单元与集成测试
scripts/             环境、验收、UI 截图脚本
samples/             生成后的确定性样本与真值
artifacts/           验证报告与 UI 截图
```

## 已知边界

- 逻辑时间使用整数毫秒；没有墙钟时间参与结果。
- `output` 节点需要按事件时间线重新组装帧，复杂度为 O(n)，但不会重跑未失效的 axis/rule 映射节点。
- 事件窗口缓存以输入事件序号稳定划分；窗口大小改变会使输出节点失效并重新组装。
- 未接入真实手柄或未经授权目标；输入只来自本地确定性样本。
- 快照采用单文件原子替换而非 WAL；强杀后不承诺持久化中间进度，只保证不混用部分旧缓存。
