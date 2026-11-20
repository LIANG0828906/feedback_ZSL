# Mocap Constraint Workbench

本地、确定性的 BVH 多约束求解工作台。项目从空工作区构建，核心算法与 PyQt/OpenGL
桌面界面共用同一套 BVH、约束、缓存、快照和轨迹日志实现。

主要能力：

- 严格 BVH 解析、层级校验、逐帧正向运动学与世界空间姿态内容哈希。
- 骨骼长度、关节角度限位、末端效应器位置三类约束，可独立设置权重和容差。
- 解析式局部 Jacobian + 阻尼 Gauss-Newton 求解，关节限位投影，逐帧残差与冲突证据。
- 按帧数/约束数/关节数自动选择整段并行批处理或重叠窗口分段求解。
- 帧级求解缓存。容差变化只重算状态，权重/目标变化只使实际受影响帧的缓存失效。
- 原子帧区间快照、姿态哈希链、约束参数版本、求解器配置版本和断链校验恢复。
- 跨进程 BVH 单任务锁、区间边界协作式取消/暂停、内存预算中止、强制终止恢复。
- PyQt5 三维视口、骨架播放、逐帧残差热力、约束编辑、后台线程和快照恢复按钮。
- 确定性样本生成器与脚本化两档规模实验入口。

## 环境准备

Python 3.10+，依赖 NumPy；桌面界面需要 PyQt5 与 PyOpenGL。

Windows PowerShell：

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
pip install -r requirements.txt
python -m mocap_workbench.cli generate --out samples
python -m mocap_workbench.cli gui `
  --bvh samples\small.bvh `
  --constraints samples\small.constraints.json `
  --snapshot-dir snapshots\small `
  --software-gl
```

当前工作区也提供了已安装依赖的 `.venv`（Python 3.12）。Linux/WSL 下使用：

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements.txt
.venv/bin/python -m mocap_workbench.cli generate --out samples
.venv/bin/python -m mocap_workbench.cli experiment \
  --sample-dir samples --out artifacts/experiment --workers 1
```

`--software-gl` 会在创建 `QApplication` 前启用 Qt 软件 OpenGL，适用于无硬件加速的
Windows 桌面。程序不需要真实动捕设备、网络服务或 Docker。

## 快速使用

生成全部确定性样本：

```bash
python -m mocap_workbench.cli generate --out samples
```

生成自定义样本：

```bash
python -m mocap_workbench.cli generate \
  --out samples/custom \
  --name custom --seed 42 --frames 5000 --joints 24 \
  --missing-rate 0.2 --conflict unreachable
```

求解并输出轨迹、摘要和冲突报告：

```bash
python -m mocap_workbench.cli solve \
  --bvh samples/large.bvh \
  --constraints samples/large.constraints.json \
  --snapshot-dir work/snapshots-large \
  --chunk-size 240 --workers 1 \
  --trajectory work/trajectory.jsonl \
  --summary work/summary.json \
  --conflicts-out work/conflicts.json
```

强制终止恢复验收：

```bash
# 写入一个原子区间后直接 os._exit(23)，模拟强杀
python -m mocap_workbench.cli solve \
  --bvh samples/small.bvh \
  --constraints samples/small.constraints.json \
  --snapshot-dir work/kill-snapshots \
  --chunk-size 30 --workers 1 --crash-after-chunks 1

# 校验版本、哈希链，从第 31 帧继续且不重复提交前 30 帧
python -m mocap_workbench.cli solve \
  --bvh samples/small.bvh \
  --constraints samples/small.constraints.json \
  --snapshot-dir work/kill-snapshots \
  --chunk-size 30 --workers 1 --resume
```

只校验并读取快照：

```bash
python -m mocap_workbench.cli recover \
  --bvh samples/small.bvh \
  --constraints samples/small.constraints.json \
  --snapshot-dir work/kill-snapshots \
  --chunk-size 30 --workers 1
```

运行脚本化实验：

```bash
python -m mocap_workbench.cli experiment \
  --sample-dir samples --out artifacts/experiment --workers 1
```

实验输出包括：

- `small/batch|window/trajectory.jsonl`、`large/batch|window/trajectory.jsonl`
- 每档规模的 `comparison.json`
- `conflict/auto/conflicts.json` 与 `unreachable/auto/conflicts.json`
- `experiment.json`：残差、策略、缓存、冲突和确定性汇总
- `determinism_small/run_1|run_2/trajectory.jsonl`

## 桌面工作台

```bash
python -m mocap_workbench.cli gui \
  --bvh samples/small.bvh \
  --constraints samples/small.constraints.json \
  --snapshot-dir snapshots/workbench
```

![Workbench after a 90-frame solve](artifacts/workbench.png)

界面操作：

- `Start solve`：后台 `QThread` 求解，主线程保持可交互。
- `Pause`：在下一个帧区间边界暂停，保留最后已提交快照，可继续。
- `Cancel`：协作式取消，未完成区间不写出，保留最后完整区间。
- `Recover snapshot`：校验当前 BVH、约束版本、求解器配置和哈希链后继续。
- 约束表可修改权重和容差；`Apply` 后以新约束修订号开始求解，界面线程级缓存跨次
  求解保留，只重算签名发生变化的帧。
- 帧滑块、播放按钮和残差热力条联动三维视口；热力可选择“最大约束”或单约束。
- 残差表按当前帧显示每个约束的残差、容差、比值和满足状态。

## 约束文件

示例：

```json
{
  "schema": "mocap-constraints-v1",
  "name": "example",
  "revision": 1,
  "constraints": [
    {
      "id": "bone:LeftArm",
      "type": "bone_length",
      "joint": "LeftArm",
      "target": 0.26,
      "weight": 1.0,
      "tolerance": 0.002
    },
    {
      "id": "limit:LeftArm:z",
      "type": "joint_limit",
      "joint": "LeftArm",
      "axis": "z",
      "min_deg": -55.0,
      "max_deg": 55.0,
      "weight": 1.0,
      "tolerance_deg": 0.25
    },
    {
      "id": "effector:LeftHand_EndSite",
      "type": "effector",
      "joint": "LeftHand_EndSite",
      "targets": [[0.5, 0.7, 0.1], null, [0.6, 0.8, 0.0]],
      "weight": 1.0,
      "tolerance": 0.008
    }
  ]
}
```

`targets` 中的 `null` 表示该帧标记点缺失，约束被跳过，不计入冲突。也支持
`frame_targets`（帧号到目标或 `null` 的映射）和 `missing_frames`。根关节不能配置骨骼
长度约束；关节限位轴只接受 `x/y/z`。

## 求解与缓存

每帧先由 BVH 原始通道构建基础局部旋转和偏移，再为关节增加局部旋转向量：

```text
R_local(joint) = R_bvh(joint) @ exp(delta(joint))
world(joint)   = world(parent) @ [R_local | offset + root_translation]
```

末端位置残差对祖先关节旋转的解析 Jacobian 为
`d(position)/d(delta_axis) = world_axis x (position - joint_pivot)`。求解器使用带阻尼
Gauss-Newton、确定性线搜索和关节限位投影；关节限位由 BVH 通道顺序对应的欧拉分解
提取并投影。骨骼偏移固定，因此长度约束若目标与 BVH 实际长度不一致，会稳定地产生
可定位冲突，而不是伪造可伸缩骨骼。

残差输出：

- 每个约束的原始残差、容差、权重、适用性和满足状态。
- `total_residual = sqrt(sum(weight * residual^2))`。
- 超出容差且权重大于零的约束形成冲突帧；缺失标记点不会形成冲突。

缓存键包含骨架指纹、帧号、通道值、该帧适用的约束定义（含权重/目标）和求解器数值
参数，不包含容差。因此：

- 只改容差：复用已求姿态，仅重新判定残差，全部帧缓存命中。
- 改某帧专属目标：其他帧键不变，只重算该帧。
- 改全局约束权重：该约束适用的所有帧失效，不符合条件的缺失帧继续复用。

## 自适应策略

`auto` 使用以下确定性复杂度：

```text
complexity = frame_count * constraint_count * rotating_joint_count
```

当帧数不超过 `batch_frame_limit` 且复杂度不超过 `batch_complexity_limit` 时选择
`batch`，否则选择 `window`。也可以显式指定策略。

- `batch`：帧相互独立，使用 `ThreadPoolExecutor` 并行求解，结果按帧号排序汇聚。
- `window`：滑动窗口大小 `window_size`，步长 `window_size - window_overlap`；重叠帧
  会求两次并按平移平均、旋转正交投影融合。由于逐帧求解器确定性，实际融合结果与
  批处理逐位一致。

默认 `window_size=96`、`window_overlap=12`、快照区间 `chunk_size=240`，因此长序列
每个提交区间内部至少包含多个重叠窗口。窗口覆盖整个待求解帧范围，不截断、不丢帧。
实验在两个规模上均比较全部帧的残差和姿态哈希。

## 快照与恢复

每个快照区间在临时文件中写入、`fsync` 后通过 `os.replace` 原子重命名。快照是自定义
确定性二进制格式：magic、长度前缀的规范 JSON 头、float64 世界变换和残差矩阵。

快照包含：

- 区间起止帧、`committed_frames`（只含成功帧）
- 每帧姿态 SHA-256、`chain_start`、`chain_end`
- 约束修订号、约束语义哈希、完整求解器配置及其哈希
- BVH 哈希、骨架指纹、冲突证据

`committed_frames` 允许区间内存在冲突空洞，但冲突帧不会进入姿态数组或哈希链。
恢复时逐快照校验必需字段、格式版本、输入/约束/求解器版本、区间连续性、每帧姿态
哈希和整条哈希链；任何断链、缺失或版本不一致均返回 `E_RECOVERY_INVALID`。

同一 BVH 使用临时目录下的跨进程 advisory lock。重复启动返回 `E_BUSY`；强杀后 OS
自动释放锁。取消/异常只保留最后一次完整提交。若估算工作集超过内存预算，区间在写出
任何部分结果前以 `E_BUDGET_EXCEEDED` 中止。

## 业务码

| 代码 | 含义 |
| --- | --- |
| `E_BVH_INVALID` | BVH 层级、通道、帧数或运动数据非法 |
| `E_CONSTRAINT_INVALID` | 约束字段、关节引用或数值非法 |
| `E_BUSY` | 同一 BVH 已有求解任务 |
| `E_RECOVERY_INVALID` | 快照缺失、版本不符、字段缺失、哈希链断裂 |
| `E_BUDGET_EXCEEDED` | 区间工作集超过内存预算，未写部分结果 |
| `E_CANCELLED` | 在检查点协作式取消 |
| `E_CONFIG_INVALID` | 求解配置非法或依赖缺失 |
| `E_IO` | 本地文件读写失败 |

运行报告在 `status/error_code/message` 中携带取消或预算中止结果。CLI 映射为：
`E_BUSY=2`、BVH/约束错误 `=3`、恢复错误 `=4`、预算 `=6`、取消 `=130`。
冲突不是笼统失败：任务状态为 `completed_with_conflicts`，CLI 返回 0，并输出完整诊断。

## 确定性轨迹

`trajectory.jsonl` 第一行是运行元数据（BVH/约束 SHA-256、策略、求解器配置），之后每帧
一行，包含：

```text
frame, strategy, total_residual, conflict, conflict_constraints,
cache_hit, restored, pose_hash, residuals[]
```

日志不包含系统时间、文件大小、PID 或随机数。相同种子、相同操作序列、相同初始缓存
状态会生成逐字节相同的轨迹；同一运行生成的 `run.json` 和 `.snap` 文件也逐字节相同。

## 实际验证结果

环境：Python 3.12.16、NumPy 2.5.5、PyQt 5.15.14、PyOpenGL 3.1.10。完整测试：

```text
29 passed in 18.86s
```

全量样本实验耗时约 21.0 秒，峰值 RSS 约 76.7 MiB。样本规模：

- small：90 帧，16 个命名关节，缺失率 0.08，无冲突。
- large：1200 帧，26 个命名关节，缺失率 0.18，无冲突。
- conflict：120 帧，骨骼长度目标故意不一致，全部帧可定位冲突。
- unreachable：120 帧，从第 79 帧起左侧末端目标不可达。

实测：

| 样本 | 策略 | 帧数 | 最大总残差 | 平均总残差 |
| --- | --- | ---: | ---: | ---: |
| small | batch | 90 | 0.013163965357692493 | 0.0096809287854511 |
| small | window | 90 | 0.013163965357692493 | 0.0096809287854511 |
| large | batch | 1200 | 0.01341801487408942 | 0.009139186990347346 |
| large | window | 1200 | 0.01341801487408942 | 0.009139186990347346 |

两档规模均比较了全部帧：

- `complete=true`，无缺失帧。
- `pose_hashes_equal=true`，姿态哈希不匹配列表为空。
- `max_total_residual_delta=0.0`。
- `max_pose_abs_delta=0.0`。

其他验收结果：

- 骨骼长度冲突样本 120/120 帧产生诊断，0 帧写入提交区间。
- 不可达样本第 79 帧起出现 37 个冲突帧，冲突帧与提交帧集合严格不相交。
- 容差修改后的 90/90 帧复用缓存姿态，姿态哈希全部保持。
- 相同操作序列两次生成的 `trajectory.jsonl` SHA-256 均为
  `5a652968f6a702eddb94fbdd173467d5a5107b9ce3f788b074e3cce40ca07fb5`。
- hard-kill 测试在首个 30 帧区间原子提交后以退出码 23 结束；恢复从第 31 帧继续，
  最终 90 帧完整且不重复提交。

完整数值见 `artifacts/experiment/experiment.json`、各规模 `comparison.json`、各运行
`trajectory.jsonl` 和 `summary.json`。

当前 WSLg 的 Wayland 桌面路径已实际打开窗口：软件 Mesa 下得到 OpenGL 4.5 上下文，
完成 90 帧后台求解并正确渲染骨架、目标点和热力条；界面截图见
`artifacts/workbench.png`。XCB 插件在当前离线环境不可用，因此另以
`QT_QPA_PLATFORM=offscreen` 验证了无 GL 情况下的 QPainter 二维回退。Windows 实机
OpenGL 窗口仍需在目标桌面执行 README 顶部命令，必要时加 `--software-gl`。

## 项目结构

```text
mocap_workbench/
  bvh.py          BVH 严格解析、FK、姿态哈希
  constraints.py  约束模型、校验、逐帧适用性
  solver.py       局部迭代、残差、缓存、batch/window
  snapshot.py     原子快照、哈希链、恢复
  locking.py      跨进程单任务锁
  pipeline.py     调度、取消/暂停、预算、提交
  samples.py      确定性样本生成器
  experiment.py   两档实验与轨迹日志
  cli.py          命令行入口
  gui/            PyQt5 + PyOpenGL 工作台
tests/            29 个本地确定性测试
samples/          已生成的 small/large/conflict/unreachable 样本与基线哈希
artifacts/        实测实验输出
```

运行测试：

```bash
.venv/bin/pytest -q
```

## 说明与边界

- 局部 Gauss-Newton 不承诺任意初始姿态下的全局最优；不可满足或超过迭代上限时返回
  可定位冲突报告，而不是伪造满足。
- 固定 BVH 骨骼偏移保证旋转求解不会改变骨长；若要重定向到不同骨长骨架，应在预处理
  阶段先完成骨骼映射。
- OpenGL 不可用或驱动受限时使用 `--software-gl`；PyOpenGL 完全缺失时视口自动退化为
  QPainter 二维投影，但求解、快照、日志和残差功能不受影响。
