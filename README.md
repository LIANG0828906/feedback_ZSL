# 本地光照探针烘焙器

一个不依赖外部资源的确定性光照探针实现，覆盖场景载入、规则探针网格、实球谐投影、遮挡可见性、邻近插值补偿、自适应稠密/稀疏策略、依赖失效、增量重烘、不可变版本发布、分块哈希链快照、强杀恢复、并发重烘边界和可选 OpenGL 预览。

核心算法与测试不需要 OpenGL 上下文；无图形设备时可使用脚本化入口。

## 环境准备

要求 Python 3.11、`numpy==2.2.6`、`PyOpenGL==3.1.7`；测试额外使用 `pytest==8.3.5`。

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m probe_baker generate --out samples
.\.venv\Scripts\python -m probe_baker bake --scene samples\street_room.json
```

Linux/macOS：

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m probe_baker generate --out samples
```

OpenGL 预览使用 PyOpenGL + GLUT。若系统缺少 GLUT 运行库，需要安装平台对应的 FreeGLUT；烘焙、恢复、查询和以下无头入口不依赖该运行库：

```bash
.venv/bin/python -m probe_baker viewer \
  --scene samples/street_room.json --headless --out artifacts/viewer
```

## 快速验证

```bash
.venv/bin/python -m pytest -q
.venv/bin/python scripts/run_acceptance.py --clean
```

验收输出位于 `artifacts/acceptance/`：

- `summary.json`：本次验证摘要
- `trace.jsonl`：总体验收轨迹
- `incremental/trace.jsonl`：增量失效、复用、版本查询轨迹
- `comparison/trace.jsonl`：稠密/稀疏策略对比轨迹
- `forced/trace.jsonl`：真实强杀与恢复轨迹
- `incremental/snapshots/v0000000*/`：v1 至 v4 完整快照
- `recovery/recovered_snapshots/v00000001/`：恢复后的完整快照

## 目录

```text
probe_baker/
  scene.py          严格 JSON 场景解析、影响因子哈希
  grid.py           规则三维格点、26 邻域、结构哈希
  sh.py             2-6 阶实球谐、投影、截断误差、辐照度重建
  sampling.py       Fibonacci 方向、天空光、AABB/球体解析遮挡
  interpolation.py  三线性查询、IDW 邻域插值、漏光补偿
  strategy.py       自适应策略、成本模型、误差上界估计
  bake.py           稠密/稀疏烘焙、增量失效、事务性状态
  publish.py        不可变发布快照、钉住版本查询
  snapshot.py       分块 NPZ、哈希链、原子提交、恢复
  jobs.py           每场景单写任务、协作取消、E_BUSY
  viewer.py         OpenGL 预览与无头入口
  cli.py            本地命令行入口
samples/            本地确定性样本
tests/              30 项自动化测试
scripts/run_acceptance.py
artifacts/acceptance/
```

## 场景 JSON

所有样本由 `probe_baker.samples` 在本地生成。输入字段缺失、包围盒非法、遮挡体非法、动态遮罩引用未知物体、间距非正或非有限时返回 `E_SCENE_INVALID`。

```json
{
  "scene_id": "street_corner_room",
  "bounds": {"min": [-5, 0, -4], "max": [5, 3, 5]},
  "spacing": 1.0,
  "occluders": [
    {"id": "wall", "type": "aabb", "min": [-5, 0, 4.4], "max": [5, 3, 5]},
    {"id": "lamp", "type": "sphere", "center": [2.8, 2.4, -2.3], "radius": 0.55}
  ],
  "material": {"albedo": [0.72, 0.69, 0.64]},
  "sky": {
    "zenith": [0.78, 0.88, 1.0],
    "horizon": [0.92, 0.86, 0.74],
    "ground": [0.12, 0.11, 0.09],
    "intensity": 1.15
  },
  "dynamic_mask": ["lamp"]
}
```

`dynamic_mask` 中的遮挡体保留在场景描述中，但不参与当前静态烘焙采样；切换遮罩会只失效与该遮挡体采样射线相关的探针，并扩展补偿依赖。

## 命令行

```bash
# 生成本地样本
.venv/bin/python -m probe_baker generate --out samples

# 烘焙、写完整快照、发布 v1
.venv/bin/python -m probe_baker bake \
  --scene samples/street_room.json --spacing 1 --order 3 --directions 96 \
  --compensation 0.35 --strategy auto --out artifacts/run \
  --snapshot-root artifacts/run/snapshots --log artifacts/run/trace.jsonl

# 在同一场景、同一方向集上比较稠密与稀疏策略
.venv/bin/python -m probe_baker compare \
  --scene samples/street_room.json --spacing 1.5 --order 3 --directions 64

# 脚本化增量重烘演示
.venv/bin/python -m probe_baker incremental \
  --scene samples/street_room.json --out artifacts/incremental

# 强杀模拟：写完指定块后直接 os._exit(23)，不写 manifest
.venv/bin/python -m probe_baker snapshot-kill \
  --scene samples/city_block.json --snapshot-root artifacts/forced/snapshots \
  --chunk-size 64 --after-chunks 2 --version 1

# 恢复：校验连续合法前缀，仅采样未提交探针
.venv/bin/python -m probe_baker recover \
  --scene samples/city_block.json --snapshot-root artifacts/forced/snapshots \
  --snapshot-dir artifacts/forced/snapshots/v00000001 \
  --chunk-size 64 --out artifacts/recovery

# 查询完整快照；版本或内容哈希不符返回 E_VERSION_STALE
.venv/bin/python -m probe_baker query \
  --snapshot-root artifacts/run/snapshots --version 1 \
  --expected-hash <content-hash> --position 0.5 1.0 0.5
```

## 实现语义

### 探针网格

- 从包围盒 `min` 开始按间距生成三维格点，索引顺序为 `(x, y, z)`。
- 邻接关系为规则格点的 26 邻域，边数、探针数和结构哈希由显式 `GridBuildResult` 输出。
- 间距或包围盒变化时重新生成格点，不走过旧网格复用路径；旧的探针数、边数和新值同时写入轨迹。
- 非正或非有限间距直接返回 `E_SCENE_INVALID`，不会沿用旧格点。

### 采样与球谐

- 默认方向集为确定性 Fibonacci 球面，方向归一化后计算方向集哈希。
- 支持 2 至 6 阶实球谐，索引为 `l*l+l+m`；投影使用 `4π/N` 离散正交权重。
- 每个探针输出 RGB 系数、可见度均值和相对 L2 截断误差。
- AABB 使用 slab 射线求交，球体使用解析二次方程；动态遮罩物体跳过。
- 非有限方向、方向数不足以覆盖所选基、阶数越界或采样值非法时返回 `E_SH_INVALID`。验证在返回结果前完成，调用方状态不会被部分写入。

### 插值与补偿

- 运行时任意位置查询使用 8 角三线性插值；查询超出格点范围或邻域残缺返回 `E_INTERP_FAILED` 并带最近探针编号。
- 烘焙内部使用 26 邻域反距离多点加权（IDW）生成补偿目标。
- 漏光补偿只作用于强遮挡探针：可见度低于 `0.5` 时产生非零混合权重。补偿强度编辑只重算混合权重非零的探针及其一环补偿依赖，其他探针复用系数与逐探针内容哈希。

### 自适应策略

- 稠密：每个探针都按完整方向集采样。
- 稀疏：按 `sparse_anchor_stride` 选择锚点，锚点仍使用完整方向集；非锚点由 8 锚点三线性恢复系数、可见度和截断误差。
- `auto` 使用“方向采样数 × (1 + 0.025 × SH 系数数)”与“锚点采样 + 全探针插值成本”比较，并输出所选策略、成本依据、预计采样数和误差上界。
- `compare` 以同一方向集、每探针 6 阶采样为参考；不会降低方向覆盖率或截断参考数据来制造加速。

### 增量失效

影响因子包括网格、遮挡体、材质反照率、天空光、SH 阶数、方向集、动态遮罩和补偿强度。

- 网格、材质、天空、SH 阶数、方向集或采样策略变化：保守扩大到全部探针。
- 遮挡体或遮罩变化：对旧、新遮挡体分别计算固定方向集下的射线命中集合，取并集；这是对当前确定性采样集精确、对未知方向保守的策略。
- 补偿强度变化：只取补偿权重非零的强遮挡探针，再扩展一环补偿依赖。
- 稀疏模式下若锚点被重采样，所有引用该锚点的非锚点探针重插值，并继续扩展补偿依赖。
- 每个探针保存影响因子版本和内容哈希；未进入失效集合的探针直接复用已提交结果。

### 版本发布与查询

- `PublishedStore` 内部只做一次不可变快照引用替换；发布前查询持续读取旧版本。
- 查询先钉住快照对象，再在锁外完成插值。一次 `query_probes` 的所有探针来自同一版本。
- 版本号或内容哈希不匹配返回 `E_VERSION_STALE`；旧快照对象在后续发布后仍可继续查询。

### 分块快照与恢复

每个版本目录包含：

```text
begin.json                    场景、配置、方向集、总探针数、块大小、内容哈希
chunk_00000000.npz            原始/最终 SH 系数、截断误差、可见度、采样标记、
                              遮挡命中、影响因子版本、逐探针哈希、链元数据
...
manifest.json                 仅在所有块完整校验后原子提交的完整标记
```

- 每个块先写同目录临时文件，`fsync` 后 `os.replace`；链哈希覆盖数组哈希、版本、块范围和前驱哈希。
- 恢复只接受从 0 开始的连续合法前缀。尾块缺失表示未提交，可按 pending 恢复；中间缺块、后续块越过缺口、字段缺失、数组哈希或链哈希不符均返回 `E_RECOVERY_INVALID`。
- 强制终止不会生成 `manifest.json`，恢复报告为 `partial`、`clean_exit=false`、`published=false`；不会将其描述为已生成退出报告。
- 恢复只重新采样 pending 探针；已提交探针数据不重采样。补偿依赖探针会按需重算最终系数。恢复完成后写入新的完整快照并重新发布。

### 并发任务与界面

- 每个场景一个 `BakeJobManager`，同时只允许一个重烘任务；重复启动返回 `E_BUSY`。
- 采样按块调用检查点；取消事件置位后抛出取消状态，不替换已发布版本，局部临时数组释放。
- OpenGL 预览绘制包围盒、AABB/球体遮挡体、探针点和辐照度热力。
- 快捷键：`+/-` 调间距，`[`/`]` 调 SH 阶数，`,`/`.` 调补偿强度，`o` 增删示例遮挡体，`d` 切换其动态遮罩，`b` 启动重烘，`x` 取消，`a` 旋转视角，`Esc` 退出。

## 错误码

| 错误码 | 场景 |
|---|---|
| `E_SCENE_INVALID` | 字段缺失、非法包围盒/遮挡体/遮罩、非正间距 |
| `E_SH_INVALID` | 阶数越界、方向非有限、方向集不足、非法投影输入 |
| `E_INTERP_FAILED` | 查询越界、邻域残缺、退化距离、补偿输入非法 |
| `E_VERSION_STALE` | 无发布版本、版本不匹配、内容哈希不匹配、非递增发布 |
| `E_RECOVERY_INVALID` | begin/manifest/块字段缺失、断链、数组或链哈希不符 |
| `E_BUSY` | 同一场景已有活动重烘任务 |
| `E_CANCELLED` | 在协作检查点取消 |

## 实际验证结果

在本地 Python 3.11.18、`numpy 2.2.6`、`PyOpenGL 3.1.7` 环境运行：

```text
30 passed
```

`scripts/run_acceptance.py --clean` 的实际结果：

| 项目 | 结果 |
|---|---|
| 发布历史 | v1, v2, v3, v4 |
| 局部遮挡体编辑 | 重算 373 / 440，依赖闭包 373 |
| 补偿强度编辑 | 仅重算 8 / 440 |
| 天空光全局编辑 | 保守重算 440 / 440 |
| 旧 v1 快照对象 | 发布 v4 后仍可查询 |
| 旧版本经当前入口查询 | `E_VERSION_STALE` |
| 策略对比探针数 | 147 |
| 共同方向数 | 64，方向哈希一致 |
| 稠密平均相对误差 | 0.001423 |
| 稀疏平均相对误差 | 0.068401 |
| 稠密采样探针 | 147 |
| 稀疏采样探针 | 32 |
| 方向覆盖率降低 | false |
| 无头预览 | v2，60 探针，热力范围 14.098910–39.884258 |
| 强杀进程退出码 | 23 |
| 强杀后 manifest | 不存在 |
| 强杀恢复报告 | partial，`clean_exit=false` |
| 已提交/未提交 | 128 / 340 |
| 实际重采样 | 340，全部为 pending |
| 恢复后版本/哈希 | v1，与中断前内容哈希一致 |

完整机器可读结果见 `artifacts/acceptance/summary.json`。

