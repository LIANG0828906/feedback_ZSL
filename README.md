# MotionReview：确定性 BVH 姿态约束复核工作台

本项目实现一个可本地运行的桌面工作台，用于在同一台机器上并行复核多段
BVH 动作。核心保证是：**同一 BVH、同一约束参数、同一配置下，逐帧姿态、
逐约束残差和内容哈希与并行度、段调度顺序、预算降级阶段无关。**

不依赖 GPU、真实动捕设备或网络。Python 3.11+，数值依赖 NumPy。

## 快速开始

Windows PowerShell（Python 3.11 已在 PATH 中）：

```powershell
python -m pip install -r requirements.txt
python -m motionreview generate --out samples
python -m motionreview verify --sample samples --config samples/run_config.json
python -m motionreview gui --sample samples --case-name short --config samples/run_config.json --out gui-runs
```

也可以安装为命令行程序：

```powershell
python -m pip install .
motionreview --help
```

Linux/macOS 将 `python` 换成 `python3` 即可。当前交付环境没有 Tk 组件，
因此 GUI 只做了导入/语法检查；核心求解、调度与 CLI 已实际运行验证。

## 交付内容

```
motionreview/
  bvh.py          严格 BVH 解析、层级校验、确定性 FK
  constraints.py  三类约束、固定顺序残差、受影响区间差分
  solver.py       逐帧局部迭代、姿态/内容哈希、冲突诊断、缓存
  budget.py       内存计量、声明式降级计划
  scheduler.py    跨段并行/段内串行、等待队列、取消、轨迹
  storage.py      区间临时文件+原子重命名、哈希链恢复
  generator.py    确定性 BVH/约束/基线样本
  verify.py       基线、串行、并行三方逐帧复算校验
  gui.py          Tk Canvas 三维视口与侧栏工作流
  cli.py          generate / validate / solve / verify / gui
tests/            22 项标准库 unittest
artifacts/        实际验证输出
```

## 1. BVH 解析与 FK

`parse_bvh(text, clip_id=...)` 严格解析 `HIERARCHY`、`ROOT/JOINT`、
`OFFSET`、`CHANNELS`、`End Site`、`MOTION`、`Frames:` 和
`Frame Time:`。`MOTION`/`MOTION:` 两种常见写法均支持。

校验失败统一抛出 `BVHInvalidError`，错误码 `E_BVH_INVALID`，并携带
`joint`、`frame`、`details`：

- 父关节越界、根关节错误、重复/非法关节名；
- `CHANNELS` 声明数与通道列表不一致；
- 帧行通道数少于/多于声明总数；
- 缺少帧行、额外帧行、非数值或非有限值。

运动通道出错时会按通道边界定位到具体关节，例如第 2 帧少一个 `Spine`
通道时报告 `frame=2, joint="Spine"`。

`evaluate_fk(document, frames=None)` 返回
`(frames, joints, 4, 4)` 的 float64 世界变换。约定采用列向量：

```
world_child = world_parent @ T(offset + position) @ R(channel order)
R = R_first @ R_second @ ...
```

因此列出的旋转通道顺序即局部旋转矩阵的相乘顺序。单个整数帧返回
`(joints, 4, 4)`。BVHDocument 提供 `evaluate_fk()` 和
`joint_world_positions()` 便捷方法。

## 2. 约束与逐帧局部求解

约束 JSON 支持 `bone_length`、`angle_limit`、`effector_position`：

```json
{
  "constraints": [
    {
      "id": "bone_1",
      "kind": "bone_length",
      "joint": "Spine01",
      "weight": 1.0,
      "tolerance": 1e-6
    },
    {
      "id": "limit_1_Z",
      "kind": "angle_limit",
      "joint": "Spine01",
      "axis": "Z",
      "min": -0.7,
      "max": 0.7,
      "weight": 1.0,
      "tolerance": 1e-6,
      "frames": [0, 96]
    },
    {
      "id": "effector_hand",
      "kind": "effector_position",
      "joint": "Hand",
      "target": [1.2, 8.0, 0.3],
      "weight": 2.0,
      "tolerance": 0.02,
      "frames": [0, 40]
    },
    {
      "id": "effector_snapshot",
      "kind": "effector_position",
      "joint": "Hand",
      "frames": [120, 121],
      "targets": {"120": [1.4, 8.1, 0.3]},
      "weight": 1.5,
      "tolerance": 0.03
    }
  ]
}
```

- 骨骼长度：测量子关节原点到父关节原点的世界距离，并与声明/静止长度比较。
- 角度限位：violation 为 `max(min-angle, angle-max, 0)`，求解前先投影到限位内。
- 末端目标：欧氏距离，支持固定 target 或逐帧 targets。
- `frames` 为左闭右开 `[start, end)`。

求解器按约束编号顺序构造加权 Gauss-Newton 系统，只改变声明了的旋转
通道。每帧输出：

- `residuals`：约束 id、类别、关节、原始残差、加权残差、权重、容差、
  是否满足和详情；
- `total_residual`：按约束编号顺序逐项累加的加权残差；
- `pose_hash`：世界变换 float64 的规范哈希；
- `content_hash`：帧号、参数版本、姿态哈希和逐约束残差的规范哈希；
- `iterations`、`status=CONVERGED|CONFLICT`、可选 conflict 报告。

权重/容差变化通过 `ConstraintSet.changed_frames()` 计算精确受影响帧集合；
`PoseSolver.resolve_affected(new_constraints)` 只求解该区间。区间内未变化
的帧仍复用缓存。缓存哈希不重算，更不会全量重跑。

### 冲突诊断

达到迭代上限仍不满足时，诊断 JSON 包含：

```json
{
  "schema": "constraint-conflict-v1",
  "code": "E_CONSTRAINT_CONFLICT",
  "clip_id": "...",
  "frame": 21,
  "parameter_version": "...",
  "pose_hash": "...",
  "content_hash": "...",
  "iteration_count": 40,
  "constraint_ids": ["effector_impossible"],
  "joints": ["Arm08"],
  "worst_residual": {
    "constraint_id": "effector_impossible",
    "joint": "Arm08",
    "residual": 999.1,
    "tolerance": 0.0001
  },
  "residuals": ["...逐约束完整记录..."]
}
```

报告仅凭 `frame`、`constraint_ids`、`joints` 和 `worst_residual` 即可定位。
诊断由 `SnapshotStore.write_diagnostic()` 原子写出，但冲突帧本身不会提交。

## 3. 调度、并行度与预算

配置示例：

```json
{
  "schema_version": 1,
  "max_parallelism": 2,
  "per_task_memory_bytes": 33554432,
  "total_memory_bytes": 67108864,
  "max_iterations": 40,
  "default_cache_depth": 64,
  "checkpoint_frames": 32,
  "degradation_sequence": [
    {"name": "shrink_iteration_buffer", "iteration_frames": 32},
    {"name": "batch_frame_ranges", "batch_frames": 16},
    {"name": "reduce_cache_depth", "cache_depth": 1}
  ]
}
```

调度分两阶段：

1. **计划阶段**：在任何线程启动前按关节数、通道数、帧数、迭代缓冲、
   batch 工作区和缓存深度计算预计字节数；完整计划超限时按声明序列逐级
   重试并保留每次尝试记录。最终仍超限则 `E_BUDGET_EXCEEDED`，不启动、
   不写任何新区间。
2. **执行阶段**：`ThreadPoolExecutor(max_workers=max_parallelism)` 并行
   不同段；每段只有一个串行帧流。申请全局内存预留时若
   `reserved + task > total_memory_bytes`，进入等待队列，直到释放或取消。

内存估算公式位于 `budget.estimate_memory()`，分为
`fixed/motion/cache/iteration_workspace/batch_workspace`，调度轨迹记录
总量和分项。

同一段活动期间再次启动会立即得到 `E_BUSY`。`max_parallelism` 和两个配额
在运行中不可变；CLI 的 `--parallelism` 是启动前的显式覆盖，不是静默提高。

### 取消与暂停

- `pause(clip_id)`：在下一个区间检查点阻塞。
- `cancel(clip_id)`：在下一个检查点响应，当前正在求解的区间完成后提交，
  保留最后一次已提交区间。
- 检查点触发时不会启动新帧，因此不会产生“半提交”区间。

## 4. 原子区间提交与恢复

快照目录为 `<output>/<clip_id>/`，每个区间一个文件：

```
interval_00000001_00000000_00000032.json
```

提交过程：

1. 校验帧连续升序、参数版本一致、起点等于已提交 frontier；
2. 写入同目录临时文件，`flush + fsync`；
3. 校验可注入失败点；失败则删除临时文件并抛出 `E_COMMIT_FAILED`，
   旧区间不变；
4. `os.replace(temp, final)` 原子重命名，再做目录 fsync。

区间文件包含姿态 float64 hex、逐约束残差、帧内容哈希、参数版本、
快照版本、依赖信息、前一链哈希、`content_hash` 和 `chain_hash`。

恢复时仅扫描磁盘区间文件，不依赖进程内状态：

- 验证 schema/快照版本、clip id、连续 seq、连续帧区间；
- 验证前一链哈希、区间内容哈希、链哈希；
- 重构每帧姿态，复算 `pose_hash` 和 `content_hash`；
- 任一不符抛出 `E_RECOVERY_INVALID`，在 `details.breakpoint_file`
  与 `frame` 中定位**首个断点**。

`Scheduler(resume=True)` 将已提交帧 seed 回缓存，从
`next_frame` 继续；`TaskReport.frames_reused` 记录复用数量，
`frames_solved` 记录新求解数量。已提交帧不会重复求解。

## 5. 确定性设计

确定性来自以下约束，而不是依赖线程完成顺序：

- 每段只有一个写帧流的 worker；同段没有并发提交。
- 所有 FK 与 Jacobian 都按关节索引、通道声明顺序计算。
- 约束按 `constraint_index` 顺序编号；总残差用 Python 显式循环逐项
  累加，不使用 NumPy 并行归约。
- 哈希先把 float64 转成 `float.hex()`，再做排序键的规范 JSON 和
  SHA-256；不使用随机时间戳或完成次序。
- 预算降级只改变缓存深度、帧区间 batch 和内存计划，**不改变迭代上限
  或求解算法**。
- 并行配置、段提交先后、降级阶段不进入逐帧 `pose_hash/content_hash`
  的计算输入。

测试 `test_parallel_results_match_serial_results` 与
`test_degraded_execution_matches_full_frame_hashes` 直接比较逐帧内容哈希。

## 6. 样本生成器

默认样本：

| 名称 | 帧数 | 关节数 | 用途 |
|---|---:|---:|---|
| short | 96 | 7 | 快速复核 |
| medium | 240 | 12 | 中长序列 |
| conflict | 64 | 8 | 第 21 帧不可达末端目标 |
| long | 1024 | 16 | 长序列与降级 |

生成器同时输出：

- `<case>/<case>.bvh`；
- `<case>/constraints.json`；
- `<case>/baseline.jsonl`：逐帧状态、迭代数、逐约束残差、姿态哈希、
  内容哈希；
- `manifest.json`：样本、BVH、约束、基线聚合哈希、冲突帧；
- `run_config.json` 和 `run_config_tight.json`。

更大规模可显式生成（建议先不产基线，避免长时间求解）：

```powershell
python -m motionreview generate --out stress `
  --case huge:12000:20 --no-baseline
```

## 7. 验证命令与错误返回

```powershell
python -m motionreview validate --bvh samples/short/short.bvh
python -m motionreview solve --sample samples --case-name short `
  --config samples/run_config.json --out runs/short
python -m motionreview solve --sample samples --case-name short `
  --config samples/run_config.json --out runs/short --resume
python -m motionreview verify --sample samples `
  --config samples/run_config.json --output verify-runs
```

`verify` 会分别用配置并行度和 `max_parallelism=1` 运行全部段，比较：

- 每个已提交帧的姿态哈希与内容哈希；
- 每个约束的原始残差；
- 串行/并行的逐帧内容哈希；
- 冲突帧诊断与基线残差、姿态/内容哈希；
- 冲突后没有提交帧，其他段仍完整提交。

退出码：`0` 全部完成/校验通过；`2` 求解发生冲突或被拒绝；`1` 校验失败；
`3/4` 用法或结构化错误。

主要错误码：

| 错误码 | 场景 |
|---|---|
| `E_BVH_INVALID` | 层级、通道数、帧缺失或非有限值 |
| `E_BUSY` | 同段活动期间重复启动 |
| `E_BUDGET_EXCEEDED` | 所有降级阶段后仍超单任务/全局配额 |
| `E_COMMIT_FAILED` | 临时文件/重命名失败，旧区间保留 |
| `E_RECOVERY_INVALID` | 快照版本、参数或哈希链首个断点 |
| `E_CONSTRAINT_CONFLICT` | 迭代上限仍未满足约束（诊断 JSON） |

## 8. GUI

`python -m motionreview gui ...` 提供：

- Tk Canvas 三维等距视口，鼠标拖拽旋转，骨骼按帧显示；
- 骨骼和关节按残差/容差比例热力着色；
- 末端效应器目标十字和约束标签；
- 侧栏 Treeview 选择约束，编辑权重/容差；
- “Apply / Re-solve affected”只重解相关区间并复用其他帧；
- 播放/暂停、逐帧、区间检查点协作取消；
- Start、Pause、Cancel、Resume snapshot；
- 有已提交区间时从快照恢复，显示冲突约束。
