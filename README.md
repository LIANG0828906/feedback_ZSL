# MeshRepair：确定性分块网格修复与强杀恢复

这是一个不依赖容器、仅使用 C++17 和 CMake 的本地三角网格修复系统。它把输入面片确定性地分配到空间分块，先原子提交分块表与边界映射，再在内存预算和并行度约束下修复分块，最后按提交结果重建整体网格。

核心特性：

- 自定义二进制三角网格解析，支持 `float32/float64` 坐标和 `uint32/uint64` 索引。
- 顶点邻接、有向边表、无向边引用、边界边和非流形边构建。
- 面片跨越多个分块时的唯一所有者与 straddle 记录。
- 可检查的 `chunk_table.json`、`boundary_map.json` 和 SHA-256 内容哈希。
- 容差焊接、封闭边界环提取、耳切/扇形孔洞填补、非流形边分组拆分、碎片删除和法向误差统计。
- 稳定的分块提交顺序、原子临时文件加 `rename`、逐块哈希链和强杀续修。
- 强制终止后只读取已提交 `plan.bin` 与 `.mrchunk` 分块，续修输出和轨迹与未中断执行逐字节一致。
- 可选的 GLFW/OpenGL 快照查看器，后台线程修复，渲染线程只读取完整替换的不可变快照。
- 确定性样本生成器，包含跨块孔洞、重复顶点、非流形边和碎片。

## 构建

要求 CMake 3.16+、支持 C++17 的编译器。Linux 示例：

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=RelWithDebInfo
cmake --build build -j
ctest --test-dir build --output-on-failure
```

OpenGL 查看器是可选目标。找到 OpenGL 和 GLFW 3.2+ 时自动启用；否则构建一个明确返回“未启用”的 stub，不影响核心修复和测试：

```bash
cmake -S . -B build -DMESHREPAIR_BUILD_UI=ON
```

Windows 可使用 MSVC 或 MinGW。POSIX 构建使用 `flock` 和 `fsync`；Windows 构建使用独占文件句柄和 `MoveFileExA` 的原子替换语义。

## 快速运行

```bash
# 生成确定性样本、默认配置
./build/meshrepair_cli generate \
  --out /tmp/sample.mesh --config /tmp/sample.cfg --seed 42

# 查看输入拓扑
./build/meshrepair_cli info --input /tmp/sample.mesh

# 完整修复
./build/meshrepair_cli repair \
  --input /tmp/sample.mesh --config /tmp/sample.cfg --out /tmp/run --json

# 只读取磁盘已提交状态重新校验、拼接
./build/meshrepair_cli verify \
  --input /tmp/sample.mesh --config /tmp/sample.cfg --out /tmp/run
```

强杀恢复验证：

```bash
# 提交前两个分块后进程强制 _exit(137)
./build/meshrepair_cli repair --input /tmp/sample.mesh \
  --config /tmp/sample.cfg --out /tmp/killed --stop-after 2

# 续修只使用 /tmp/killed/chunks 中已提交的分块
./build/meshrepair_cli repair --input /tmp/sample.mesh \
  --config /tmp/sample.cfg --out /tmp/killed
```

完整验收脚本：

```bash
./scripts/acceptance.sh build
```

## 配置

以下字段必须存在且只能出现一次：

```ini
parallelism=4
memory_budget_bytes=268435456
block_memory_bytes=67108864
chunk_size_x=1.0
chunk_size_y=1.0
chunk_size_z=1.0
weld_tolerance=0.000001
```

可选字段：

```ini
min_component_faces=3
normal_tolerance_deg=5.0
temp_reserve_bytes=67108864
temp_dir=/tmp
```

`parallelism` 在 `[1,4096]`，预算和尺寸必须为正，焊接容差非负。`block_memory_bytes` 不得大于全局预算。运行时不会静默提高任何预算或并行度。未知字段和重复字段会返回 `E_CONFIG_INVALID`。

## 输出目录

```text
run/
  plan.bin                原子提交的分块表 + 边界映射 + 版本与哈希
  chunk_table.json        可检查的分块所有者、面片、顶点和 straddle 清单
  boundary_map.json       可检查的 (块, 相邻块, 边界顶点, 边界边) 清单
  chunks/
    chunk_000000.mrchunk  单文件原子提交：结果 + 局部边界哈希 + 链上哈希
    ...
  repaired.mesh           确定性重建的整体网格
  stitch.sha256           拼接结果内容哈希
  report.json             统计、误差、孔洞、分块状态
  trace.log               确定性调度/提交/拼接轨迹
```

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_MESH_INVALID` | 头部、长度、索引范围、面片数或坐标有限性错误；附首个出错 `face`/`vertex` |
| `E_CONFIG_INVALID` | 配置字段缺失、重复、未知或取值非法 |
| `E_BOUNDARY_PENDING` | 跨块孔洞所需的两端边界映射尚未提交；附块和孔洞 ID |
| `E_TOPOLOGY_CONFLICT` | 重复填补、面片重叠、边归属冲突或拼接校验失败；附块/边/面片/孔洞 |
| `E_BUDGET_EXCEEDED` | 单块预估占用超过 `block_memory_bytes`，未启动；附块和预估字节数 |
| `E_RECOVERY_INVALID` | 分块表、边界映射、版本、内容哈希或链式哈希不符；附首个断链块 |
| `E_BUSY` | 同一网格已有修复会话 |
| `E_CANCELLED` | 在分块边界响应协作式取消 |

CLI 在 stdout 输出稳定的 JSON 错误对象，在 stderr 输出便于阅读的定位信息。

## 格式与设计

- [`docs/FORMAT.md`](docs/FORMAT.md)：二进制网格、plan、chunk bundle 与哈希定义。
- [`docs/DESIGN.md`](docs/DESIGN.md)：切分、边界协议、修复算法、调度、原子提交、恢复和渲染隔离。
- [`docs/VALIDATION.md`](docs/VALIDATION.md)：本机实际验收记录。

## 确定性边界

- 分块所有者是面片包围盒触及单元中字典序最小的单元；跨单元面片同时写入 straddle 清单。
- 分块 ID 按单元坐标排序；边界 pair 按块 ID 排序；顶点和边在 pair 内排序。
- 焊接代表元是连通组内最小原始顶点 ID。非流形拆分的新 ID 按排序后的边和面组顺序分配。
- 所有持久化数值使用小端固定宽度编码；浮点为 IEEE-754，哈希为 SHA-256。
- 分块任务可以并行构建，但提交只按分块 ID 顺序进行，因此快照、哈希链和最终字节不受线程完成顺序影响。
- 孔洞填补所有者是孔洞涉及块中最小的块 ID，只生成一次。所有封闭边界环按孔洞处理；开放/损坏环不会被强行三角化。
- `report.json` 额外输出每条非流形边拆分的 `(edge, owner_chunk, group, faces)`，便于审计边复制后的唯一所有者。

## 当前验收结果

在本机 Ubuntu、GCC 15、CMake 3.30 环境：

```text
checks=206 failures=0
100% tests passed
```

样本 `seed=42` 的实际修复统计：

```text
original_vertices=17
vertices_after_weld=16
final_vertices=17
original_faces=16
final_faces=17
filled_faces=2
split_edges=1
split_groups=3
split_vertex_copies=2
debris_faces_removed=1
cross_chunk_hole_vertices=4
stitch_valid=true
```

强杀验证中，干净执行与 `--stop-after 2` 后重启的 `repaired.mesh`、`trace.log` 和 `stitch.sha256` 均逐字节一致。
