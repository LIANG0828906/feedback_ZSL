# 开放世界地形渲染数据重建管线

这是一个从零实现的本地确定性地形烘焙管线，覆盖：

- 地形工程结构校验与逐块内容哈希；
- 顶点、法线、索引、材质权重纹理、散点实例缓冲和视锥包围盒烘焙；
- 接缝依赖 DAG、失效闭包与增量调度；
- 内容寻址块缓存、全局快照、断点恢复台账；
- 全量/增量/恢复路径的逐字节比对；
- PyQt5 分块预览与失效传播诊断面板；
- Windows 批处理启动脚本和无网络、无账号的复现实验。

## 快速开始

目标环境：Windows 10/11，Python 3.10+。

```bat
py -3.10 -m venv .venv
.venv\Scripts\activate
python -m pip install --upgrade pip
pip install -r requirements-ui.txt
pip install -r requirements-test.txt

python scripts\generate_sample.py --out artifacts\sample
python -m terrain_pipeline.cli bake artifacts\sample --cache artifacts\runtime\cache --mode full
python -m terrain_pipeline.cli edit artifacts\sample --cache artifacts\runtime\cache --edits artifacts\runtime\edits.json --tile T0005 --material rock 1.8
python -m terrain_pipeline.cli gui artifacts\sample --cache artifacts\runtime\cache --edits artifacts\runtime\edits.json
```

批处理入口：

```bat
scripts\generate_sample.bat
scripts\run_gui.bat artifacts\sample artifacts\runtime\cache
```

Linux/WSL 开发环境使用同一套 Python 入口，把 `python` 换成虚拟环境中的解释器即可。界面同时提供 Qt Painter 二维失效图和 ModernGL 三维地形页签；GL 上下文不可用时自动回退到二维诊断视图。

## 复现实验

```bash
.venv/bin/python -m pytest -q
.venv/bin/python scripts/verify_pipeline.py
```

本次实际结果：

- `37 passed`
- 端到端验收：`PASS`
- 12 块样本全部通过解析真值检查
- 增量与全量快照：逐字节一致
- 单线程与四工作线程：逐字节一致，失效传播记录一致
- 强制进程退出后无完整快照；重启恢复 11 个已提交块，仅重烘焙失败块；恢复结果与全量一致
- 全局内容哈希变化：整份快照作废并全量重算
- GUI offscreen：窗口成功创建

完整报告见 `reports/verification.md` 和 `reports/verification.json`。验证脚本会重新生成本地样本、所有缓存、对比结果和报告。
界面截图由 offscreen 冒烟测试写入 `reports/gui_smoke.png`。

## 工程输入

```text
project/
  manifest.json
  tiles/
    T0000/
      height.npy       float64, shape=(tile_size+1, tile_size+1)
      weights.npy      uint8,   shape=(tile_size+1, tile_size+1, layer_count)
      scatter.json     植被/道具散点
      truth.json       解析真值
```

`manifest.json` 关键字段：

- `format`: 固定为 `openworld-terrain-project/1`
- `seed`, `tile_size`, `grid_tiles`, `subdivisions`
- `materials`: 1 到 4 层，含 `name` 和可选 `default_param`
- `tiles[]`: `id`, `grid`, 四个文件路径, `height_shape`, `weights_shape`, 可选逐层 `material_params`

散点字段为 `id`, `kind`, `layer`, `x`, `y`, `scale`, `rotation`, `variant`。真值含若干局部高程点、逐材质覆盖比例和剔除后的散点计数。

生成器固定随机种子，并在世界空间生成高度/权重格点后再切块，因此共享接缝的源采样值逐位一致。散点使用“工程种子 + 块 ID”派生的独立随机流。

## 输出与快照

缓存布局：

```text
cache/
  snapshot.json                 # 完整的全局渲染清单，原子提交标记
  pending.json                  # 未完成运行的块台账
  blocks/<cache_key>/
    block.json                  # 缓存键、版本、依赖、产物哈希、真值检查
    positions.bin               # <f4, XYZ
    normals.bin                 # <f4, XYZ
    indices.bin                 # <u4
    weights.bin                 # RGBA8 材质纹理
    instances.bin               # <f4, 8 字段实例记录
    bounds.bin                  # <6f: min_x,min_y,min_z,max_x,max_y,max_z
  diagnostics/
    last_run.json               # 计时、线程数、失败状态，不属于确定性产物
    last_invalidation.json      # 最后一次失效传播记录
```

`snapshot.json` 即全局清单，包含每块的内容哈希、递归缓存键、依赖、顶点/实例数、包围盒和产物 SHA-256。块目录不可变；同一缓存键重复提交会先校验已有块，碰撞或不一致返回 `E_CACHE`。

确定性约束：

- 二进制统一小端和固定字段顺序；
- JSON 使用排序键、紧凑分隔符和固定换行；
- 块清单与快照不含时间戳、线程 ID、绝对路径或运行耗时；
- 并行度、顶点上限等资源参数不进入烘焙版本；
- 只有 `subdivisions`, `normal_strength`, `min_layer_weight` 和算法版本影响缓存键。

因此完整快照的 SHA-256 可用于跨路径逐字节验收。

## 烘焙算法

1. 对高度图做双线性细分，默认每源格 `subdivisions` 份。
2. 用中央差分从高度场计算 `dz/dx`, `dz/dy`，法线为 `normalize((-dzdx, -dzdy, 1))`；边缘使用单边差分。
3. 东/南接缝使用相邻块高度覆盖边界差分，使显式接缝依赖有实际数据意义。
4. 材质权重做同分辨率双线性采样，应用逐块逐层参数并量化到 RGBA8。
5. 散点按材质层权重阈值剔除；保留点按稳定 ID 顺序写入实例缓冲。
6. 包围盒覆盖地形顶点及实例的近似空间范围。
7. 在容差内检查解析高程点、材质覆盖比例和散点计数，失败返回 `E_TRUTH`。

单块顶点数超过 `max_vertices_per_thread` 时返回 `E_RESOURCE`，不会静默降低质量。最大并行度硬上限为 4，且受 CPU 数和任务数约束。

## 依赖、失效与缓存键

为避免无向接缝图形成环，采用确定性的“东/南接缝所有权”：

- 块 `(x,y)` 依赖 `(x+1,y)` 和 `(x,y+1)`；
- 依赖边同时表示 `seam_normal` 和 `seam_material`；
- 图为 DAG，按拓扑层分波次并行烘焙；
- 缓存键递归包含当前块内容哈希、烘焙版本和依赖块缓存键。

为保持依赖图无环，每个共享接缝由东/南侧块负责平滑采样，西/北侧使用单边差分；顶点位置仍来自同一世界采样，因此不会产生几何裂缝。

编辑某块的内部高度只改变该块；编辑共享接缝顶点会同步更新共享该世界顶点的所有块副本。材质参数编辑改变目标块。失效查询从实际 changed 块出发，沿反向依赖求完整闭包，记录：

- `trigger`: 高度/材质/全量重建触发原因；
- `changed_tiles`: 真正发生内容变化的块；
- `affected_tiles`: 需要失效的完整下游闭包；
- `reused_tiles`: 可直接复用持久化块的集合；
- `reasons`: 每个受影响块的传播来源与依赖边；
- `graph_edges` 和 `topological_order`。

这使漏失效和过度失效都能从记录中核对。多步编辑在真正烘焙前会累积触发原因，不会只保留最后一次编辑的失效范围。

## 增量、全量与恢复

`Pipeline.run(mode="auto"|"incremental"|"full")` 的语义：

- 同一进程内编辑后，旧快照仍可为未受影响块提供内容键匹配的持久化缓存；
- 增量路径只重烘焙失效闭包，按拓扑波次执行，主线程按稳定顺序提交块；
- 全量路径清空快照和块缓存，从零重算；
- 两条路径最终 `snapshot.json` 与全部块产物逐字节一致。

无修改的失效范围查询：

```python
report = pipeline.query_invalidation(["T0005"])
print(report.affected, report.reused, report.reasons)
```

重启时：

- 全局内容哈希或烘焙版本不匹配：整份已完成快照作废，不允许部分混用旧块；
- 未完成运行：只读取 `pending.json` 中已原子提交且缓存键匹配的块；
- 进程被强制终止后不承诺存在完整全局清单；重启可完成剩余块，但只有全部成功后才原子发布 `snapshot.json`；
- 单块异常只留下其他成功块和 pending 台账，失败块可重试，不会写入伪造块。

`pending.json` 每次更新也是原子的。实际顺序为：不可变块目录落盘 → pending 台账标记提交 → 全部成功后原子替换快照 → 删除 pending → 清理未引用块。

注意：由于业务要求“全局内容哈希变化时整份快照作废”，跨进程在编辑后直接增量复用不是允许路径；跨进程编辑必须走匹配 pending 的断点恢复，或全量重算。这与“快照整体版本匹配”约束一致。

## 比对错误

```python
from terrain_pipeline.compare import compare_snapshots

compare_snapshots("cache_a", "cache_b")
```

比较按稳定块顺序和产物顺序执行，定位首个不一致的 `tile_id` 与产物类型，并抛出 `E_DIFF`。产物顺序为：

`block.json`, `positions.bin`, `normals.bin`, `indices.bin`, `weights.bin`, `instances.bin`, `bounds.bin`

CLI：

```bat
python -m terrain_pipeline.cli compare cache_a cache_b
```

## 错误码

| 错误码 | 场景 |
|---|---|
| `E_SCHEMA` | 字段缺失、块尺寸/层数不匹配、权重越界、路径越界等 |
| `E_DECODE` | 工程 JSON 或 NumPy 文件损坏/无法解码 |
| `E_BAKE` | 单块内部异常 |
| `E_RESOURCE` | 单块顶点数超过每线程上限 |
| `E_TRUTH` | 高程、材质覆盖或散点计数超出真值容差 |
| `E_CACHE` | 快照/块缺失、缓存键碰撞或损坏 |
| `E_DIFF` | 两条快照路径产物不一致 |
| `E_CANCEL` | 用户取消 |
| `E_STATE` | 非法调用状态，例如编辑未配置文件 |

`E_SCHEMA` 会尽量带 `tile_id` 和 `field`。格式化字符串类似：

```text
E_SCHEMA tile=T0000 field=tiles[0].height_file: missing required field
```

## GUI 诊断面板

```bat
python -m terrain_pipeline.cli gui artifacts\sample --cache artifacts\runtime\cache --edits artifacts\runtime\edits.json
```

面板包含：

- 分块地形着色图，叠加依赖箭头；
- ModernGL 三维地形视图，展示已提交的顶点/法线/索引缓冲；
- 绿色命中/复用、蓝色重烘焙/恢复、橙色失效、红色失败；
- 每块顶点数、实例数、耗时和包围盒；
- 高度点编辑、逐层材质参数编辑；
- 增量重烘焙、全量重算、取消；
- 本次 affected/reused 集合和重烘焙耗时。

编辑会先写入可恢复的 edits JSON，再触发失效查询和增量烘焙。未失效块不会重写；全量重算需要显式确认。

## CLI 摘要

```text
python -m terrain_pipeline.cli generate [--out DIR]
python -m terrain_pipeline.cli bake PROJECT --cache DIR [--mode auto|incremental|full]
python -m terrain_pipeline.cli edit PROJECT --cache DIR --edits FILE --tile ID --height X Y VALUE
python -m terrain_pipeline.cli edit PROJECT --cache DIR --edits FILE --tile ID --material LAYER VALUE
python -m terrain_pipeline.cli compare CACHE_A CACHE_B
python -m terrain_pipeline.cli inspect CACHE
python -m terrain_pipeline.cli gui PROJECT --cache DIR
```

常用资源参数：`--workers 1..4`, `--max-vertices-per-thread`, `--subdivisions`, `--normal-strength`, `--min-layer-weight`。

## 测试覆盖

测试包含 37 项，覆盖：

- 正常加载、内容哈希、编辑持久化；
- 缺字段、尺寸错误、层数错误、权重越界、损坏 NPY/JSON；
- 真实几何、法线归一化、产物尺寸、无时间戳/线程字段；
- 依赖 DAG 与精确失效闭包、共享接缝编辑；
- 增量/全量逐字节一致、缓存全命中、`E_DIFF` 首差异定位；
- 单块失败隔离与重试、断点恢复、全局哈希/版本失效、取消；
- 单线程/并行一致、资源上限；
- CLI 与 PyQt5 offscreen 冒烟测试。

实际测试命令与结果：

```text
$ .venv/bin/python -m pytest -q
37 passed

$ .venv/bin/python scripts/verify_pipeline.py
status=0
```

本仓库的 `reports/verification.md` 保存了本次实际运行环境、快照 SHA-256、失效集合、恢复计数和错误码证据。当前验证主机为 WSL2/Linux；PyQt5 窗口通过 offscreen 冒烟测试，Windows 原生窗口需在目标机运行上面的 `gui` 命令确认桌面主题与字体效果。
