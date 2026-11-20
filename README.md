# 战棋战术决策服务

本项目实现一个本地、确定性、可恢复的战术网格最短路服务。它覆盖：

- 每格基础移动代价、单位可通行位掩码、动态危险区标记；
- 按单位反向传播的最小代价场与内容哈希；
- 批量地形变更的影响闭包、局部增量重算和路径缓存依赖失效；
- 不可变发布快照、版本隔离、批次原子发布与失败回滚；
- 查询/重算并发隔离、同单位同目标任务互斥；
- 临时文件加原子重命名的快照持久化、哈希链校验与强制终止恢复；
- 64×64 与 512×512 两级确定性样本、轨迹日志和基线哈希。

实现仅依赖 Python 标准库和 NumPy，不访问网络或外部服务。

## 环境准备

要求 Python 3.11 和 NumPy。Windows 命令行可直接执行：

```bat
py -3.11 -m venv .venv
.venv\Scripts\python -m pip install -r requirements.txt
.venv\Scripts\python run_tests.py
.venv\Scripts\python run_experiment.py generate --output artifacts\experiment --sizes 64 512 --verify-twice
```

Linux/macOS：

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m unittest discover -v
.venv/bin/python -m tactical.cli generate --output artifacts/experiment --sizes 64 512 --verify-twice
```

`--verify-twice` 会在 `artifacts/experiment/replay` 中重新生成同样种子和操作的样本与轨迹，并逐字节比较样本、轨迹、基线文档以及最终版本哈希。

## 快速运行

生成样本、快照、轨迹和基线：

```bash
python -m tactical.cli generate \
  --output artifacts/experiment \
  --sizes 64 512 \
  --seed 20261121 \
  --verify-twice
```

运行单个样本：

```bash
python -m tactical.cli run-sample \
  --sample artifacts/experiment/sample_64.json \
  --output artifacts/single-64
```

主要输出：

- `sample_<size>.json`：确定性样本参数、初始目标、预置查询和批次变更序列；
- `trace_<size>.jsonl`：逐批轨迹和隔离/回滚/恢复证据；
- `baseline.json`：样本哈希、轨迹哈希、最终版本哈希和字段哈希；
- `snapshots_<size>/snapshot-<batch>.snap`：每个完整发布的持久化快照；
- `replay_report.json`：双跑逐字节比较结果。

## 核心语义

### 网格与代价场

`BattleGrid` 包含：

- `base_cost`：每格基础移动代价，`int32`，至少为 1；
- `pass_mask`：`uint64`，低 63 位表示单位是否可通行，最高位 `WORLD_BLOCK_BIT` 表示全局障碍；
- `danger`：`uint8`，0/1 危险区；
- `danger_penalty`：危险区附加代价，样本中为 5。

单位 `u` 在格 `c` 的通行代价为：

```text
INF                                     若 c 被全局障碍封锁或未开放给 u
base_cost[c] + danger[c] * danger_penalty  否则
```

反向最短路由目标格距离 0 初始化，使用固定邻居顺序 `上、左、右、下` 与堆键 `(距离, 行, 列)`。路径代价包含起点和中间格代价，不重复计入目标格。

代价场哈希由格式头、单位、目标坐标、形状和 `<i8` 小端距离数组计算。发布版本哈希为：

```text
version_hash = SHA256(previous_version_hash || 0x00 || payload_hash)
payload_hash = SHA256(canonical_json(version, batch, grid_hash, field_hashes))
```

### 增量影响闭包

单次批量先应用全部变更，再按单位做一次增量重算。增量算法不只处理“减小”，还显式处理障碍新增导致的不可达和断环：

1. 找出局部通行代价变化的种子格；
2. 沿旧代价场的最短路径前驱关系求闭包。正权网格保证该前驱图无环；
3. 将闭包内旧距离置为 `INF`，以闭包外的旧距离为边界做局部反向 Dijkstra；
4. 对代价下降产生的改善向外传播，直到标签稳定；
5. 用新代价场与旧代价场逐格比较，得到实际改变的格集合。

批次先计算一次性合并后的字段差异，再按顺序逐条回放得到影响并集。若某格只在逐条回放的中间状态改变、最终又抵消，它仍属于需求定义的传递闭包，因此纳入批次的保守影响集并触发相关缓存失效；`raw_merged_impact_size` 与 `impact_sizes` 分别记录原始最终差异和闭包影响集。逐条回放最终字段必须与合并字段完全一致，且 `closure_equals_merged` 必须为 `true`，否则批次以 `E_BATCH_FAILED` 拒绝发布。

### 版本发布与回滚

`PublishedSnapshot` 是不可变对象，持有只读网格数组、各单位代价场和路径缓存映射。候选批次在独立工作区完成：

1. 克隆网格并应用全部变更；
2. 完成全部单位增量重算和闭包校验；
3. 完成缓存失效/重算和新快照哈希链；
4. 若有持久化目录，先原子写入完整候选快照；
5. 最后仅在短暂发布锁内替换 `_snapshot` 引用。

查询在方法开始时取得一次快照引用，后续所有格、哈希和路径都来自该引用。批次构建期间旧快照持续可读。失败时只释放候选结构，不修改 `_snapshot`。

### 路径缓存

每条缓存记录：

- 单位、目标、起点；
- 所属发布版本；
- 完整路径、路径哈希；
- 路径格及其四邻候选格的线性索引依赖集合（用于保证固定顺序选路的字节一致性）；
- 可达标志和代价。

批次影响集与依赖集合相交时失效并重算；不相交时保留。缓存不会全量清空。轨迹输出每批 `cache_invalidated`、`cache_hits`、`cache_recomputed`。

### 并发与任务互斥

- 查询只读取不可变快照；
- 批次使用单独提交锁串行化；
- 发布引用使用独立短锁，不会在候选计算期间阻塞查询；
- 路径任务按 `(unit, target)` 原子占位，重复提交返回 `E_BUSY`；
- 任务异常或完成时通过 `finally`/Future 回调释放占位和线程内临时代价场。

### 恢复

每份快照写入同目录临时文件，执行 `flush + fsync + os.replace`。文件使用 NumPy `npz` 容器，包含：

- 规范 JSON manifest；
- 网格数组与每单位距离数组；
- 版本、批次号、前序哈希、负载哈希、版本哈希；
- 路径缓存版本、路径哈希和依赖集合。

恢复按批次号扫描全部 `.snap` 文件，要求：

- 从版本 0 开始；
- 批次号和版本号连续；
- 前序哈希等于上一发布版本哈希；
- 网格哈希、字段哈希、负载哈希和版本哈希全部匹配；
- 字段数组和缓存字段完整。

任一断链、缺失字段、哈希不符或容器损坏均返回 `E_RECOVERY_INVALID`，不会发布部分状态。残留 `.tmp` 文件会被忽略。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_VERSION_MISMATCH` | 查询版本或哈希不等于当前完整发布版本，或待发布候选已过期 |
| `E_BATCH_FAILED` | 批次校验、闭包、重算或持久化失败，已保持上一发布版本 |
| `E_BUSY` | 同一单位同一目标已有路径重算任务 |
| `E_RECOVERY_INVALID` | 快照断链、损坏、缺字段或哈希校验失败 |

## 轨迹字段

每条 `record=batch` 至少包含：

- `batch`、`change_count`；
- `field_hashes_before`、`published_version_hash`；
- `impact_sizes`、`closure_impact_sizes`；
- `raw_merged_impact_sizes`、`raw_merged_impact_size`；
- `merged_impact_size`、`closure_impact_size`、`closure_equals_merged`；
- `closure_extends_raw`、`field_replay_equals_merged`；
- `recomputed_cells`、`local_cost_changed_cells`；
- `generation_cells_after`；
- `cache_hits`、`cache_invalidated`、`cache_recomputed`；
- `snapshot_array_bytes`、`field_array_bytes`、`grid_array_bytes`；
- `cache_dependency_count`、`two_version_array_bytes`。

轨迹不含时间戳、线程调度或绝对路径，使用规范 JSON 加固定换行输出，因此相同种子与操作序列可逐字节比较。

## 测试覆盖

运行：

```bash
python -m unittest discover -v
```

测试覆盖：

- 反向代价场与独立 Bellman 不动点参考解一致；
- 随机代价、危险区、障碍、单位掩码变化下增量结果与全量 Dijkstra 完全一致；
- 断开环导致的不可达分量能正确清空；
- 批次合并闭包与逐条顺序闭包一致；
- 缓存依赖失效、命中、重算统计，以及缓存路径与新鲜重算逐字节一致；
- 候选快照构建期间旧版本查询仍然成功；
- 发布后旧版本/旧哈希查询返回 `E_VERSION_MISMATCH`；
- 注入批次中途异常后版本和哈希不变；
- 重复任务返回 `E_BUSY`，任务结束后可再次提交；
- 正常恢复、损坏文件、断链、缺失字段和残留临时文件；
- 64×64 双跑样本、轨迹和基线逐字节一致。

## 规模观测

实验输出的内存字段是确定性的 NumPy 数组字节数，不是机器相关 RSS：

- `grid_array_bytes`：基础代价、掩码和危险区数组；
- `field_array_bytes`：全部单位距离场；
- `snapshot_array_bytes`：单个发布快照的数组部分；
- `two_version_array_bytes`：候选构建时旧、新两个快照数组的内存下界；
- `cache_dependency_count`：所有缓存路径依赖格引用数；
- `recompute_mask_bytes`：每单位影响/工作布尔掩码按一格一字节计算。

实际验证哈希和规模对比记录在 `artifacts/experiment/baseline.json` 与对应轨迹中。

