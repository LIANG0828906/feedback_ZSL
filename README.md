# BattleEconomy Replay

回合制战术对局的地形破坏经济复核台。项目包含跨平台经济核心、WinForms 工作台和本地确定性验证器。

## 交付内容

- `src/BattleEconomy.Core`：地形/采集经济模型、增量结算、批次闭包校验、不可变发布、快照与恢复。
- `src/BattleEconomy.WinForms`：Windows Forms 工作台，展示地形网格、破坏高亮、双方产出曲线、批次状态和闭包结论。
- `tests/BattleEconomy.Verify`：本地验收器，覆盖 391 项断言，包括真实子进程强制终止恢复。
- `local-data`：本轮生成的场景、轨迹日志、快照、基线哈希和验收报告。

核心只使用 .NET 8 共享框架，`System.Text.Json` 直接来自 SDK，不引入外部数据或容器依赖。

## 环境与构建

本机确认：

```text
.NET SDK 8.0.428
Microsoft.NETCore.App 8.0.34
```

当前系统级 `libicu` 缺失，已在本机用户目录解包 `libicu78`，并通过 `~/.local/bin/dotnet` 包装器自动注入库路径。现在可直接执行：

```bash
dotnet --version
# 8.0.428
```

换到其他环境时，可安装系统 `libicu`，或设置 `DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1`。

构建整个解决方案：

```bash
dotnet build BattleEconomy.Replay.sln
```

Linux SDK 未随附 `winforms` 模板，`dotnet new winforms` 在该环境会返回模板不存在；因此工作台采用等价的 `net8.0-windows`、`UseWindowsForms=true`、`OutputType=WinExe` 手工项目。Linux 上通过 `EnableWindowsTargeting=true` 可完成编译，但 WinForms 进程必须到 Windows 桌面运行：

```powershell
dotnet run --project src/BattleEconomy.WinForms/BattleEconomy.WinForms.csproj
```

也可运行 `scripts/run-winforms.ps1`。

## 本地验收

```bash
./scripts/verify.sh
```

或：

```bash
dotnet run --project tests/BattleEconomy.Verify/BattleEconomy.Verify.csproj
```

验收器会重建 `local-data` 中的确定性场景和结果。主要产物：

- `local-data/verification-report.txt`：逐项断言与汇总。
- `local-data/scenarios/<name>/scenario.json`：地形、前哨和破坏批次。
- `local-data/runs/<name>/baseline.sha256`：基线内容哈希。
- `local-data/runs/<name>/trajectory.jsonl`：逐批轨迹日志。
- `local-data/runs/<name>/snapshots/economy-snapshot.json`：最终已提交快照。
- `local-data/determinism/a|b`：同种子、同操作序列的双跑对照。

## 经济模型

每块地形包含：

- 网格坐标、基础产出速率、双向邻接关系；
- 所属前哨节点引用和可采集标记；
- 前哨节点各自拥有阵营。

阵营贡献按以下规则推导：

1. 从地块出发，在未破坏地形上做确定性 BFS。
2. 最近的可达前哨接收该地块；距离相同按前哨 ID 的序数排序决胜。
3. 路径保存为“地块到前哨”的地形编号序列，并纳入内容哈希。
4. 不可采集、已破坏或没有可达前哨的地块贡献为 0。
5. 阵营每步产出是该阵营当前接收地块的产出速率之和。

每次成功批次只发布一个经济版本，累计资源按 `旧累计 + 新每步产出 × 本批经济步数` 一次性结算。

## 增量影响与批次闭包

单条破坏：

1. 在旧图上取被破坏地块的连通分量，作为保守候选集。
2. 应用破坏后只探测候选集中的采集路径。
3. 逐地块比较旧/新贡献内容哈希，最终影响集只保留真正变化的编号。
4. 候选集之外直接复用旧贡献和旧哈希。

一个批次：

1. 先在独立工作区按提交顺序计算每条破坏的增量影响集。
2. 取逐条影响集的并集作为传递闭包。
3. 在另一个独立工作区一次性应用全部破坏并统一结算。
4. 校验合并影响集与闭包集合相等，同时校验逐条模拟的最终工作区与合并工作区逐地块哈希相等。
5. 校验失败则不发布；校验通过也只产生一个版本。

若缓存不完整、依赖无法证明或显式请求保守模式，则重算全部地形，并在轨迹日志写入 `conservativeFallback=true` 和具体 `conservativeReason`。

轨迹日志每行包含：

- 批次号、破坏数；
- 每条破坏的影响集与候选数；
- 合并影响集、闭包集与 `closureValidated/closureConclusion`；
- 探测、重算、复用地块数；
- 前后版本哈希、经济步和双方累计产出。

## 不可变发布与查询

- `EconomySnapshot` 创建后不再修改，贡献表使用不可变字典。
- 结算只在局部工作区修改；全部校验完成后一次性替换发布引用。
- `AcquirePublishedSnapshot()` 返回完整不可变快照句柄和内容哈希。
- `Query(handle, expectedVersion)` 返回该句柄所属版本的完整贡献集。
- 句柄被后续发布淘汰后返回 `E_STALE_HANDLE`。
- 查询携带的版本不是当前版本时返回 `E_VERSION_STALE`。
- 同一战场由一个原子忙碌门保护，重复提交返回 `E_BUSY`。
- 结算期间查询仍读取上一完整发布，不会看到半更新状态。

## 快照、哈希链与恢复

快照路径：`.../snapshots/economy-snapshot.json`。

快照包含：

- 当前版本、批次序号、经济步、提交水位；
- 全部地形贡献及逐地块内容哈希；
- 采集路径和前哨分配；
- 已破坏编号、已提交破坏稳定键；
- 前序内容哈希与完整版本哈希链；
- 各阵营每步产出和累计资源。

写入流程为：同目录临时文件 → 写穿并 `Flush(true)` → 原子重命名。只有磁盘提交后的快照才会替换内存发布引用。

恢复流程：

1. 读取最近快照。
2. 校验版本与批次连续、提交水位、前序链接、逐版本链哈希、逐贡献内容哈希和当前内容哈希。
3. 任意断链或字段缺失返回 `E_RECOVERY_INVALID`，并设置 `FirstBrokenBatch`。
4. 恢复只从已提交快照继续，不信任未提交内存状态。
5. 批次开始前原子写入 `pending-batch.json`；成功提交后删除。
6. 强制终止后，若意图批次已包含在提交水位中则直接丢弃意图，不重复结算；否则从快照水位恢复该批次。
7. 已提交破坏按稳定键跳过，重复请求返回幂等 `NoOp`。

验收器通过启动真实子进程、写入意图后由父进程强制 `Kill` 的方式验证恢复，不依赖子进程正常退出，也不承诺生成退出报告。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_TERRAIN_INVALID` | 地形为空、邻接越界/非双向、前哨悬空、产出非正等，`TerrainId` 定位地块 |
| `E_BATCH_FAILED` | 结算/提交失败或取消；带 `Phase`、`ProcessedDestructionIndex`，发布与磁盘快照保持旧版本 |
| `E_BUSY` | 同一战场已有批次结算 |
| `E_VERSION_STALE` | 查询携带的版本不是当前发布版本 |
| `E_STALE_HANDLE` | 快照句柄已被后续发布淘汰 |
| `E_RECOVERY_INVALID` | 快照、哈希链、字段或恢复意图无效；带 `FirstBrokenBatch` |

## WinForms 工作台

工作台提供：

- 小/中/大三组内置场景和自定义种子、行列、邻接密度；
- 自绘地形网格、Aurora/Bastion 采集着色、前哨菱形标记、破坏叉号、待提交高亮；
- 双方累计产出曲线；
- 当前版本、经济步、完整内容哈希、提交水位；
- 载入样本批次、点击地块手工组批、提交并统一结算；
- 结算进度、闭包 `MATCH/MISMATCH` 结论；
- 协作式取消；
- 检测到 `pending-batch.json` 时恢复未完成批次；
- 轨迹文件路径与批次明细。

工作台数据位于 `local-data/workbench/<seed-size-density>/`，不会与验证器使用的场景快照混淆。

## 本轮实际验证

验证时间：2026-11-22（Asia/Shanghai）。

```text
dotnet build BattleEconomy.Replay.sln
Build succeeded. 0 Warning(s), 0 Error(s)

dotnet run --project tests/BattleEconomy.Verify/BattleEconomy.Verify.csproj
checks=391 passed=391 failed=0
```

内置场景结果：

| 场景 | 规模 | 密度 | 批次 | 基线哈希（前缀） | 最终版本 | 最终哈希（前缀） |
| --- | --- | ---: | ---: | --- | ---: | --- |
| small-sparse | 10×8 | 30% | 4 | `cd4ee9797ca11421` | 4 | `fd9edc4b83898545` |
| medium-balanced | 18×14 | 55% | 6 | `20caccaa95c43d62` | 6 | `baf0cad05be2c9ad` |
| large-dense | 28×22 | 78% | 8 | `1c677126e48e8543` | 8 | `a035e70e685bda20` |

确定性双跑：

```text
4a84e48b8794d9861fb2a2b340a804feaa67bcb2fcc42e06f9edb22c94c92924  determinism/a/trajectory.jsonl
4a84e48b8794d9861fb2a2b340a804feaa67bcb2fcc42e06f9edb22c94c92924  determinism/b/trajectory.jsonl
```

同种子、同操作序列的轨迹日志与最终快照均逐字节一致。

覆盖验证包括：

- 三类地形无效输入及地块定位；
- 多种规模/密度的确定性生成；
- 18 个成功批次的逐条影响、闭包、单次发布、缓存复用和经济累计；
- 结算期并发查询、`E_BUSY`、`E_STALE_HANDLE`、`E_VERSION_STALE`；
- 结算阶段失败、提交阶段失败、协作式取消的完整回滚和快照字节不变；
- 保守全量重算及原因标注；
- 已提交快照恢复、重复破坏幂等、断链和缺字段定位；
- 真实进程强制终止后的意图恢复与单次补结算。

## 说明

- 全程本地运行，不使用 Docker、外部数据下载或未授权目标。
- 当前环境只能编译 WinForms，无法在 Linux 无显示服务上实际打开桌面窗口；因此 GUI 的编译与交互代码已交付，业务行为由同一核心库的验证器完整覆盖。
