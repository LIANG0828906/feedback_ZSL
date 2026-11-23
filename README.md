# levelsession — 面向关卡设计的程序化生成编辑会话组件

纯 Go 1.22+ 标准库实现，无外部依赖、无容器、无网络服务。把玩家的连续编辑操作
作为首要输入：维护可变约束状态，按操作做增量失效与重生成，提供 100ms 内粗略
预览与停止编辑后的精确重生成双路径，并保证增量编辑、快照回退重放、一次性全量
生成三条路径逐字节一致。

## 快速开始

```bash
go version          # 需要 go1.22+
go test ./...       # 全部单元测试（异常码 + 一致性 + 恢复边界）
scripts/run_example.sh        # Linux/macOS 一键示例
scripts\run_example.ps1       # Windows 一键示例
```

或手动执行：

```bash
# 1. 生成确定性样本（固定种子，重复生成逐字节相同）
go run ./cmd/levelsession sample -scenario basic -out samples/basic

# 2. 加载 -> 编辑增量生成 -> 预览/精确比对 -> 快照提交 -> 三路径比对
go run ./cmd/levelsession run -sample samples/basic -store artifacts/store -report artifacts/report

# 3. 重启后从已持久化提交点继续
go run ./cmd/levelsession resume -sample samples/basic -store artifacts/store
```

## 样本规模与生成命令

| 项 | 值 |
|---|---|
| 生成命令 | `go run ./cmd/levelsession sample -scenario <name> -out DIR` |
| 固定种子 | `0x5EED0042`（`internal/level/samplegen.go` 的 `SampleSeed`） |
| 地图尺寸 | 256 x 256 |
| 分区网格 | 4 x 4 = 16 个分区 |
| 编辑操作 | 12 条（覆盖全部 5 种操作类型，逻辑时间戳 100..1200） |
| 细分密度 | 精确 32 x 32，预览 8 x 8，障碍/资源参考网格 16 x 16 |

场景样本：`basic`（主流程）、`budget`（E_BUDGET）、`conn`（边界修通）、
`conn-fail`（E_CONN）、`hist`（E_HIST 时间戳倒退）。样本文件为
`project.json`（约束工程）、`ops.json`（操作序列）、`truth.json`（真值清单）、
`meta.json`（生成命令与规模），重复生成逐字节一致
（`TestSampleFilesDeterministic`）。

## 架构

```
cmd/levelsession/main.go   CLI：sample / run / resume
internal/level/
  types.go      约束工程、操作、分区结果、提交、清单、预览/分歧报告
  rng.go        splitmix64 确定性伪随机源 + 声明式子种子派生规则
  validate.go   加载与校验（E_SCHEMA / E_PARA / E_HIST）
  generate.go   分区高度场/障碍/资源点确定性生成核心
  budget.go     难度预算取舍策略（E_BUDGET）
  connect.go    跨分区连通性判定与边界修通（E_CONN）
  pool.go       有界工作线程池（并行度 + 缓冲上限 + 异常隔离）
  session.go    会话历史模型、增量失效、提交、回退、三路径比对（E_DIFF）
  preview.go    粗略预览 / 精确重生成双路径与分歧检测（E_DIVERGE）
  persist.go    本地原子串行提交存储（tmp+rename，HEAD 指针）
  samplegen.go  固定种子确定性样本生成器 + 真值清单
```

关键设计：

- **子种子派生**：`DeriveSubSeed(globalSeed, row, col, salt, version)`，纯函数；
  生成不读取线程标识、完成顺序或墙钟时间，同一分区在任何工作线程、任何轮次
  产出逐字节一致（`TestDeterministicGeneration` 以 1 线程/1 缓冲 对 8 线程/4
  缓冲交叉验证）。
- **密度无关判定**：障碍、资源点、边界通行标志、难度全部定义在固定 16x16
  参考网格上，预览只降低高度场细分密度，因此预览与精确路径在连通性判定、
  预算占用、资源总量上天然一致，不会判定跳变。
- **修通为纯函数**：会话分别保存"基础生成结果"（base，约束的纯函数）与
  "修通后结果"（parts = repair(base)）。每轮从 base 重新计算修通，保证修通
  结果只依赖当前约束快照，与历史路径无关——这是三路径逐字节一致的核心。
- **会话历史模型**：约束快照为节点（commit）、操作为边。commit 0 为初始状态，
  commit i 对应操作 i。每次提交原子写入 `commit-NNNNNN.json`（tmp+rename）
  后更新 `HEAD`，写入串行且与操作序号一致。
- **增量失效**：`set_budget` / 配额增减 / 子种子变更会使目标分区 + 正交邻接
  分区失效（邻接分区参与连通性重判定）；`set_resource_cap` 仅失效目标分区。
  未受影响分区复用上一轮结果，清单逐分区记录原因与复用状态。
- **强制终止恢复**：HEAD 在 commit 文件落盘后才更新；进程在两者之间被杀只会
  留下被忽略的孤儿 commit 文件。重启时校验指纹链与子种子版本，不匹配则整体
  作废历史并全量重生成。只承诺从已持久化且版本匹配的提交点继续，不承诺强制
  终止后仍生成完整清单。

## 取舍策略（预算约束）

超出预算上限时按可解释顺序取舍，每步记录分区编号、候选标识、理由、取舍后难度：

1. 用最便宜的候补障碍替换最贵的在位障碍（`replace`）；
2. 否则裁剪最贵的在位障碍（`prune`）；
3. 否则裁剪当前占比最高类别的一个资源点（`prune_resource`，类别比例偏差有界）；
4. 低于预算下限时，从候补池补回最贵候选（`add`）。

迭代上限内仍无法满足则返回 `E_BUDGET`，携带当前难度值与预算区间。

## 异常码

| 代码 | 含义 | 定位信息 |
|---|---|---|
| E_SCHEMA | 必填字段缺失 / 未知操作类型 | 字段名 |
| E_PARA | 数值越界、分区越界、资源上限超全局上限 | 字段名 + 分区编号 |
| E_HIST | 操作时间戳倒退 / 序号断裂 / 回退目标不存在 | 操作序号 |
| E_BUDGET | 迭代上限内预算不可满足 | 分区编号 + 当前难度 + 预算区间 |
| E_CONN | 修通后仍不连通 | 不可达目标点标识 + 经过的分区编号 |
| E_DIFF | 三路径比对首个不一致 | 操作序号 / 分区编号 + 数据项名称 |
| E_DIVERGE | 预览与精确结果超容差 | 三项偏差实测值 + 首个分区编号 + 差异项 |

## 验证结果（本仓库实测）

- `go test ./...`：18 项全部通过，覆盖 E_SCHEMA/E_PARA/E_HIST/E_BUDGET/
  E_CONN/E_DIFF/E_DIVERGE、确定性、真值吻合、三路径一致、回退重放、
  强制终止续跑、版本不匹配作废、工作池异常隔离与重试、样本文件确定性。
- `run` 主流程：真值校验 OK；12 个操作预览耗时 0.1–1.2ms（远低于 100ms
  预算）；每操作仅重生成 1–5 个失效分区、复用 11–15 个；三路径逐字节比对 OK。
- 场景样本：`budget` 触发 E_BUDGET（难度 668，预算 [20000,30000]）；
  `conn` 全部边界闭合时 15 处边界修通后连通；`conn-fail` 返回 E_CONN
  （t1 不可达）；`hist` 返回 E_HIST（op_seq=2 时间戳倒退）。
