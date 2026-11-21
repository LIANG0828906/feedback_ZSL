# Local Light Probe Baker

面向开放世界双级本地场景的确定性探针烘焙组件。同一套 API 处理街角小房间与
城区场景，支持可配置探针间距、2 至 6 阶球谐、遮挡补偿、增量重烘、不可变版本
查询、分块原子快照和强制终止恢复。运行时只读取本地 JSON 与磁盘快照，不访问网络。

## 快速开始

环境已验证：Python 3.11.18、NumPy 2.4.6、pytest 9.2.0。

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements.txt

# 重新生成固定种子的两级场景和 256 方向高分辨率真值基线
.venv/bin/python -m lightprobe generate-samples --out samples

# 运行全部测试
.venv/bin/python -m pytest

# 运行两级场景验收，输出可观测策略、基线误差、增量复用和快照恢复结果
.venv/bin/python -m lightprobe verify \
  --report artifacts/reports/verification.json

# 启动内置三维 Web 界面
.venv/bin/python -m lightprobe serve \
  --scene samples/city_district.json --port 8765
```

浏览器打开 `http://127.0.0.1:8765/`。界面完全离线，无 CDN、贴图或外部脚本。

## 目录

- `lightprobe/scene.py`：场景 JSON 校验、材质/天空/遮挡体模型与稳定指纹。
- `lightprobe/grid.py`：三维均匀探针格点、坐标哈希、邻接关系、重建差异。
- `lightprobe/geometry.py`：AABB/球体批量首命中射线求交。
- `lightprobe/directions.py`、`lightprobe/sh.py`：确定性 Fibonacci 方向集、实 SH 投影、余弦卷积。
- `lightprobe/strategy.py`：稠密/稀疏策略审计与同口径误差比较。
- `lightprobe/influence.py`、`lightprobe/planner.py`：影响因子、版本/内容哈希复用证明和依赖闭包。
- `lightprobe/bake.py`：烘焙主流程、补偿、增量重烘、快照提交。
- `lightprobe/version.py`：不可变发布版本与同版本三线性查询。
- `lightprobe/snapshot.py`：fsync + 原子重命名 + 分块哈希链。
- `lightprobe/coordinator.py`、`lightprobe/ui.py`：单写者并发边界、协作式取消、三维界面。
- `lightprobe/samples.py`：固定种子样本和基线生成器。
- `tests/`：33 项自动化验证。

## 场景格式

```json
{
  "schema_version": 1,
  "scene_id": "city_district",
  "bounds": {"min": [0, 0, 0], "max": [32, 10, 32]},
  "settings": {
    "probe_spacing": 2.0,
    "sh_order": 3,
    "direction_count": 64,
    "direction_seed": 11,
    "compensation_strength": 0.42,
    "strategy": "auto"
  },
  "sky": {
    "zenith_radiance": [0.64, 0.78, 1.15],
    "horizon_radiance": [0.92, 0.83, 0.66],
    "ground_radiance": [0.12, 0.11, 0.10],
    "intensity": 1.25
  },
  "materials": {
    "concrete": {"albedo": [0.56, 0.55, 0.51], "bounce_gain": 0.16}
  },
  "occluders": [
    {
      "id": "block_a",
      "type": "aabb",
      "min": [2, 0, 2],
      "max": [10, 8, 8],
      "material": "concrete"
    },
    {
      "id": "tree_a",
      "type": "sphere",
      "center": [12, 4.8, 12.5],
      "radius": 2.0,
      "material": "foliage"
    }
  ]
}
```

缺失字段、非有限数值、未知材质、重复 ID、退化 AABB（任一轴 `max <= min`）或
非正球半径均返回 `E_SCENE_INVALID`，`details.path` 与 `details.object_id`
定位到具体字段和对象。

## 数值管线

### 网格与坐标哈希

每个轴按 `floor(extent / spacing) + 1` 生成格点；不足两个格点的轴不能构成完整
插值邻域，返回 `E_INTERP_FAILED`。每枚探针 ID 为稳定索引形式
`pXXXX_YYYY_ZZZZ`，坐标哈希由 `scene_id + 量化坐标` 的 SHA-256 构成。因此间距
变化时重建报告能区分真正新增/移除的坐标，而不是把所有索引变化都视作移除。

### 方向采样与 SH

方向集使用确定性 Fibonacci 球面，种子只控制固定偏航；默认 64 方向。
每枚直接采样探针沿全部方向取：

- 未被遮挡：天空 RGB 辐亮度；
- 首命中遮挡体：天空平均辐亮度 × 材质反照率 × `bounce_gain`。

实 SH 基使用以 `+Y` 为极轴的归一化伴随 Legendre 多项式，系数索引为
`l*l + l + m`。带限辐亮度系数通过均匀球面积分投影：

```text
L_lm = (4*pi/N) * sum_i L(d_i) * Y_lm(d_i)
```

再乘 `max(cos(theta), 0)` 的逐阶 SH 卷积因子得到辐照度系数。每枚探针同时保存
投影残差相对 RMS，作为该探针的带限/方向集截断误差估计。阶数越界、空方向集、
样本数不匹配或投影得到全零系数均返回 `E_SH_INVALID`，不会产出伪零结果。

### 遮挡补偿与稀疏插值

稀疏策略只对棋盘隔点源探针做完整 `D` 方向 SH 采样。其余探针从最多 8 个最近
源探针做反距离加权系数插值；所有探针仍保留独立记录，不会截断探针集合。

每枚探针另有确定性审计方向集，计算可见比例、审计平均辐亮度和首命中摘要。
直接采样探针不做漏光补偿；插值探针若出现“低可见比例但插值亮度高于审计亮度”
则得到 `leak_score`。补偿把其辐照度系数按配置强度向邻域更暗的原始系数混合。
补偿后沿 8 个法线检查负辐照度；出现负值时返回 `E_INTERP_FAILED` 并列出探针 ID。

### 稠密/稀疏自适应

`strategy=auto` 使用以下预算选择：

- 稠密射线样本 `probe_count * (direction_count + audit_count)`；
- 稀疏射线样本 `source_count * direction_count + probe_count * audit_count`；
- 原始 + 补偿后系数内存 `probe_count * (order+1)^2 * 3 * 8 * 2`。

超过 `max_dense_ray_samples` 或 `max_dense_coefficient_bytes` 时选稀疏。
也可以用 `strategy=dense|sparse` 强制，便于同场景对照。

每次策略审计从场景中确定性抽取最多 24 个验证探针，包含源探针与插值探针；
用独立的 192 方向 Fibonacci 集和 8 个法线生成同一真值，再分别比较稠密估计与
稀疏估计。输出包含抽样方式、真值来源、两策略 RMS/最大误差和保守上界
`selected_max + 2 * selected_rms`。该上界是基于验证抽样的经验包络，不是对所有
连续查询位置的数学保证；真实基线比较另见下文。

## 增量重烘

影响因子：

- 间距/包围盒：重建格点，全部探针失效；
- SH 阶数、主方向集、审计方向集：全部探针失效；
- 天空光参数：全部探针失效；
- 遮挡体几何：逐探针比较旧首命中摘要与新几何近距离，仅把新增遮挡、失去遮挡或
  首命中改变的探针设为种子；
- 材质反照率：仅首命中该材质对应对象的探针失效；
- 补偿强度：仅 `leak_score > 0` 的探针失效；
- 策略或插值邻域变化：全部失效。

种子集合随后沿 `source_ids`（稀疏插值来源）和有效补偿邻域做反向依赖闭包。
旧状态缺失、对象 ID 集合/顺序变化、无法证明安全时保守重烘全部探针，并在
`influence.conservative_reason` 标注原因。

复用必须同时满足：

1. 记录 `content_hash` 可由当前 payload 重新计算；
2. 记录 `influence_version` 与当前格点、方向、天空、命中对象、源探针 payload、
   补偿邻域和强度重新推导一致；
3. 依赖闭包中没有脏探针。

任一证明失败都进入重烘；不会仅凭探针 ID 或全局版本号复用陈旧数据。

## 版本化查询

`VersionedStore` 的发布操作在同一锁内整体替换不可变 `PublishedVersion` 引用。
查询先取得版本快照，再在锁外完成三线性插值。返回结果包含：

- 单一 `version` 与版本内容哈希；
- 参与插值的 8 枚探针 ID、权重、各自版本与内容哈希；
- 指定法线上的 RGB 辐照度。

若调用方传入的版本号或内容哈希已过期，返回 `E_VERSION_STALE`。发布前旧版本
持续可读；查询不会混用两个版本的探针。邻域缺探针或查询点超出完整均匀格点范围
返回 `E_INTERP_FAILED`。

## 原子快照与恢复

快照目录包含 `meta.json` 和 `chunk_XXXXXX.json`：

- 每块写入同目录临时文件，`flush + fsync + os.replace + 目录 fsync`；
- 块内保存原始/补偿后 SH、审计可见性摘要、首命中 ID/距离、源探针、影响因子版本、
  payload/content 哈希；
- 每块 `prev_hash` 指向上一块，块哈希覆盖完整块 payload；
- 元数据固定场景、格点、配置、策略和全部影响因子版本。

恢复只读取磁盘元数据与连续提交块，校验格式、字段、块序号、哈希链、探针
版本/内容哈希和格点坐标哈希。断链、缺字段、未知探针或因子版本不匹配均返回
`E_RECOVERY_INVALID`。已提交探针直接复用，未提交探针重烘；恢复完成后内容哈希
必须与不中断烘焙一致。进程在快照提交阶段被 `SIGKILL` 时没有退出报告，恢复也不
读取原进程内存残留。

## 并发与界面

`SceneRuntime` 每场景只有一个写任务。任务运行期间再次调用 `start_bake` 返回
`E_BUSY`。取消事件在采样批次、快照块边界和发布前检查；块提交函数使用上下文
关闭文件句柄，取消后最后一个已发布版本继续可查询。

内置 Web 界面支持：

- 鼠标拖动旋转、滚轮缩放的三维场景/探针/热力图；
- 修改间距、SH 阶数、补偿强度和遮挡体位移；
- 启动/取消重烘，轮询阶段与检查点进度；
- 展示版本、内容哈希、策略、复用数、最大系数偏差和截断误差；
- 以当前版本号和内容哈希发起中心点查询，展示 8 枚同版本插值探针。

## 主要错误码

| 错误码 | 含义 |
| --- | --- |
| `E_SCENE_INVALID` | 场景字段缺失、数值非法、退化遮挡体、未知材质等 |
| `E_SH_INVALID` | 阶数越界、空方向集、样本不匹配、全零投影 |
| `E_INTERP_FAILED` | 插值邻域不完整或补偿后出现负辐照度，附探针 ID |
| `E_VERSION_STALE` | 请求版本/哈希与当前发布版本不一致 |
| `E_RECOVERY_INVALID` | 快照元数据、块字段、哈希链或版本证明损坏 |
| `E_BUSY` | 同一场景已有重烘任务 |
| `E_CANCELLED` | 任务在协作式检查点取消 |

## 实测结果

固定样本生成种子 `19093`。基线使用 256 个独立方向、6 个轴对齐法线、直接
Lambertian 辐照度积分，覆盖所有探针。2026-11-22 本机实测：

| 场景 | 探针 | 自动策略 | 基线绝对 RMS | 基线相对 RMS | 增量变化 | 重烘/复用 | 恢复一致 |
| --- | ---: | --- | ---: | ---: | --- | ---: | --- |
| 街角小房间 | 75 | dense | 0.0550 | 2.83% | 移动球形灯 | 7 / 68 | 是 |
| 城区 | 1734 | sparse | 0.3116 | 19.09% | 移动小树 | 911 / 823 | 是 |

策略审计同时输出两策略同口径误差：

| 场景 | 稠密审计 RMS | 稀疏审计 RMS | 选定误差包络 |
| --- | ---: | ---: | ---: |
| 街角小房间 | 0.0478 | 0.3665 | 0.2180 |
| 城区 | 0.0781 | 0.3296 | 1.8130 |

小房间网格太小，稀疏棋盘插值误差明显，因而自动选择稠密；城区按射线预算选择
稀疏但仍为全部 1734 枚探针生成记录，其中 243 枚完整采样、1491 枚插值并经过
漏光补偿。完整机器可读结果写入
`artifacts/reports/verification.json`，稳定摘要见 `VALIDATION.md`。

## 测试覆盖

```text
33 passed
```

覆盖场景定位错误、退化遮挡体、格点重建与坐标哈希、SH 正交性/非法输入/零系数、
方向与阶数变化偏差、遮挡体移动、材质反照率、补偿强度、天空光保守重烘、内容
哈希污染拒绝、不可变版本与并发查询、快照往返、断链/缺字段、子进程 `SIGKILL`
恢复、块检查点取消、`E_BUSY`、HTTP 查询与版本过期。
