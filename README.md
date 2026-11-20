# 本地离线反向成像工具

一个仅依赖本地文件、NumPy 与 PyTorch CPU 的确定性反向成像实现。场景包含多个球体和一个平面；相机内外参、几何、漫反射材质、光源方向与强度全部是可微参数。正向管线使用解析求交、Lambert 光照和软阴影，随后计算目标图像逐像素 L2 损失。

本实现重点解决三个工程问题：

- 按参数组构建显式依赖图，只重算受影响节点；复用节点受参数版本和内容哈希双重保护。
- 拟合支持协作式取消，每步以“临时文件 + `os.replace`”原子提交带哈希链的检查点。
- 同一场景只允许一个拟合任务；训练线程与查询线程通过不可变快照并行，查询路径不上锁。

## 目录

```text
inverse_render/
  scene.py           场景 JSON 解析、结构/内容哈希
  params.py          参数路径、分组、版本与检查点值
  rendering.py       解析求交、可见性、软阴影、Lambert、L2
  graph.py           依赖 DAG、环检测、影响集合、边差异
  engine.py          增量前向/反向、缓存校验、全量一致性比较
  checkpoint.py      原子检查点、逐提交哈希链、恢复校验
  snapshot.py        不可变快照、无锁发布/查询
  fit.py             分组 SGD、取消、轨迹日志、服务级互斥
  generate.py        确定性样本生成器
  cli.py             脚本化演示与检查入口
tests/               验收测试
artifacts/demo/      固定种子的实际演示输出
```

## 环境准备

已在 Python 3.11.18、NumPy 2.4.6、PyTorch 2.7.1+cpu、pytest 8.4.2 上验证。

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install --upgrade pip
.venv/bin/python -m pip install 'torch==2.7.1+cpu' \
  --index-url https://download.pytorch.org/whl/cpu
.venv/bin/python -m pip install 'numpy>=1.26,<3' 'pytest>=8,<9'
.venv/bin/python -m pip install -e .
.venv/bin/python -m pytest -q
```

也可使用 `requirements-cpu.txt`。不需要 Docker、GPU、网络素材或外部目标。

## 快速演示

```bash
.venv/bin/python -m inverse_render.cli env-check
.venv/bin/python -m inverse_render.cli demo \
  --out artifacts/demo --seed 20261121 --steps 4
```

`demo` 会：

1. 生成 16x12、24x18、32x24 三种分辨率，4、6、8 个参数组，耦合等级 0、1、2 的样本。
2. 修改材质组，比较增量复用与全新参数的从零全量重算。
3. 绕过版本号直接修改参数，验证陈旧缓存拒绝与全量降级。
4. 在子进程中于第 2 步提交后执行 `os._exit(137)`，再仅依赖磁盘检查点恢复。
5. 用相同参数在两个目录重复拟合，逐字节比较检查点与轨迹日志。

结果写入 `artifacts/demo/demo_report.json`。

## 场景格式

`scene.json`：

```json
{
  "schema_version": 1,
  "name": "coupled_small",
  "resolution": [16, 12],
  "camera": {
    "position": [0.18, 0.52, 4.15],
    "target": [0.0, -0.08, 0.0],
    "up": [0.0, 1.0, 0.0],
    "fov_deg": 48.0
  },
  "geometry": {
    "spheres": [
      {"name": "primary", "center": [-0.82, -0.1, 0.0], "radius": 0.63},
      {"name": "accent", "center": [0.82, 0.06, -0.05], "radius": 0.53}
    ],
    "plane": {
      "name": "floor",
      "point": [0.0, -1.02, 0.0],
      "normal": [0.0, 1.0, 0.0]
    }
  },
  "materials": {
    "primary": {"albedo": [0.84, 0.22, 0.12]},
    "accent": {"albedo": [0.12, 0.34, 0.88]},
    "floor": {"albedo": [0.48, 0.5, 0.54]}
  },
  "light": {"direction": [-0.55, -0.82, -0.22], "intensity": 2.55},
  "shadow_softness": 0.025,
  "coupling_level": 0,
  "target": {"path": "target.npy"}
}
```

目标数组必须是形状 `(height, width, 3)`、`float64` 的本地 `.npy` 文件。路径相对场景 JSON 解析。

`groups.json` 可以任意拆分参数，但每个可微参数必须恰好属于一个组：

```json
{
  "schema_version": 1,
  "groups": [
    {"id": "camera", "kind": "camera", "parameters": ["camera.*"]},
    {"id": "geometry.a", "kind": "geometry", "parameters": ["geometry.primary.*"]},
    {"id": "geometry.b", "kind": "geometry", "parameters": ["geometry.accent.*"]},
    {"id": "geometry.floor", "kind": "geometry", "parameters": ["geometry.floor.*"]},
    {"id": "material.a", "kind": "material", "parameters": ["material.primary.*"]},
    {"id": "material.b", "kind": "material", "parameters": ["material.accent.*"]},
    {"id": "material.floor", "kind": "material", "parameters": ["material.floor.*"]},
    {"id": "light", "kind": "light", "parameters": ["light.*"]}
  ]
}
```

支持的参数路径：

| 路径 | 形状 | 参数组类型 |
| --- | --- | --- |
| `camera.position` | `[3]` | `camera` |
| `camera.target` | `[3]` | `camera` |
| `camera.up` | `[3]` | `camera` |
| `camera.fov_deg` | `[]` | `camera` |
| `geometry.<object>.center` / `.radius` | `[3]` / `[]` | `geometry` |
| `geometry.<plane>.point` / `.normal` | `[3]` / `[3]` | `geometry` |
| `material.<object>.albedo` | `[3]` | `material` |
| `light.direction` | `[3]` | `light` |
| `light.intensity` | `[]` | `light` |

不支持或未覆盖的参数、重复分组、未知模式、空组均返回 `E_GRAPH_INVALID`。非法场景返回 `E_SCENE_INVALID`。

## 可微正向管线

计算节点固定为：

```text
intersections -> visibility -> shadow_factor -> lighting -> loss
```

- 光线：由相机位置、朝向、`up` 和垂直 FOV 生成像素中心射线。
- 求交：球体使用二次方程解析解，平面使用射线-平面解析解；最近命中选择驱动可见性。
- 软阴影：从命中点向光源发射阴影射线，用判别式、近交点门限的 sigmoid 形成连续遮挡；多个遮挡者按透射率相乘。
- 光照：`albedo * intensity * max(N·L, 0) * visibility * shadow / pi`。
- 损失：`mean((rgb - target)^2)`。

所有参数和上述节点都保留 PyTorch 自动微分图。图像与梯度使用固定字段顺序导出：

```bash
.venv/bin/python -m inverse_render.cli render \
  --scene artifacts/demo/samples/coupled_small/scene.json \
  --groups artifacts/demo/samples/coupled_small/groups.json \
  --output artifacts/demo/render.json
```

顶层字段固定为 `schema_version, scene, resolution, node_order, nodes, loss, loss_hash, content_hash`；梯度固定为 `schema_version, group_order, groups, loss, content_hash`。

## 依赖图与增量复用

依赖边包括：

```text
group:camera     -> intersections
group:geometry   -> intersections, shadow_factor
group:material   -> lighting
group:light      -> shadow_factor, lighting
intersections    -> visibility, shadow_factor, lighting
visibility       -> shadow_factor, lighting
shadow_factor    -> lighting
lighting         -> loss
```

参数组或场景结构变化后可重建图谱，并得到 `added`、`deleted`、`changed` 三类边差异：

```bash
.venv/bin/python -m inverse_render.cli graph \
  --scene artifacts/demo/samples/coupled_small/scene.json \
  --groups new-groups.json \
  --previous-groups old-groups.json
```

`IncrementalEngine.reconfigure(...)` 同样返回 `GraphDiff` 并清空旧缓存。环、悬空边、重复边、组节点失配均返回 `E_GRAPH_INVALID`。

每次评估使用以下签名判断缓存：

```text
graph_version
+ 每个上游参数组的版本号
+ 每个上游参数的内容 SHA-256
+ 该节点输出的内容 SHA-256
```

只有依赖图中从被改组可达的节点会重建前向图并执行反向；其它节点直接复用缓存张量及其保留的自动微分中间量。因此材质修改只影响 `lighting/loss`，光源修改影响 `shadow_factor/lighting/loss`，相机或几何修改影响全部下游节点。

若节点不在预期受影响集合中，但其上游版本或内容哈希已经变化，缓存会被拒绝，并强制整图重算。轨迹记录 `degraded=true`、`degraded_reason`、`stale_nodes`，界面字段为 `ui_badge=FULL_RECOMPUTE`。不会静默返回陈旧值。

增量与全量的默认声明容差为：

- 损失绝对误差：`1e-8`
- 每组任一梯度分量绝对误差：`1e-7`

超差抛出 `E_REUSE_MISMATCH`，详情包含首个异常的 `node`、`group`、参数路径、误差和容差。

## 拟合、取消与检查点

```bash
.venv/bin/python -m inverse_render.cli fit \
  --scene artifacts/demo/samples/coupled_small/scene.json \
  --groups artifacts/demo/samples/coupled_small/groups.json \
  --state-dir artifacts/demo/my_fit \
  --steps 8 --lr 0.02 --schedule rotate
```

`--schedule` 支持：

- `all`：每步更新所有组。
- `rotate`：按组顺序轮换，每步只更新一个组。
- 组 ID：每步只更新指定组。
- 逗号分隔的组 ID 列表。

取消事件只在步边界检查。已完成步会提交；未完成步不会留下半成品。优化器为确定性无状态 SGD，取消/异常时立即释放临时状态。

检查点位于 `state_dir/checkpoint.json`，写入路径为预先约定的 `state_dir/checkpoint.json.tmp`。流程是 `write -> flush -> fsync -> os.replace -> fsync(dir)`。强制终止最多留下 `.tmp` 文件，恢复仍读取上一个完整提交。

检查点包含：

- 模式版本与格式标识
- 步号、场景哈希、依赖图版本
- 组顺序固定的全部当前参数值
- 累计损失、最后一步损失
- 五个计算节点的缓存内容哈希
- 前一次提交哈希
- 每次提交的 `payload_hash / previous_hash / record_hash`
- 顶层自哈希

恢复会逐项校验版本、场景、图版本、字段、组集合、每个历史记录的本地哈希、历史链接、当前 payload 哈希和顶层自哈希。失败返回 `E_CKPT_INVALID`，并定位首个异常 `step`。`resume` 从 `checkpoint.step + 1` 开始，已提交步不会重复执行。

```bash
.venv/bin/python -m inverse_render.cli query \
  --scene artifacts/demo/samples/coupled_small/scene.json \
  --groups artifacts/demo/samples/coupled_small/groups.json \
  --state-dir artifacts/demo/my_fit
```

## 并发与快照

`FitLock` 同时使用进程内注册表和 `flock`，同一场景的第二个拟合任务返回 `E_BUSY`。锁文件位于系统临时目录下、以用户 ID 和场景哈希隔离。

`SnapshotPublisher` 在完整构造参数、损失、检查点哈希后才执行一次引用替换。查询只读取当前引用并深拷贝，不加锁，因此不会被发布替换阻塞。

- 没有已发布快照：`E_SNAPSHOT_STALE`
- 查询旧快照句柄：`E_SNAPSHOT_STALE`
- 查询已构造但未发布的句柄：`E_SNAPSHOT_STALE`

`FitService.start(...)` 启动后台拟合，`query(handle)` 可在训练期间读取最近已发布检查点，`cancel()` 请求协作式取消。

## 确定性

- 样本生成、渲染、SGD、检查点、轨迹均由固定顺序和固定种子驱动。
- 不记录时间戳、随机临时名或绝对路径到检查点/轨迹。
- 轨迹为 canonical JSON Lines，每条字段顺序固定。
- 相同场景、种子和操作序列下，`checkpoint.json` 与 `trace.jsonl` 逐字节一致。

轨迹字段：

```text
step, modified_groups, affected_nodes, reused_nodes, recomputed_nodes,
loss, gradient_norm, content_hash, graph_version, degraded,
degraded_reason, stale_nodes, ui_badge
```

## 主要错误码

| 错误码 | 含义 |
| --- | --- |
| `E_SCENE_INVALID` | 场景、目标数组或本地路径非法 |
| `E_GRAPH_INVALID` | 分组、依赖边、节点或环非法 |
| `E_REUSE_MISMATCH` | 增量与全量损失/梯度超差 |
| `E_CKPT_INVALID` | 检查点版本、字段、哈希链或断链异常 |
| `E_BUSY` | 同一场景已有拟合任务 |
| `E_SNAPSHOT_STALE` | 快照尚未发布或已过期 |
| `E_CONFIG_INVALID` | 学习率、步数、调度等配置非法 |
| `E_IO` | 本地读写失败 |

CLI 错误以单行 JSON 写入 stderr，并返回稳定退出码；`E_BUSY=3`、`E_SNAPSHOT_STALE=4`、`E_CKPT_INVALID=5`、`E_REUSE_MISMATCH=6`。

## 实际验证

验证命令：

```bash
.venv/bin/python -m pytest -q
.venv/bin/python -m inverse_render.cli demo \
  --out artifacts/demo --seed 20261121 --steps 4
```

本机结果：

- `17 passed`。
- 材质修改只重算 `lighting, loss`，`intersections, visibility, shadow_factor` 复用。
- 增量与全量损失误差和四组梯度最大误差均为 `0.0`。
- 未版本化参数注入产生 `upstream_content_changed_without_invalidation`，`ui_badge=FULL_RECOMPUTE`，降级后与全量结果一致。
- 第 2 步强制退出返回码 `137`；恢复从第 2 步继续，仅执行第 3、4 步，历史步号严格为 `0,1,2,3,4`。
- 崩溃恢复后的最终检查点与轨迹，和不间断的 4 步运行逐字节一致。
- 两次独立拟合的检查点 SHA-256 与轨迹 SHA-256 分别完全一致。

完整机器可读结果见 `artifacts/demo/demo_report.json`。
