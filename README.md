# 机械设计曲面查看器：依赖感知的增量重算

本项目实现了一个本地、确定性、可恢复的曲面显示产物流水线。它把曲面参数和显示产物建模为显式依赖图，只重算依赖闭包内失效的产物；持久化采用内容寻址文件和原子提交；渲染侧只读取不可变快照。

实现没有使用 Docker、数据库或外部数据。样本、基线产物和轨迹日志均由本地脚本生成。

## 快速开始

环境要求：Python 3.11+、NumPy、PyOpenGL、PyOpenGL-accelerate。

```bash
python3 -m pip install -r requirements.txt

# 生成 seed=0 的本地确定性样本
python3 -m surface_viewer.cli generate --output samples --seed 0

# 运行全部增量、恢复、并发、确定性测试
python3 -m unittest discover -s tests -v

# 生成基线产物、内容哈希与轨迹日志
python3 -m surface_viewer.cli baseline \
  --samples samples --output baseline --preview-backend software
```

常用命令：

```bash
python3 -m surface_viewer.cli validate samples/tiles_2x2/assembly.json
python3 -m surface_viewer.cli graph samples/tiles_2x2/assembly.json
python3 -m surface_viewer.cli run \
  --case samples/tiles_2x2 --store work/surface-store
python3 -m surface_viewer.cli recover \
  --case samples/tiles_2x2 --store work/surface-store
```

`run` 和 `recover` 默认启用同参数全量重算对照，任何增量产物差异都会返回 `E_INCR_MISMATCH`。使用 `--preview-backend opengl` 可切换到 EGL surfaceless 离屏 OpenGL；默认 `software` 后端用于跨机器确定性基线。

## 样本格式

```json
{
  "schema_version": 1,
  "assembly_id": "sample-tiles_2x2",
  "patches": [
    {
      "id": "p0000",
      "block_id": 0,
      "surface_type": "bezier",
      "precision_level": 2,
      "flip_normal": false,
      "control_points": [
        [[0.0, 0.0, 0.0], [0.33, 0.0, 0.1]],
        [[0.0, 0.33, 0.2], [0.33, 0.33, 0.3]]
      ]
    }
  ]
}
```

约束：

- `id` 非空且唯一，`block_id` 为非负整数且装配件内唯一。
- `surface_type` 支持 `bezier`、`bspline`、`nurbs`；NURBS 控制点可带第 4 个权重分量。
- 控制点网格至少 `2 x 2`，各行维度必须一致，坐标必须是有限数值。
- `precision_level` 范围为 `0..4`，对应每边 `4/8/16/24/32` 段。
- 任一字段缺失、维度不一致、精度越界或块编号重复均抛出 `E_SURF_INVALID`，异常 `field` 精确指向如 `patches[2].precision_level`。

每个曲面片和整个装配件都计算规范 JSON 的 SHA-256 版本哈希；每个参数节点也单独记录版本哈希。

## 依赖图与失效

每个曲面片有 4 个参数节点和 4 个产物节点：

| 产物 | 直接参数依赖 | 产物依赖 |
|---|---|---|
| `mesh` | 控制点、曲面类型、精度 | 无 |
| `normals` | 控制点、曲面类型、精度、法向翻转 | 无 |
| `density` | 曲面类型、精度 | 无 |
| `preview` | 控制点、曲面类型、精度、法向翻转 | `mesh`、`normals`、`density` |

每条参数依赖边都携带源参数节点的版本哈希。4 个曲面片的样本得到 32 个节点、64 条边。图构建后会检查所有必选边；缺边时 `assert_publishable()` 和原子提交都会以 `E_DEP_GRAPH_INCOMPLETE` 拒绝发布，并给出首个缺失边及目标产物。

修改后从变更参数节点沿出边做广度优先遍历，得到依赖闭包和受影响产物。未受影响产物直接复用缓存对象和内容哈希。若产物不在缓存、缓存签名异常或图结构发生无法证明安全的变化，则该产物及其下游会保守重算，并在失效原因中标为 `conservative recompute`。

边界控制点变化会同步到相邻曲面片的共享边界控制点。几何层使用同一组共享控制点和规范边界采样，测试中接缝顶点最大误差为 `0.0`。

## 几何与局部预览

- 失效块按参数域重新采样。Bezier 使用 Bernstein 基，B-spline 使用 clamped Cox-de Boor 基，NURBS 使用有理基。
- 法向使用参数域中心差分：`P(u±eps,v)` 与 `P(u,v±eps)` 的切向叉积，然后归一化；翻转标记直接作用于最终法向。
- 网格、法向、细分密度和预览块均序列化为固定字节格式，内容哈希为 SHA-256。
- 每个预览块为 `64 x 64` RGBA 离屏缓冲。整屏预览按块编号布局，精度变化只替换对应脏矩形；最终整屏缓冲与全量重渲逐字节比较。
- 每次增量结果都与同参数全量结果逐字节比较。网格或法向不同时，错误详情包含 `first_difference_vertex`、行列号和两侧分量值。

## 原子提交与恢复

产物目录：

```text
store/
  artifacts/<sha256>.bin
  tmp/
  manifest.json
```

每次重算先写 `tmp/`、`fsync`，再 `os.replace` 到内容寻址文件；所有产物完成后才原子替换清单。清单记录：

- `commit_seq`、装配件版本哈希；
- 每个产物的参数签名、参数版本映射和内容哈希；
- 当前提交内按产物排序的哈希链；
- 跨提交的 `previous_chain_head -> chain_head` 历史链。

恢复只读取清单引用的已提交 blob。清单 schema/版本不支持、哈希断链、引用文件缺失或内容哈希不符，均返回 `E_RECOVERY_INVALID`。清单之外的孤儿 blob 和临时文件不会被当作已提交数据。当前装配件参数版本变化时，只有参数签名变化的产物会被补算。

进程强制终止验证：

```bash
python3 -m surface_viewer.crash_worker \
  --assembly samples/tiles_2x2/assembly.json \
  --store work/crash-store \
  --operations samples/tiles_2x2/operations.json \
  --operation-index 0 --crash-after-artifacts 1

python3 -m surface_viewer.cli recover \
  --case samples/tiles_2x2 --store work/crash-store
```

子进程在首个产物原子重命名后、清单提交前以退出码 `23` 终止。Windows 下 `os._exit` 对应 `TerminateProcess` 语义，也可用等效命令强制结束：

```bat
taskkill /PID <pid> /F
```

## 并发与资源边界

- 重算在后台线程写独立缓冲，完成后一次性发布 `Snapshot`；产物映射和整屏预览均为不可变对象。
- 渲染线程用 `render_fixed_fps` 按固定帧率读取当前快照，不会观察到一半网格或一半法向。
- 旧快照句柄被新快照替代后，读取返回 `E_STALE_HANDLE`。
- 同一装配件已有后台重算时，再次提交返回 `E_BUSY`。
- 默认资源上限：10,000 曲面片、每片 250,000 顶点、40,000 产物、256 MiB 预览；超出返回 `E_RESOURCE_LIMIT`。

## 确定性与轨迹

`samples/` 包含不同曲面片数量、块布局、精度和修改序列的 4 组样本。`baseline/<case>/trace.jsonl` 每行包含：

- 操作序号、提交序号、新旧链头；
- 参数版本哈希、依赖闭包、受影响/复用/重算产物；
- 每个失效产物的原因、内容哈希、脏矩形；
- 整屏预览哈希和全量对照结果。

日志不含时间戳，也不使用文件大小作为观测标识。相同 seed 和操作序列下，产物 blob、清单、轨迹日志和 `baseline_hashes.json` 逐字节一致。

## 实际验证结果

本工作区实测环境：

- Python `3.14.4`
- NumPy `2.5.5`
- PyOpenGL `3.1.10`
- PyOpenGL-accelerate `3.1.10`
- EGL surfaceless / Mesa：OpenGL `4.5 (Compatibility Profile) Mesa 26.0.8`

验证结果：

- `python3 -m unittest discover -s tests -v`：31 项测试全部通过。
- 2×2 样本接缝位置最大误差：`0.0`。
- 控制点微调：仅失效该片的 `mesh/normals/preview`，`13` 个无关产物复用。
- 精度等级变化：仅影响对应曲面片，整屏只更新一个 `64 x 64` 脏矩形，最终预览与全量重渲逐字节一致。
- 强制终止：子进程退出码 `23`；旧清单仍为提交 `1`。恢复后新提交为 `2`，从磁盘复用 `13` 个产物，只补算 `3` 个失效产物。
- 确定性：全新生成的基线目录与仓库基线共 `108` 个文件逐一比较，产物、清单与轨迹日志全部逐字节一致。
- OpenGL：EGL surfaceless FBO 路径通过增量/全量逐字节对照。

基线摘要见 `baseline/baseline_hashes.json`，每个案例的完整逐提交记录见 `baseline/<case>/trace.jsonl`。

## 代码结构

```text
surface_viewer/
  model.py         参数解析、字段校验、版本哈希、共享边界传播
  graph.py         节点/边、图差异、闭包失效、保守重算
  geometry.py      曲面采样、中心差分法向、接缝共享控制点
  artifacts.py     产物二进制格式、哈希、首个顶点差异定位
  preview.py       软件栅格化与 EGL/OpenGL 离屏预览
  engine.py        增量重算、缓存复用、全量对照
  store.py         原子 blob、哈希链清单、恢复校验
  concurrency.py   不可变快照、陈旧句柄、E_BUSY、固定帧率读取
  runtime.py       引擎/存储/快照/后台任务编排
  samplegen.py     样本、操作序列、基线与轨迹生成
tests/             31 项自动化测试
tools/             样本与基线脚本入口
```
