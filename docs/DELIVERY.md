# 离线命中判定复算与命中归因内核

本交付在 WSL2 Ubuntu 26.04 上验证，环境自带 `go1.22.12 linux/amd64`，无需 Docker。所有样本均由固定种子 `428691` 的本地确定性构造器生成；SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.29.10`。

## 快速运行

```bash
go version                 # 实测 go1.22.12 linux/amd64
go mod download
go run ./cmd/samplegen -out samples
go test -timeout 120s ./...
go run ./cmd/judge -once -work .diagnostic -samples samples
go run ./cmd/judge -addr 127.0.0.1:8099 -work .diagnostic -samples samples
# 浏览器打开 http://127.0.0.1:8099/
```

`cmd/judge -once` 会执行实时、断点恢复、全量、编辑后增量与全新全量路径，把诊断数据写入 `.diagnostic/dashboard.json`；任一比对不一致时以退出码 2 结束。

## 目录与职责

- `internal/model`：统一内部模型、错误码、转换痕迹、损失清单和真值结构。
- `internal/conv`：带溢出检查的整数加减/乘除、帧↔毫秒、定点刻度、绝对坐标↔增量换算。
- `internal/schema`：`battleformat-v1/v2/v3` DTO、加载校验、默认补齐、显式升降级导出和 `E_LOSSY` 清单。
- `internal/geom`：AABB 与朝向胶囊体的扫掠段求交、首触时间二分、首触面/帽/杆部位分类和最近距离。
- `internal/engine`：逐帧推进、穿透/多段结算、帧区间依赖图、链式缓存键、有界 worker pool、失败隔离和三路径比对。
- `internal/store`：SQLite 快照代际、区间原子检查点、内容寻址区间缓存和串行提交日志。
- `internal/sample`：固定种子样本工程与解析构造真值。
- `internal/web`：本地诊断页面和 `/api/dashboard` JSON。
- `cmd/samplegen`：生成 `samples/project.v1.json`、`project.v2.json`、`project.v3.json`、`truth.json`。
- `cmd/judge`：自检与诊断页面服务。

## 三版本口径

| 版本 | 字段风格 | 时间 | 刻度 | 端点/朝向 | 可选字段 |
| --- | --- | --- | --- | --- | --- |
| `battleformat-v1` | PascalCase，如 `Schema`、`Colliders[].No` | 逻辑帧 | 千分比 `scale=1000` | 绝对 `StartPosQ/EndPosQ`、胶囊 `CapStart/CapEnd` | 无穿透/多段字段，加载时补声明默认 |
| `battleformat-v2` | snake_case，如 `collider_no`、`start_ms` | 毫秒，帧 1 对应 0ms | 万分比 `scale=10000` | `origin_q + advance_delta_q`，胶囊 `center_q ± half_axis_delta_q` | 有 `penetrate/decay_myriad`，无多段策略 |
| `battleformat-v3` | camelCase，如 `colliderNo`、`startFrame` | 逻辑帧 + `frameStepMs` | 内部 Q 刻度 `1e6` | 绝对 `fromQ/toQ`、胶囊 `p0/p1` | 全量穿透、衰减、多段、段上限、重复命中策略 |

默认结算策略为：允许穿透、每命中一段保留 50% 伤害、允许多段、最多 12 段、同一碰撞体在一次扫掠中只命中一次。每次补齐都会写入 `model.Trace`，包含源版本、字段路径、旧值 `<absent>`、新值/默认值、来源和 `transform-rules-2026-11-26` 规则版本。

导出低版本时返回 `[]model.LossNotice`，每项的 `code` 为 `E_LOSSY`，并给出字段路径、原值和处置方式，例如：

- 增量端点：`flatten_incremental_endpoint_to_absolute_start_end`
- 穿透字段：`omit_fields_and_apply_v1_declared_default_policy`
- 多段字段：`omit_multi_segment_fields_and_apply_v2_defaults`
- 刻度余数：`floor_quantize_and_retain_residual_trace`

## 几何与结算

内部坐标、半径和距离使用 Q=`1,000,000` 的整数单位；帧位置每帧都从起点、整数分子/分母和 checked arithmetic 直接计算，不累积浮点误差。几何求解中仅用 `float64` 作为临时解析计算，并把 TOI、位置和距离重新截断/量化为整数记录。

每个逻辑帧以上一帧位置到当前位置的胶囊段作为该帧扫掠体：

- AABB：候选面平面、端点 clamp、12 条边的段-段最近点联合求解。
- 朝向胶囊：两段中心线的候选内部解与边界解，再减去武器半径和胶囊半径。
- 首触：从已知相交参数向 t=0 做 48 次确定性二分，在首触点 clamp/投影并分类 `aabb:face:*`、`aabb:edge:*`、`aabb:corner`、`capsule:shaft/cap_start/cap_end`。

样本主扫掠 `sw_primary` 的真值为：

- F2：未命中，到 `box_front` 的表面距离 `500000` Q。
- F3：首触 `#1 box_front/torso/aabb:face:-X`，穿透后伤害 `20000000`。
- F5：首触 `#2 capsule_pylon/pylon_shaft/capsule:shaft`，穿透后伤害 `10000000`。
- F6：首触 `#3 box_rear/rear_core/aabb:face:-X`，穿透后伤害 `5000000`。

耗尽扫掠 `sw_exhaust` 构造 11 个门形碰撞体。初始伤害 `1000` Q、保留比例 50%，前 10 段伤害依次为 `500,250,125,62,31,15,7,3,1,0`，第 11 段状态为 `depleted` 且原因是 `damage_exhausted_before_segment`，不会静默丢弃。

## 依赖图、缓存与 SQLite

默认每 2 帧一个区间。区间缓存键输入包含：

- `model.AlgoVersion`
- 整数时间基准版本和 `frameStepMs`
- 区间实际粗筛到的碰撞体内容哈希
- 本区间请求的 sweep 完整内容哈希（端点、半径、帧范围、伤害、穿透策略）
- 排序后的显式依赖边
- 前一区间缓存键；I1 使用 `GenesisKey`

编辑 `#3` 的 `min.x` 从 `9.0` 到 `8.5` 时，失效清单包含：

- I3 `intersection: collider_size_changed collider_no=3 field=min.x`
- I4 `downstream_settlement: upstream_cache_key_changed_after_interval_3`

实测增量运行在新快照代际中复用 I1、I2 的持久化内容寻址结果，重算 I3、I4，并与编辑后全新全量报告逐帧一致。

SQLite 表：

- `snapshots`：逻辑快照、代际、全局身份、算法/时间基准/参数版本和作废原因。
- `interval_checkpoints`：每代际每区间的 payload、缓存键、内容哈希和三类版本。
- `interval_cache`：按缓存键保存的内容寻址结果，快照代际作废后仍可安全复用。
- `commit_log`：串行提交顺序。

`Store` 使用 `SetMaxOpenConns(1)` 加互斥锁；每个区间的 checkpoint、cache 和 commit_log 在同一个事务中提交。重启时若全局身份、算法版本、时间基准版本或参数版本不匹配，会把旧代际标记为 `void` 并开启新代际；当前缓存键命中的区间从 `interval_cache` 续算/复用，损坏或哈希不符的缓存项会删除并重算。

## 并行与预算

`engine.Pool` 支持：

- `MaxParallel`：worker 数。
- `MaxQueuedIntervals`：有界 channel 容量；满时提交方阻塞，并记录区间、`queue_budget_exhausted` 和等待毫秒。
- `GeometryBufferBytes`：每个 worker 独立几何缓冲上限；超出返回 `E_BUDGET`。
- 几何求解任务并行；结算任务按区间状态链顺序提交到同一 pool 并等待，保证伤害、段号和去重集合确定。

故障注入链路：

- `Injection.CorruptColliderNo`：仅在区间粗筛实际装载该碰撞体时返回 `E_DECODE`；已提交的前序区间保留，清理后续 future 后可用无注入参数重试。
- `Injection.UseBadStep + BadStepMS<=0`：`E_STEP`，字段路径 `$.frameStepMs`。
- `Injection.UnitContradiction` 或 V1 写入 `UnitScale=10000`：`E_UNIT`。
- 整数溢出：`E_OVF`，错误包含 `field_path/op1/op2`。

## 三路径比对

`engine.CompareReports(want, got)` 按 `(frame,sweep_id)` 排序后比较：

- `first_collider_no`
- `first_part`
- `first_surface`
- `remaining_damage_q`
- `nearest_distance_q`
- 完整段序列 JSON
- `commit_sequence` 与物理 `write_order`

不一致时返回首个帧号、sweep 和字段名。验收测试覆盖实时（parallel=1）、断点重启恢复（先提交 I1/I2）、4 worker 全量、编辑后增量与编辑后全量，结果全部一致。

## 实测结果

详细逐测试输出保存在 `docs/verification-output.txt`。最终摘要：

```text
go version go1.22.12 linux/amd64
modernc.org/sqlite v1.29.10

go test -timeout 120s ./...
ok  combatjudge
ok  combatjudge/internal/engine
ok  combatjudge/internal/store
ok  combatjudge/internal/web

go test -race -timeout 180s ./...
ok  combatjudge
ok  combatjudge/internal/engine
ok  combatjudge/internal/store
ok  combatjudge/internal/web

go run ./cmd/judge -once -work .diagnostic -samples samples
compare: live=true resume=true incrementalMatch=true
```

本地 HTTP 实测 `/` 返回诊断 HTML，`/api/dashboard` 返回 JSON，其中包含 project、truth、三路径 report、invalidations、queue waits 和版本损失清单。
