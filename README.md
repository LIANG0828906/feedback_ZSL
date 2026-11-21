# 战术战场拾取调试工作台

本项目实现一个可直接在本地运行的回合制战术游戏拾取与放置调试器。核心使用
Python 3.10+ 标准库，保证固定种子、固定操作序列下场景、拾取结果、放置结果和
轨迹日志逐字节一致；PyQt5 仅用于图形工作台，NumPy 为可选数值环境依赖。

实现重点：

- 自校验二进制场景与区域快照格式。
- 屏幕坐标到世界射线的确定性反投影。
- 单位 AABB、单位胶囊、掩体三角形、地面四类求交。
- 不可变区域快照、单调版本、内容哈希、句柄式并发隔离。
- 原子批量编辑：完整校验并重建 BVH 后才整体替换发布。
- 草拟区域可通行、单位重叠、掩体穿模判定。
- 内容哈希链轨迹、按历史版本精确回放。
- 入口级异常注入，五类故障各有独立业务码且不写结果文件。
- 确定性样本生成器、基线文件和 PyQt5 图形工作台。

## 快速开始

核心脚本不依赖第三方包：

```bash
python3 -m tactical_pick generate --seed 7 --samples samples --baselines baselines
python3 -m tactical_pick verify --samples samples --baselines baselines
python3 -m unittest discover -s tests -v
```

图形工作台和完整环境：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m tactical_pick generate --seed 7
.venv/bin/python -m tactical_pick gui --samples samples --baselines baselines
```

当前执行环境已创建 `.venv`，其中安装了 `numpy==2.5.5` 与
`PyQt5==5.15.11`。若虚拟环境不带 `pip`，可使用
`uv pip install --python .venv/bin/python -r requirements.txt`。
本机 `xcb` 插件缺少部分系统共享库，但 Wayland 会话可用；`run_gui` 会在未显式
指定 `QT_QPA_PLATFORM` 且存在 `WAYLAND_DISPLAY` 时自动选择 `wayland`。
无显示环境可只用脚本入口，或执行离屏测试：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest tests.test_gui -v
```

## 命令行

```text
python -m tactical_pick generate  [--seed N] [--samples DIR] [--baselines DIR]
python -m tactical_pick generate  --inject KIND ...
python -m tactical_pick verify    [--samples DIR] [--baselines DIR]
python -m tactical_pick inject    --kind KIND [--seed N]
python -m tactical_pick inspect   [--samples DIR]
python -m tactical_pick gui       [--samples DIR] [--baselines DIR]
```

`generate` 先在内存中构建并回放全部样本，成功后才写文件。`--inject` 与
`inject` 在输入入口触发异常，失败时不会创建 `samples/`、`baselines/` 或其他
结果文件。

## 目录

```text
tactical_pick/
  model.py        不可变场景模型与严格校验
  scene_io.py     TPSC 场景与 TPSV 快照二进制格式
  bvh.py          确定性二叉 BVH
  camera.py       相机校验、反投影、投影
  picking.py      单位/掩体/地面最近命中
  versioning.py   版本、内容哈希、句柄、原子批量发布
  placement.py    草拟区域几何与业务冲突判定
  trace.py        JSONL 哈希链、加载校验、版本回放
  injection.py    仅包装入口的故障注入适配器
  generator.py    场景/操作序列/基线生成与逐字节回归
  gui.py          PyQt5 俯视工作台（延迟导入）
samples/          确定性输入场景与操作序列
baselines/        轨迹日志、结果与基线清单
tests/            标准库 unittest 回归测试
```

## 坐标与相机约定

- 世界坐标为右手系，`Z` 向上；地面是 `z = ground_height` 的平面。
- 格子索引为 `(row, col)`，格心为
  `origin + (col + 0.5, row + 0.5) * cell_size`。
- 可通行位掩码使用 LSB-first，`bit = row * cols + col`，长度必须严格等于
  `ceil(cols * rows / 8)`，未使用高位必须为 0。
- 屏幕坐标为左上角原点，`x` 向右、`y` 向下；视口闭区间
  `[0, viewport_width] x [0, viewport_height]`。
- 相机方向由 `position -> target` 构造，`up` 只用于建立正交基；FOV 为垂直
  FOV。射线方向在返回前归一化。

## 二进制格式

两种文件均使用小端信封：

```text
magic[4] | format u16 | flags u16 | payload_length u32 | sha256(payload)[32]
payload...
```

- 场景 magic：`TPSC`，payload 包含结构版本、地面高度、网格参数与位掩码、
  顶点池、单位、掩体面片。
- 快照 magic：`TPSV`，payload 包含区域版本、内容哈希、场景 payload。

加载顺序是这样：先校验信封、长度和 SHA-256，再解析字段，然后校验三角面索引、
实体 ID、AABB/胶囊关系、位掩码和场景不变量。任何失败返回
`E_SCENE_INVALID` 或 `E_SNAPSHOT_INVALID`，`location` 中带 `field`、
`entity_id`、索引或期望/实际长度。

## 拾取

`pick_from_store(store, camera, screen, expected_version=...)`：

1. 校验相机内外参和屏幕坐标，失败返回 `E_RAY_INVALID`。
2. 生成世界射线。
3. 在单位 BVH 与掩体 BVH 中做宽相，再分别做 AABB、胶囊、三角形窄相。
4. 与地面平面求交。
5. 按 `(距离, 类型优先级, 实体编号)` 选最近命中，类型优先级为
   `unit < cover < ground`。
6. 返回命中实体、点、法向、距离、区域版本和内容哈希。

近乎相切的表面命中会附带 `W_RAY_GRAZING` 诊断，但不会丢弃有效交点。相同相机
与屏幕坐标的重复查询不引入随机数、时间或浮点归约差异，字段完全一致。

## 版本与并发

- 初始发布版本为 `1`。每次成功批量修改产生 `version + 1`。
- 内容哈希是完整已校验场景二进制 payload 的 SHA-256，不包含区域版本本身。
- 编辑在锁外基于不可变旧场景和不可变操作序列构建新场景与 BVH，最后用短临界区
  比较基版本并整体替换当前引用。
- 任一操作失败时，已应用的前序操作只存在于局部临时场景中，当前发布状态不变。
- 查询获取 `SnapshotHandle` 后固定读取完整快照。发布新版本不会中断已获取的旧
  句柄；句柄释放后再次查询返回 `E_STALE_HANDLE`。
- 新查询携带旧 `expected_version` 时返回 `E_VERSION_STALE`，不回退猜测。

支持的批量操作包括：`translate_unit`、`set_unit_position`、
`set_unit_active`、`set_unit_bounds`、`replace_units`、
`set_walkable_cells`、`set_walkable_mask`、`replace_cover_geometry`、
`set_cover_vertices`、`set_ground_height`、`set_structure_version`。

## 放置判定

`check_placement(store, vertices, expected_version=...)` 在同一快照上完成：

1. 顶点数量、有限性、网格边界、零长度边、重复顶点、自相交、零面积检查。
2. 计算草拟多边形实际覆盖的格子。
3. 检查全部覆盖格子可通行。
4. 检查单位 AABB 足迹和胶囊足迹。
5. 检查掩体三角形投影。

返回 `valid`、`failed_vertex`、`conflict_entity`、`conflict_point`、
`covered_cells`、版本与内容哈希。几何本身非法时中止为
`E_REGION_INVALID`；业务冲突时返回：

- `E_REGION_UNWALKABLE`
- `E_REGION_UNIT_OVERLAP`
- `E_REGION_COVER_OVERLAP`

## 轨迹格式

轨迹为 UTF-8 JSONL，首行为固定 header，末行为 footer：

```text
header
record 0
record 1
...
footer
```

每条记录包含连续 `seq`、屏幕坐标、完整相机参数、射线、区域版本、区域内容哈希、
命中实体/点/法向/距离、诊断、`prev_hash` 和 `entry_hash`。`entry_hash` 是对
去掉 `entry_hash` 后的规范化 JSON 做 SHA-256；链首为 64 个 `0`。footer 记录
记录数与链头。没有时间戳字段。

`load_trace_bytes` 会拒绝空文件、截断、缺失 footer、错误记录数、缺失版本、
断链和字段篡改。`replay_trace` 按记录中的历史版本取不可变快照并重新查询，
逐字段比较射线、命中与版本归因；结果不一致返回 `E_TRACE_INVALID`。

## 异常注入

注入由 `injection.py` 在输入入口包装，不修改拾取或版本管理分支：

| 运行参数 | 顶层业务码 | 底层成因 |
|---|---|---|
| `degenerate-bounds` | `E_INJECT_DEGENERATE_BOUNDS` | `E_SCENE_INVALID` |
| `zero-ray` | `E_INJECT_ZERO_RAY` | `E_RAY_INVALID` |
| `out-of-range-screen` | `E_INJECT_SCREEN_RANGE` | `E_RAY_INVALID` |
| `self-intersect-region` | `E_INJECT_SELF_INTERSECT` | `E_REGION_INVALID` |
| `corrupt-snapshot` | `E_INJECT_CORRUPT_SNAPSHOT` | `E_SNAPSHOT_INVALID` |

错误 JSON 的 `location.injection_point` 给出注入位置，`cause_code` 给出底层校验
码。注入命令在写任何结果文件前退出，返回码为 `2`。

## 样本与基线

生成器输出三种网格与相机组合：

- `8x8`，960x600，FOV 48°
- `12x10`，1024x640，FOV 45°
- `16x14`，960x600，FOV 52°

每个样本包含 AABB 单位与胶囊单位、两个或三个掩体三角形、不可通行格、初版拾取、
合法/不可通行/单位冲突/掩体冲突放置检查、三次批量修改及修改后拾取。每个样本
有 9 条拾取轨迹，最终版本为 `4`。

`samples/manifest.json` 和 `baselines/manifest.json` 记录文件哈希、轨迹链头、
记录数、最终版本和最终内容哈希。`verify` 从输入重新执行场景，只有轨迹与结果
文件的字节哈希都与基线一致才通过。

## 图形工作台

`python -m tactical_pick gui` 显示：

- 可通行/不可通行格子、AABB 单位、胶囊单位、掩体三角形。
- 鼠标拾取后的命中十字与命中实体/版本。
- 草拟区域的合法/非法着色和冲突文本。
- “提交批量修改”原子移动当前选中单位。
- “启动回放”在后台线程连续查询；“协作取消”设置取消事件并等待线程退出。

`run_gui(..., smoke_ms=N)` 提供离屏生命周期测试入口。图形界面只消费核心 API，
不参与确定性基线的生成。

## 业务码汇总

| code | 含义 |
|---|---|
| `E_SCENE_INVALID` | 场景字段、几何、索引、掩码或批量操作非法 |
| `E_RAY_INVALID` | 相机参数、屏幕坐标或射线方向非法 |
| `E_VERSION_STALE` | 携带的版本/哈希不是当前发布版本 |
| `E_STALE_HANDLE` | 快照句柄已释放或不再是活动句柄 |
| `E_REGION_INVALID` | 草拟区域自相交、越界或退化 |
| `E_TRACE_INVALID` | 轨迹截断、断链、缺字段或回放不一致 |
| `E_SNAPSHOT_INVALID` | 快照信封、校验和、版本或内嵌场景损坏 |
| `E_BATCH_CONFLICT` | 候选版本破坏单调版本约束 |
| `E_CANCELLED` | 脚本或界面协作取消 |
| `E_GUI_UNAVAILABLE` | 当前解释器未安装 PyQt5 |
| `W_RAY_GRAZING` | 近似相切命中的诊断告警 |

## 实际验证

本仓库包含已生成的 `samples/` 与 `baselines/`。直接执行：

```bash
python3 -m tactical_pick verify
python3 -m unittest discover -s tests -v
```

验证覆盖：二进制往返与损坏检测、退化包围体、非法面片索引、掩码长度、AABB/
胶囊/掩体/地面拾取、相机退化、屏幕越界、重复查询一致性、批量失败零污染、
版本过期、陈旧句柄、并发查询完整快照、四类放置结果、区域自相交/越界、轨迹
截断/断链/缺版本、篡改回放、五类注入无输出、相同种子逐字节复现和离屏 GUI
生命周期。

本次实际执行结果：

- `verify`：3 个样本全部通过，每样本 9 条轨迹回放一致，最终版本均为 4。
- 系统解释器：41 项测试中 40 通过，GUI 项因系统解释器无 PyQt5 跳过。
- `.venv` + `QT_QPA_PLATFORM=offscreen`：41 项全部通过，GUI 生命周期退出码 0。
- 真实 Wayland 显示平台：窗口创建与 250ms 生命周期冒烟退出码 0。
- 五类注入均以返回码 2 退出，并分别返回独立 `E_INJECT_*` 码与注入点。
- 三种样本的轨迹字节哈希分别为
  `bf0335c8a8e3d64c6c3fd803aa078ae9373c11c08ebf827017b73d5f27052c10`、
  `700f5839b74656e6f08bf44cabf9b75ca19ab56265257f4e7b5780e9ff543e79`、
  `1b127ecca845456c0ed62d35ac3d0aa71cbcd9b7dae1a171975d3545e28e9863`。
