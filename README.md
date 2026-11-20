# TacticalField：增量代价场、版本化发布与崩溃恢复

本项目是回合制战棋战场的 .NET 8 参考实现，覆盖：

- 多层可通行掩码、可破坏障碍、危险区与单位移动代价。
- 反向 Dijkstra 最小代价场、固定平局顺序、逐格内容哈希与 Merkle 根哈希。
- 局部变更影响集、依赖树失效、增量代价传播与路径缓存迁移。
- 不可变发布快照、查询版本校验、并发查询隔离、批次票号顺序与 `E_BUSY`。
- 临时文件 + 原子重命名的快照、哈希链校验、强制终止恢复。
- 确定性样本、基线哈希、逐字节一致的轨迹日志。
- WinForms 网格/掩码/热力图/路径/变更注入界面。

## 环境与构建

当前工作区实测：

```text
$ DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 dotnet --version
8.0.428

$ dotnet build TacticalField.sln -c Release --nologo
...
Build succeeded.
    0 Warning(s)
    0 Error(s)
```

WSL 环境缺少 ICU，项目显式启用 `InvariantGlobalization`，验证命令也设置
`DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1`。运行时不访问网络或数据库；只需在首次构建时还原
WinForms targeting pack（`EnableWindowsTargeting`）。

Windows Forms 运行时只存在于 Windows。本机通过 WSL 互操作将官方 .NET 8 基础运行时与
Windows Desktop Runtime 8.0.34 安装到 Windows 用户目录：

```text
C:\Users\LIANG\AppData\Local\Microsoft\dotnet8-desktop\
  Microsoft.NETCore.App 8.0.34
  Microsoft.WindowsDesktop.App 8.0.34
```

GUI 已用该 Windows 运行时实际启动并自动关闭验证：

```bash
./scripts/smoke-gui-windows.sh
# GUI SMOKE PASS
```

Windows 用户也可直接运行：

```bat
scripts\run-gui.cmd
```

Linux/WSL 上可完整运行核心、CLI、自包含测试和确定性样本；Windows Desktop 运行时仅用于 GUI。

## 快速验证

```bash
export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1
./scripts/verify.sh
```

脚本会执行 Release 构建、20364 项检查、三个本地样本、轨迹基线和强杀恢复演示。完整输出见
`artifacts/logs/verify.log`，各样本轨迹见：

- `artifacts/logs/small_8x8.log`
- `artifacts/logs/medium_12x10.log`
- `artifacts/logs/large_16x12.log`

## 主要语义

### 格子与单位代价

每个格子包含：

- `BaseCost`：基础移动代价，必须大于 0。
- `PassMask`：整数位掩码；单位掩码与该掩码按位与非零才可进入。
- `Hazard`：危险区标记，进入时附加 `HazardPenalty`（默认 5）。
- `Obstacle`：障碍标记；存在时不可进入。

非障碍且通行掩码匹配时：

```text
weight(cell, unit) = BaseCost + (Hazard ? HazardPenalty : 0)
```

否则 `weight = +∞`。目标格自身代价固定为 0。

### 最小代价场

从目标格反向运行 Dijkstra。松弛顺序固定为几何邻居顺序 `N, W, E, S`，优先队列平局按
格子索引升序。距离确定后，每格按同一固定邻居顺序选择最小后继格，因此相同输入下代价、路径和
哈希完全确定。

### 哈希

- 每格哈希：`SHA256("TF-CELL-V1|index|cost|next")`。
- 字段根：逐格哈希构建二叉 Merkle 树。
- 字段内容哈希：`SHA256` 覆盖尺寸、单位掩码、目标格和 Merkle 根。
- 发布链：`SHA256("TF-CHAIN-V1|prev|batchSeq|version|stateHash")`。

增量更新只为实际改变的叶子生成新哈希，并复用未改变子树的 Merkle 节点；`ReusedCells`
记录未受影响的格数。

## 增量重算

单条或批量变更先构造影子网格与影子单位状态，对每个受影响的 `(unit, target)` 代价场执行：

1. 比较变更前后每格入口权重，得到初始变更格。
2. 沿旧 `next` 的反向依赖树求脏格闭包。
3. 脏格距离重置为 `+∞`；从干净边界格重新向脏区传播。
4. 允许新路径降低脏区外格代价，但不允许旧路径无依据地升高干净格代价。
5. 按固定顺序重建后继指针，得到精确 `ImpactCells`（代价或 `next` 实际变化）。
6. 只更新这些格子的代价、后继和 Merkle 叶子；其余格复用旧值和旧哈希。

`FieldDelta` 同时记录：

- `ImpactCells`：最终实际变化的格。
- `RecomputedCells`：求闭包/松弛所触及的候选格。
- `ReusedCells`：未改变、复用旧代价与旧叶子哈希的格。

随机验证中，每批增量结果都与同状态的完整 Dijkstra 逐格比对代价、`next`、逐格哈希和字段哈希。

## 版本化发布与查询

`BattlefieldView` 是不可变发布快照，包含网格、单位、所有代价场、全局发布版本、批次序号和链哈希。
重算在影子状态完成后整体替换；查询只保存一次 `Current` 引用，因此不会混合两个版本。

查询可携带期望字段版本或内容哈希。任一不匹配返回 `E_FIELD_STALE`。

## 路径缓存

缓存键为：

```text
(unit, target, startCell, fieldVersion, fieldHash)
```

每条缓存记录路径字节、路径哈希、路径依赖格集合和字段版本。发布时：

- 影响集与依赖集相交：删除缓存。
- 影响集不相交：保留原路径字节，迁移到新的字段版本/哈希键，因此仍可命中。
- 字段未变：缓存实例和键都保持不变。

`PathCacheEntry.IsValid` 会重新校验路径链、路径哈希和依赖集。缓存命中与冷重算使用同一个
`TF-PATH-V1` 哈希算法，测试要求两者逐字节相同。

## 并发、批次与错误码

- 发布用不可变引用和 `Volatile.Read/Write` 完成整体替换。
- 提交先领取单调票号，再按票号依次进入发布临界区；同步/异步批次和路径重算均遵守该顺序。
- 同一 `(unit, target)` 用并发预约表互斥；重复重算或冲突批次返回 `E_BUSY`。
- 批次先验证、再在影子结构上计算、最后写快照并发布。任何异常返回 `E_BATCH_ABORTED`，旧版本不受污染。

| 错误码 | 含义 |
| --- | --- |
| `E_FIELD_STALE` | 查询携带的字段版本或哈希与已发布字段不一致 |
| `E_BUSY` | 同一单位/目标已有重算任务或冲突批次 |
| `E_BATCH_ABORTED` | 批次验证、计算或持久化失败，已回滚 |
| `E_RECOVERY_INVALID` | 快照缺失、损坏、断链或字段不完整 |
| `E_UNREACHABLE` | 起点无法到达目标格 |

## 快照与恢复

每次发布将以下内容写成确定性 JSON：

- 网格数组、单位掩码/起点/目标。
- 每个代价场的代价、`next`、逐格哈希、内容哈希、字段版本和批次序号。
- 路径缓存路径、依赖集和路径哈希。
- 全局版本、批次序号、危险区附加代价、完整链历史与文档完整性哈希。

写入流程为同目录临时文件、`Flush(true)`、原子 `File.Move(..., overwrite: true)`。恢复只读取磁盘
快照，并验证：

- 文档 magic/格式版本/完整性哈希。
- 链序号、版本、前驱链哈希和链哈希。
- 最后一环与当前状态哈希一致。
- 每个字段的目标代价、最短路径递推、逐格哈希、Merkle 根和内容哈希。
- 缓存路径链、路径哈希、依赖集和字段版本/哈希。

断链、截断、缺字段或内容篡改均返回 `E_RECOVERY_INVALID`，不会加载半写快照。

## 确定性样本与日志

`SampleGenerator` 首次运行写入 `data/samples/`，并生成 `data/baselines/`。当前三个规模：

| 样本 | 网格 | 轨迹哈希 / 基线 |
| --- | --- | --- |
| `small_8x8` | 8x8 | `84283dfc00844e075155c05c006c5fa7bf2c06f019e6dbbbcb2eeeece0131be3` |
| `medium_12x10` | 12x10 | `efcd399ea5680d9394843bbbed0a486991e6b0e2362d78824c7d94ed47904a50` |
| `large_16x12` | 16x12 | `d77d30a9f740cf0387bae3f44d3912246a0a339c956ad207c560913ff3bb2303` |

轨迹按固定顺序输出变更、影响集、重算格数、缓存命中、字段版本/哈希和最终链哈希；无
时间戳、路径或进程相关信息。三个样本均通过“连续运行两次日志逐字节相等”检查。

## 图形界面

WinForms 界面包含：

- 网格和基础代价、通行掩码、障碍、危险区、不可达格。
- 当前单位/目标代价热力图、S/T 标记和路径高亮。
- 单位、目标、单位掩码选择。
- “当前格立即切换障碍/危险区”、暂存多条变更后统一提交批次、清空批次。
- 任意选中格作为查询起点。
- 发布版本、字段版本、字段哈希、缓存命中、影响格/重算格日志。
- 启动时优先从本地快照恢复。

## 工程结构

```text
src/TacticalField.Core/          核心模型、增量算法、发布、缓存、快照、样本
src/TacticalField.Cli/           样本/轨迹/恢复/强杀演示命令
src/TacticalField.WinForms/      WinForms 网格界面
tests/TacticalField.Verification/ 自包含验收程序
data/samples/                    确定性样本
data/baselines/                  样本基线哈希
artifacts/logs/                  实测轨迹与验证输出
scripts/verify.sh                一键构建与验收
```

## 常用命令

```bash
export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1

dotnet build TacticalField.sln -c Release
dotnet run --project tests/TacticalField.Verification/TacticalField.Verification.csproj -c Release --no-build

dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  generate-samples --data-dir data --force
dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  verify-samples --data-dir data
dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  run-sample small_8x8 --data-dir data --out artifacts/logs/small_8x8.log

dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  crash-demo --data-dir artifacts/verification/demo
dotnet run --project src/TacticalField.Cli/TacticalField.Cli.csproj -c Release --no-build -- \
  recover --data-dir artifacts/verification/demo

scripts/smoke-gui-windows.sh
```

## 本次实测结果

```text
== verification suite ==
PASS full-field-determinism checks=66
PASS randomized-incremental-vs-full checks=20212
PASS batch-impact-and-cache checks=15
PASS versioned-query-isolation checks=11
PASS ordered-async-batches checks=8
PASS busy-and-batch-rollback checks=12
PASS snapshot-recovery-and-corruption checks=15
PASS forced-termination-recovery checks=6
PASS deterministic-samples checks=19
VERIFICATION PASS tests=9/9 checks=20364

PASS sample=small_8x8 ... deterministic=true baseline=true fields=true chain=true cache_hits=6 cache_misses=4
PASS sample=medium_12x10 ... deterministic=true baseline=true fields=true chain=true cache_hits=2 cache_misses=8
PASS sample=large_16x12 ... deterministic=true baseline=true fields=true chain=true cache_hits=3 cache_misses=7
VERIFY PASS 3/3

forced-termination exit_code=134
RECOVERY code=OK version=3 sequence=2 chain=0bd8b6cddf82230faa8c28ab1d98fd80777df8232bd9449339f43e84463e19b7 fields=2
RESTORED_FIELD key=alpha@14 ... cache=True
RESTORED_FIELD key=bravo@9 ... cache=True

WindowsDesktop runtime:
Microsoft.WindowsDesktop.App 8.0.34 [...]
GUI SMOKE PASS
```

强杀退出码 134 来自 `Environment.FailFast`，用于模拟进程未走正常关闭流程。新进程仅从
`data/runtime/snapshot.json` 恢复，校验链后成功读到同一字段哈希与缓存路径。
