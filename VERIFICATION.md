# 实际验证记录

验证日期：2026-11-21  
运行环境：Ubuntu 26.04，本地 venv Python 3.11.18，NumPy 2.4.6  
运行方式：无 Docker、无网络服务、仅本地确定性样本。

## 自动化测试

命令：

```bash
.venv/bin/python -m unittest discover -v
```

结果：18 项测试全部通过，覆盖：

- 随机增量重算与全量 Dijkstra 一致；
- 断环/不可达分量清空；
- 批量闭包；
- 随机多格批次中闭包影响包含一次性重算最终差异，且顺序回放字段与合并字段一致；
- 缓存命中、失效、重算及缓存路径与新鲜重算逐字节一致；
- 构建期旧版本可读与发布后版本隔离；
- 中途失败回滚；
- 查询任务 `E_BUSY` 与释放；
- 恢复、损坏文件、断链、缺失字段和临时文件忽略；
- 64×64 双跑逐字节一致。

另执行 200 个随机四变更批次的压力回放：200/200 成功发布，5 个批次出现“逐条中间变化但合并最终抵消”的闭包扩展，且每次 `closure_equals_merged` 与 `field_replay_equals_merged` 均为 `true`。

## 两级实验

命令：

```bash
.venv/bin/python -m tactical.cli generate \
  --output artifacts/experiment \
  --sizes 64 512 \
  --seed 20261121 \
  --verify-twice
```

该命令同时执行第一次实验与第二次 replay。最终结果：

| 项目 | 64×64 | 512×512 |
|---|---:|---:|
| 最终版本 | 6 | 6 |
| 最终版本哈希 | `aeadf87876b21894e7bc2ac443028af7da4768fd6b650617a76285750997024b` | `4958e964e59027df5b48276498af25d4a034421c9b5616e1d840eaa69b85438b` |
| 轨迹 SHA-256 | `3fad35eeaad8a61150f24ec506a378f1ef6a793ed34a105790ffb6043104e75c` | `6db50f0578071e9b809c6849a59ffedf85c6752f481b2cd81d7c8c8de534858a` |
| 样本 SHA-256（`baseline.json`） | `fa515f0d858761b7b8a989d928e9359aabb7030ba5b1ed9f184479d11ecdd41b` | `7d91aa9ef3ef020f74b412e13c330aa5b01934d8dacfdde5ab8588f1d8a041ab` |
| 单版本快照数组 | 184,320 B | 11,796,480 B |
| 两个版本数组下界 | 368,640 B | 23,592,960 B |
| 网格数组 | 53,248 B | 3,407,872 B |
| 全部单位代价场数组 | 131,072 B | 8,388,608 B |
| 缓存依赖格引用（路径格 + 四邻） | 3,434 → 3,427 | 28,998 → 29,002 |

`artifacts/experiment/baseline.json` 的 SHA-256 为：

```text
28b9163193b4b216c92b0a6789ed6ed479db4548ead901570e2fbeaab0517252
```

`artifacts/experiment/replay_report.json` 确认：

```text
sample_bytes_equal = true   (64, 512)
trace_bytes_equal  = true   (64, 512)
baseline_equal     = true
ok                 = true
```

## 批次轨迹摘要

所有批次均有 `closure_equals_merged = true`。

两级确定性样本中 `raw_merged_impact_size == merged_impact_size`，即没有出现“逐条中间变化但最终抵消”的格子；随机压力测试中已覆盖该情形，并验证闭包影响会保守包含该格、字段回放仍与合并结果一致。

| 规模 | 批次 | 变更数 | 合并影响 | 闭包影响 | 缓存命中 | 缓存失效/重算 |
|---|---:|---:|---:|---:|---:|---:|
| 64 | 1 | 3 | 41 | 41 | 16 | 0 / 0 |
| 64 | 2 | 3 | 637 | 637 | 15 | 1 / 1 |
| 64 | 3 | 4 | 1,971 | 1,971 | 3 | 13 / 13 |
| 64 | 4 | 3 | 20 | 20 | 15 | 1 / 1 |
| 64 | 5 | 3 | 610 | 610 | 11 | 5 / 5 |
| 64 | 6 | 4 | 241 | 241 | 15 | 1 / 1 |
| 512 | 1 | 3 | 724 | 724 | 16 | 0 / 0 |
| 512 | 2 | 3 | 13,126 | 13,126 | 15 | 1 / 1 |
| 512 | 3 | 4 | 7,048 | 7,048 | 15 | 1 / 1 |
| 512 | 4 | 3 | 8 | 8 | 16 | 0 / 0 |
| 512 | 5 | 3 | 21,710 | 21,710 | 14 | 2 / 2 |
| 512 | 6 | 4 | 58 | 58 | 16 | 0 / 0 |

从 64×64 到 512×512，理论单快照数组从 180 KiB 增至 11.25 MiB，约为 64 倍；候选构建时双版本数组下界从 360 KiB 增至 22.5 MiB。512×512 单次实验墙钟时间约 6 秒，联合双跑加 replay 总墙钟约 13.5 秒（仅观测值，不写入确定性轨迹）。

## 内置恢复、隔离与回滚证据

每份轨迹包含：

- `record=isolation`：候选版本准备完成但尚未替换发布引用时，查询仍返回旧版本和旧哈希；
- `record=rollback_probe`：在第 2 批后注入 `RuntimeError`，返回 `E_BATCH_FAILED`，版本、哈希和旧快照查询均保持不变；
- `record=recovery`：运行结束后从磁盘恢复最近完整快照，版本、哈希、字段哈希和缓存键集合一致。

最终快照文件位于：

```text
artifacts/experiment/snapshots_64/snapshot-000000000000.snap ... 000006.snap
artifacts/experiment/snapshots_512/snapshot-000000000000.snap ... 000006.snap
```

