# 三维资产入库网格修复

本项目实现一个本地、可恢复、按空间分块并行的三角网格修复链路。所有样本、提交记录、快照和轨迹均落盘在工作目录；不使用 Docker 或任何容器环境。

## 已实现能力

- 自定义二进制网格 `MSHRP01` 的加载、写出、头部解析、索引范围和退化面校验。
- 顶点邻接、无向边表、边界边、非流形边和面连通分量构建。
- 基于几何容差的确定性重复顶点焊接，以及跨分块边界合并映射。
- 封闭孔洞边界环提取、顶点数/包围盒输出、容差变化前后差异摘要。
- 空间分位分块、两阶段并行调度、固定提交序、预算等待与协作取消。
- 跨块孔洞填补门控、质心扇形三角化、非流形边拆组、孤立碎片删除、法向重算。
- 临时文件 + `fsync` + 原子重命名的分块提交、内容哈希链和强制终止恢复。
- 原始网格真值法向误差统计、超容差定位、非法输入注入的独立子业务码。
- 修复后台线程与 OpenGL 渲染线程隔离；渲染只读取不可变整体快照。
- PyOpenGL 隐藏窗口离屏渲染；上下文失败时自动生成静态 SVG 预览，CLI 入口不受影响。

## 环境

实测环境：

- Python 3.14.4（要求 Python >= 3.10）
- NumPy 2.5.5
- PyOpenGL 3.1.10
- pygame-ce 2.5.8（提供本地隐藏 OpenGL 窗口）
- Mesa llvmpipe，OpenGL 4.5 Compatibility Context

安装：

```bash
uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python -e '.[gl,test]'
```

如果已有可用的 Python 3.10+ 和 pip：

```bash
python -m venv .venv
. .venv/bin/activate
python -m pip install -e '.[gl,test]'
```

最小运行依赖只有 NumPy。PyOpenGL 与 pygame-ce 仅用于 OpenGL 预览；缺失时会降级为静态 SVG。

## 快速开始

```bash
# 生成确定性扫描样本和基线摘要
.venv/bin/mesh-repair generate --outdir samples

# 查看头部、拓扑、边界和焊接摘要
.venv/bin/mesh-repair inspect samples/scan_asset.msh

# 分块修复；工作目录中保留 manifest、block 提交、轨迹
.venv/bin/mesh-repair repair samples/scan_asset.msh \
  -o artifacts/repaired.msh \
  --work-dir artifacts/work \
  --blocks 2,2,1 \
  --parallelism 2 \
  --global-budget 8M \
  --task-budget 4M

# 带 OpenGL 预览；失败时生成 .svg
.venv/bin/mesh-repair repair samples/scan_asset.msh \
  -o artifacts/preview-run.msh \
  --work-dir artifacts/preview-work \
  --render --preview artifacts/preview.png

# 重新焊接并输出边界环差异摘要
.venv/bin/mesh-repair compare-tolerance samples/tolerance_asset.msh \
  --old 1e-6 --new 1e-3
```

## 二进制格式

小端序头部：

```text
offset  size  field
0       8     magic = b"MSHRP01\0"
8       2     version = 1
10      1     coord_type: 0=f32, 1=f64
11      1     index_width: 2, 4, 8 bytes
12      8     vertex_count
20      8     face_count
28      ...   vertices, then faces
```

顶点载荷为 `vertex_count * 3` 个坐标；面载荷为 `face_count * 3` 个无符号索引。加载时会在返回几何前完成：

- 文件长度与头部计数一致性检查；
- 非有限坐标检查，定位首个顶点；
- 索引范围检查，定位首个越界面片和角点；
- 重复索引与零面积/低于阈值的退化面检查，定位首个面片。

业务错误码：

| code | 含义 |
| --- | --- |
| `E_MESH_INVALID` | 网格格式或几何非法 |
| `E_BUSY` | 同一源网格已有进程/任务在修复 |
| `E_BUDGET_EXCEEDED` | 分块超过单任务预算或全局预算 |
| `E_BOUNDARY_PENDING` | 跨块孔洞依赖的焊接映射尚未提交 |
| `E_RECOVERY_INVALID` | manifest、分块表版本、哈希链或提交内容损坏 |
| `E_CANCELLED` | 分块边界协作取消 |
| `E_CONFIG_INVALID` | 配置参数非法 |

`E_MESH_INVALID` 通过 `subcode` 区分注入类别：

- `E_INDEX_OUT_OF_RANGE`
- `E_DEGENERATE_FACE`
- `E_NONFINITE_COORD`
- `E_FACE_COUNT_MISMATCH`
- `E_MALFORMED_HEADER`

## 修复链路与分块一致性

1. **加载与拓扑**：严格读取格式并构建顶点邻接、有向半边、无向边表、面连通分量。
2. **全局焊接**：容差为 0 时按精确坐标合并；大于 0 时使用空间哈希和 27 邻域并查集。代表顶点固定为组内最小原始索引，保证确定性。
3. **边界环提取**：只接受一端指向下一端的封闭有向边界链；同时报告开放链和分叉顶点。每条环输出顶点数和包围盒。
4. **空间分块**：按面中心在各坐标轴上的分位数切分，而不是直接使用含远端碎片的整体包围盒。这样远处碎片不会把主壳压进单一分块。
5. **阶段 A（并行）**：每个分块构建局部拓扑、重新焊接局部副本，并登记“局部 -> 全局焊接顶点”映射以及跨块合并对。所有依赖块的映射就绪后才允许填补。
6. **边界门控填补**：对每个孔洞收集参与块；任一参与块映射未就绪即返回 `E_BOUNDARY_PENDING`，并给出缺失块编号。填补在孔洞中心生成顶点，按反向边界半边生成扇形面，保证共享边方向一致。
7. **非流形拆分**：先按二邻接边连接的流形面组拆分；若仍存在异常扇，再确定性拆出残余面。
8. **碎片删除与法向**：删除低于阈值的面连通分量，紧凑化顶点，重建邻接，按面积加权重算法向。
9. **阶段 B（并行）**：每块生成自包含结果、重新测量局部拓扑、原子提交；最后由已提交块装配，不允许使用未提交内存结果。

最终装配会检查：

- 同一全局顶点在不同块中的坐标必须逐位一致；
- 面 ID 无重复、无缺失、完整覆盖预期面集；
- 输出边界边数、非流形边数、跨块共享边数；
- 接缝冲突数为 0 才允许写出网格。

## 内存预算

计量是确定性的结构估算，而不是进程 RSS。对每个分块：

```text
vertex_bytes        = 3 * 8 * vertex_count
face_bytes          = 3 * 8 * face_count
edge_table_bytes    = 88 * edge_count
adjacency_bytes     = 8 * (3 * face_count + vertex_count)
temp_buffer_bytes   = 64 * vertex_count + 112 * face_count + 48 * edge_count
total_bytes         = 上述各项之和
```

阶段 A 使用预估值，阶段 B 使用实际最终局部拓扑重新测量。两级预算：

- `task_budget_bytes`：单任务硬上限。超限块不会进入工作线程。
- `global_budget_bytes`：同时在运行块的总和上限。

全局预算不足时工作线程在 `threading.Condition` 上等待；一个任务释放预算后 `notify_all()` 唤醒等待队列。启动闸门强制按稳定提交序开始任务，后来块不能抢占更早块的等待名额。

## 原子提交与恢复

工作目录布局：

```text
work/
  locks/<source_sha>.lock
  traces/trace-<run-id>.jsonl
  sessions/<session-id>/
    manifest.json
    block-table.json
    blocks/block-0000.json
    blocks/block-0001.json
    report.json
```

`manifest.json` 固定：

- 源文件 SHA-256；
- 修复配置哈希；
- `block_table_version`；
- 分块表 SHA-256；
- manifest 自身内容哈希。

每个块记录固定：

- manifest 哈希、源哈希、配置哈希、分块表版本和哈希；
- 提交序号、`prev_hash`、`content_hash`、`payload_sha256`；
- 自包含顶点、面、来源面 ID、焊接映射、预算和局部拓扑。

提交顺序由锁串行化，每次写入同目录临时文件，执行 `fsync` 后 `os.replace`，再同步目录。已存在的块文件永不覆盖；新结果若与已提交内容不同会触发 `E_RECOVERY_INVALID`。

恢复时只读取 manifest 和磁盘已提交块，校验 manifest 哈希、分块表、每块 payload 哈希及完整哈希链。损坏、断链或分块表版本不匹配均中止且不写出目标网格。

## 渲染隔离

`SnapshotStore` 保存单个不可变 `MeshSnapshot`：

- 写入方先构建完整顶点/面/分块着色数组，数组设为只读；
- 提交只替换一个对象引用；
- 渲染线程每次 `latest()` 取得同一完整版本，不遍历修复中的可变状态。

修复开始时发布完整输入基线，成功装配后整体替换为最终结果，不发布中间半成品。OpenGL 使用 PyOpenGL + pygame-ce 隐藏窗口；没有上下文时生成静态 SVG。

## 测试与验收

运行单元/集成测试：

```bash
.venv/bin/pytest -q
```

运行完整验收并生成报告、样本、网格、预览和轨迹：

```bash
.venv/bin/python scripts/run_acceptance.py
```

验收实际结果（本次执行）：

| 项目 | 结果 |
| --- | --- |
| 自动化测试 | 21/21 通过 |
| 验收检查 | 17/17 通过 |
| 输入 | 97 顶点，169 面 |
| 输出 | 87 顶点，170 面 |
| 焊接重复顶点 | 6 |
| 填补孔洞面 | 4 |
| 拆分非流形边 | 1 |
| 删除碎片面 | 3 |
| 分块 | 4 块并行 |
| 跨块共享边 | 35 |
| 跨块双边焊接映射组 | 30 |
| 共享顶点冲突 | 0 |
| 输出边界边 | 0 |
| 输出非流形边 | 0 |
| 法向最大偏差 | 25.4486°（容差 30°） |
| OpenGL | 4.5 Compatibility，llvmpipe |
| 强制终止恢复 | 复用 1 个磁盘块，最终 SHA-256 与不间断运行完全相同 |
| 损坏块恢复 | `E_RECOVERY_INVALID`，无输出网格 |
| 预算等待 | 激活 1 块时等待，预算释放后唤醒 |

证据文件：

- `artifacts/acceptance/acceptance_report.json`
- `artifacts/acceptance/artifact.msh`
- `artifacts/acceptance/repaired.msh.report.json`
- `artifacts/acceptance/preview.png`
- `artifacts/acceptance/traces/*.jsonl`
- `artifacts/acceptance/baseline.json`

验证仅使用本地确定性样本，不访问外部目标。

## 验收专用环境变量

仅在测试/验收脚本中使用：

- `MESH_REPAIR_CRASH_AFTER_COMMITS=N`：第 N 个新块提交后直接 `os._exit(137)`，模拟强制终止。
- `MESH_REPAIR_TEST_BLOCK_DELAY_MS=N`：在每个块的协作检查点等待 N 毫秒，用于在首块提交后触发取消。

这两个变量不会提高并行度、绕过预算或改变修复结果。

## Python API

```python
from mesh_repair import RepairConfig, RepairService

config = RepairConfig(
    tolerance=1e-6,
    block_shape=(2, 2, 1),
    parallelism=2,
    global_budget_bytes=8 * 1024 * 1024,
    task_budget_bytes=4 * 1024 * 1024,
)

report = RepairService("repair-work").repair_file(
    "samples/scan_asset.msh",
    "artifacts/repaired.msh",
    config=config,
)
```

后台运行与渲染：

```python
from mesh_repair.render import RenderLoop
import threading

service = RepairService("repair-work")
background = service.start_background(
    "samples/scan_asset.msh",
    "artifacts/async.msh",
    config=config,
)
stop_render = threading.Event()
renderer = RenderLoop(
    background.snapshot_store,
    stop_event=stop_render,
    preview_path="artifacts/async.png",
)
render_thread = threading.Thread(target=renderer.run)
render_thread.start()
report = background.join()
stop_render.set()
render_thread.join()
```

## 实现边界说明

- 样本是程序化本地扫描替身，不等同于真实扫描噪声分布。
- 孔洞使用质心扇形填补；边界环需为闭合链，开放链会报告但不填补。
- 内存预算是显式结构预算，不读取 `tracemalloc` 或 RSS，便于跨线程和跨进程复现调度决策。
- 分块提交允许完成顺序不同，但哈希链按实际原子提交顺序编号；恢复按哈希链和分块 ID 共同校验。
