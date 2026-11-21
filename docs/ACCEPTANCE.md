# 实际验收记录

## 环境

- 实际执行环境：WSL Ubuntu 26.04，GCC 15.2，CMake 3.30.5。
- 目标平台兼容：C++17、CMake 3.20+；提供 Visual Studio 2022 / PowerShell
  构建与验收入口。
- 本轮容器没有可用 Windows OpenGL 驱动，因此可见性实测使用确定性软件
  离屏帧缓冲和 HZB；未使用网络、容器或外部场景资源。

## 自动测试

执行：

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j 4
ctest --test-dir build --output-on-failure
```

结果：`chunkpipe_tests` 通过，内部断言 408 项，0 失败。覆盖：

- 三版格式字段存在性、压缩解码、优先级推导和基线；
- 未知版本、截断、越界索引、AABB/顶点不一致、悬空层级和重复 ID；
- v1/v2 升级 v3、v3/v2 降级损失字段；
- 并行流式加载、队列去重、确定性发布、陈旧版本查询；
- 加载失败不产生占位分块、协作取消保留旧快照、`E_BUSY`；
- 连续两次渲染掩码/统计/哈希一致；
- 边界超差 `E_VISIBILITY_MISMATCH` 与剔除检查点取消；
- 快照恢复、跳过重复提交、原子临时文件、断链和截断错误定位。

## 三版样本结果

固定相机：位置 `(0, 2.5, -12)`，目标 `(0, 1.5, 7)`，FOV 60°，
`256x144`，边界容差 `0.05`。

| 版本 | 可见性掩码 | 遮挡剔除 | 可见/临界 | 快照哈希 |
| --- | --- | ---: | --- | --- |
| v1 | `070707070707070707` | 0 | 9 / 0 | `e64909a54557e439` |
| v2 | `1f1f1f030303030303` | 6 | 3 / 3 | `2e1526f11d543f24` |
| v3 | `1f1f1f030303030303` | 6 | 3 / 3 | `a41580146ff083b8` |

v1 没有遮挡体标记，因此不做遮挡剔除；v2/v3 前三块显式标记为遮挡体，掩码中
额外包含 bit 4。v3 的压缩描述、优先级和层级表不改变该视点下的几何可见性。

## 多版本差异

完整加载：

- 18 个可见掩码差异，全部归因 `format_degradation`；
- 9 个元数据差异，归因 `priority_derivation`；
- 差异逐分块输出字段，例如 `occluder`、`grid`、`priority`、`hierarchy`。

部分加载 `--max-loaded 3`：

- 15 个可见差异，其中 12 个归因 `load_order`、3 个归因
  `format_degradation`；
- 12 个元数据差异，包含 `priority_derivation` 和 `format_degradation`；
- 可定位到具体分块，例如 v1→v3 的分块 0/1/2 掩码从 `0x07` 变为未加载
  `0x00`，分块 3/4/5 从 `0x00` 变为 `0x07`。

## 确定性

`chunkpipe determinism` 连续两次运行同一 v3 存档：

```text
deterministic=true mask_equal=true stats_equal=true
```

两次掩码均为 `1f1f1f030303030303`，快照内容哈希均为
`a41580146ff083b8`。

## 强制终止与恢复

使用 `--crash-after 3` 在提交第 3 个分块后调用 `_Exit(70)`：

```text
crash_status=70
committed snapshots: 00000001..00000003
```

执行 `chunkpipe recover`：

```text
recovered=3 committed_chunks=0,1,2
[progress] loading 3/9
published_version=1 content_hash=a41580146ff083b8
loaded=9/9 mask=1f1f1f030303030303
```

恢复后正式快照数从 3 增至 9，已提交的 0/1/2 没有重复写入；最终内容哈希与
未中断运行一致。

## 验收产物

`scripts/run_acceptance.sh artifacts/acceptance` 输出：

- `artifacts/acceptance/samples/scene_v{1,2,3}.arc`
- `artifacts/acceptance/samples/baseline.json`
- `artifacts/acceptance/run-v{1,2,3}/visibility.mask`
- `artifacts/acceptance/run-v{1,2,3}/visibility.json`
- `artifacts/acceptance/run-v{1,2,3}/stats.json`
- `artifacts/acceptance/compare/diff_report.json`
- `artifacts/acceptance/compare-partial/diff.jsonl`
- `artifacts/acceptance/determinism/determinism.json`
- `artifacts/acceptance/crash/recovery/*.snap`
- 各命令目录下的 `trajectory.jsonl`

验收入口最后输出：

```text
acceptance: PASS
v1 mask: 070707070707070707
v2 mask: 1f1f1f030303030303
v3 mask: 1f1f1f030303030303
v3 snapshot hash: a41580146ff083b8
recovery commits: 3 -> 9
```

## 未在本轮执行项

- 未在 Windows 10 原生环境运行 Visual Studio 生成的二进制。
- 未连接 OpenGL 4.1 驱动做 GPU 深度图实测；本轮以软件离屏后端完成确定性
  验收。PowerShell 脚本与 CMake 多配置构建路径已纳入仓库。

