# Motion Workbench

面向动捕长序列的离线约束求解工作台。载入本地 BVH 与 JSON 约束，按帧求解关节旋转、骨骼长度和末端目标；参数变化只重解显式闭包内的帧。姿态、残差、参数版本和内容哈希按区间原子提交，强杀后仅依赖磁盘状态续解。

## 已实现能力

- 确定性 BVH 解析与 FK：校验层级父引用、通道数量、帧数和数值，错误统一返回 `E_BVH_INVALID`，并携带 `joint`、`frame` 定位字段。
- 三类逐帧约束：`bone_length`、`joint_limit`、`effector_target`，各自带 `weight`、`tolerance`、残差和证据。
- 显式依赖闭包：按关节的祖先/子树链确定受影响关节，再与约束帧范围及同帧末端目标求交；轨迹记录原因，无法证明时保守重解并标记。
- 不可变区间提交：临时文件 + `fsync` + 原子替换；区间文件只追加新版本，旧文件不被覆盖。写失败返回 `E_COMMIT_FAILED`。
- 磁盘恢复：参数快照自校验、区间内容哈希、前驱哈希链和区间表版本共同验证；异常返回 `E_RECOVERY_INVALID` 和 `first_break_frame`。
- 冲突诊断：达到迭代上限时输出具体帧、约束 ID、关节、最差残差和证据，并写 barrier 阻止冲突帧及后续帧进入有效区间。
- 单任务边界：进程内锁 + OS 文件锁，同一 BVH 重复启动返回 `E_BUSY`；强杀后 OS 自动释放锁。
- 桌面界面：可旋转三维投影视口、逐帧残差热力、播放时间线、约束权重/容差编辑，以及启动、暂停、取消、磁盘恢复。
- 确定性样本生成器：不同帧数、关节数和冲突组合，输出 BVH、约束 JSON、基线残差哈希与可校验会话。

## Windows 快速开始

目标环境为 Windows 10+、64 位 Python 3.11。先准备离线 wheel 目录，然后：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\activate
python -m pip install --no-index --find-links .\wheels -r requirements.txt
python -m motion_workbench
```

依赖固定为：

```text
numpy==1.26.4
PySide6==6.7.2
```

核心解析、求解、提交与恢复仅使用 Python 标准库；NumPy 和 PySide6 用于目标环境依赖完整性及桌面 UI。无 PySide6 时仍可使用全部命令行功能。

## 命令行

生成单个样本及基线：

```powershell
python -m motion_workbench generate --out samples --frames 48 --joints 12 --conflict none --seed 7
```

生成多组合样本集：

```powershell
python -m motion_workbench generate --out samples --suite
```

全量/增量求解（同一会话目录即增量）：

```powershell
python -m motion_workbench solve `
  --bvh samples\frames048_joints12_none_seed7.bvh `
  --constraints samples\frames048_joints12_none_seed7.constraints.json `
  --session samples\work-session --chunk-size 8
```

修改约束并触发区间规划：

```powershell
python -m motion_workbench edit --session samples\work-session `
  --constraint "ee:effector" --weight 1.5 --tolerance 0.006
```

仅从磁盘快照恢复：

```powershell
python -m motion_workbench resume --session samples\work-session
python -m motion_workbench verify --session samples\work-session
```

`verify` 还可传入 `--expected-table-version` 或 `--expected-parameters-version`；与磁盘快照不一致时返回 `E_RECOVERY_INVALID`。

强杀恢复验证使用故障注入参数（进程以 137 硬退出）：

```powershell
python -m motion_workbench solve --bvh ... --constraints ... `
  --session samples\crash-session --chunk-size 6 --crash-after-commits 2
python -m motion_workbench resume --session samples\crash-session
python -m motion_workbench verify --session samples\crash-session
```

## 约束 JSON

```json
{
  "schema": "motion-workbench/constraints/v1",
  "name": "shot-001",
  "solver": {
    "max_iterations": 36,
    "rotation_step_deg": 5.0,
    "scale_step": 0.02,
    "min_rotation_step_deg": 0.01,
    "min_scale_step": 0.00001
  },
  "constraints": [
    {
      "id": "bone:forearm.R",
      "type": "bone_length",
      "joint": "forearm.R",
      "target_length": 0.28,
      "weight": 1.0,
      "tolerance": 0.0005,
      "frames": [[0, 120]]
    },
    {
      "id": "limit:upperarm.R",
      "type": "joint_limit",
      "joint": "upperarm.R",
      "min_deg": [-80, -50, -70],
      "max_deg": [90, 80, 70],
      "weight": 1.0,
      "tolerance": 0.1,
      "frames": [[0, 120]]
    },
    {
      "id": "ee:hand.R",
      "type": "effector_target",
      "joint": "hand.R",
      "weight": 1.2,
      "tolerance": 0.008,
      "targets": [
        [0, [0.4, 1.2, 0.1]],
        [1, [0.41, 1.21, 0.1]]
      ]
    }
  ]
}
```

`joint_limit` 的三个角度按该关节 BVH rotation channel 的声明顺序解释。末端约束的 `targets` 帧集合必须与其 `frames` 一致；省略 `frames` 时自动使用目标帧集合。

## 验证

```powershell
python -m unittest discover -v
python scripts/run_acceptance.py
```

完整验收输出见 `artifacts/verification.json`，固定样本和可检查会话位于 `artifacts/acceptance/`。本次实际结果：

- 24 项自动化测试全部通过。
- 24 帧/8 关节样本只修改第 10 帧末端目标，影响区间为 `[10,10]`，23 帧缓存命中；增量结果与全新全量求解 24/24 帧哈希一致。
- 强杀进程退出码 `137`，磁盘已有区间 `[0,11]`；恢复仅计算 `12..23`，缓存 `0..11`，与未中断全量结果 24/24 帧哈希一致。
- 不可达末端目标产生 `conflict` 报告，barrier 位于具体冲突帧，报告字段可独立定位约束与证据。
- 注入 manifest 写失败返回 `E_COMMIT_FAILED`，有效提交数仍为 0，链头保持 `GENESIS`。

更完整的设计、依赖闭包、提交格式和恢复校验见 [docs/TECHNICAL.md](docs/TECHNICAL.md)。

## 工程结构

```text
motion_workbench/
  bvh.py           BVH 解析、校验与确定性 FK
  constraints.py   约束模型、JSON 校验、逐帧依赖哈希
  dependency.py    关节链与帧范围影响闭包
  solver.py        确定性局部迭代求解、残差与冲突证据
  store.py         不可变区间、参数快照、原子提交、哈希链恢复
  session.py       求解任务编排、缓存复用、取消/暂停
  generator.py     确定性 BVH/约束/基线生成器
  gui.py           PySide6 桌面工作台
  cli.py           无界面命令
tests/             24 项回归与验收测试
scripts/run_acceptance.py
```

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_BVH_INVALID` | BVH 层级、通道、帧数或数值错误 |
| `E_CONSTRAINT_INVALID` | 约束字段、帧范围或关节引用错误 |
| `E_COMMIT_FAILED` | 原子写失败，上一有效状态保持不变 |
| `E_RECOVERY_INVALID` | 参数快照、区间表版本、内容哈希或哈希链断链 |
| `E_BUSY` | 同一 BVH 已有求解任务 |
| `E_CANCELLED` | 协作式取消完成 |
| `E_CONFLICT` | 迭代上限内未满足全部约束，附结构化报告 |
