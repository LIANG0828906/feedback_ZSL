# 双足步态相位锁定动作重定向组件

这是一个仅使用 Go 标准库实现的确定性实验工程：生成源步态与目标骨架，按步态相位重定向根轨迹和双足腿部关节链，输出足部落地/离地事件，并支持周期级检查点、暂停、重启续跑和三路径逐帧比对。

核心约束：

- 不引入第三方 Go 依赖，不访问网络，不使用 Docker/容器。
- 固定种子生成 `data/`，相同 Go 版本和参数下输出可重复。
- 每个提交周期独立求解；实时、断点恢复、全量重算三条路径输出逐样本一致。
- 检查点采用“内容哈希 + 输入哈希 + 参数版本 + 算法版本”整体校验。任一不匹配时删除全部旧周期快照并全量重算，不会混合新旧周期。

## 环境准备

要求 Go 1.22 或更高版本。

Windows 使用官方安装包安装后，在 PowerShell 中确认：

```powershell
go version
go env GOVERSION GOOS GOARCH
```

本仓库实机验证环境为 Go 1.22.12 / linux / amd64（WSL Ubuntu）。Windows 使用同一 Go 源码和命令行参数。

模块初始化为：

```text
module gaitretarget
go 1.22
```

## 快速开始（Windows PowerShell）

在仓库根目录执行：

```powershell
go test ./...
go vet ./...

go run .\cmd\gaitretarget generate --seed 42 --out .\data
go run .\cmd\gaitretarget validate --data .\data
go run .\cmd\gaitretarget run --mode full --data .\data --source src-walk-001 --target target-compact-01 --out .\artifacts\full-result.json --mapping-out .\artifacts\mapping.json
go run .\cmd\gaitretarget verify --data .\data --source src-walk-001 --target target-compact-01 --store .\artifacts\verify-main --interrupt-after 2 --out .\artifacts\verification-report.json
```

成功时会看到：

```text
three-path verification OK: source=src-walk-001 target=target-compact-01 frames=138 cycles=3 realtime_loaded=0 resume_loaded=2
```

Windows 全量验收脚本：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify-all.ps1
```

Linux/WSL 全量验收脚本：

```bash
./scripts/verify-all.sh
```

检查点目录必须使用专用目录。清理示例：

```powershell
Remove-Item -Recurse -Force .\artifacts\verify-main
```

```bash
rm -rf ./artifacts/verify-main
```

## 本地数据

固定种子 `42` 生成：

- 2 个源步态循环，每个 3 个周期；
- 6 个目标骨架，腿长比例分别为 0.82、1.00、1.18；
- 目标趾部关节数分别为 1、2、3；
- 解析真值：相位切换帧、每周期步长、支撑脚世界位置、根轨迹关键帧、足部事件。

文件契约：

| 文件 | 内容 |
| --- | --- |
| `data/sources.json` | 骨架层级、T 姿态局部偏移、逐帧根平移、双足位置/速度、周期切分 |
| `data/phases.json` | 每帧 `global`、`left`、`right` 相位标注 |
| `data/targets.json` | 目标骨架、腿长比例、踝 ROM、可达高度、趾关节、目标周期长度 |
| `data/truths.json` | 解析真值与足部落地/离地事件 |
| `data/params.json` | 阈值、容差、并行度、周期缓冲上限、参数/算法版本 |

相位约定：

- `left`/`right` 是单足 `stance`/`swing` 状态。
- `global` 是周期主导相位：双足支撑为 `double_support`；左足支撑、右足摆动为 `stance`；左足摆动为 `swing`。
- 相位合法性要求 `stance` 与 `swing` 之间通过 `double_support` 过渡，避免瞬时跳相。

## 处理流程

1. **结构校验**：检查字段、骨架父引用无环、周期连续、相位全覆盖、相邻相位合法、真值帧数/周期数一致、足部轨迹无跳变。
2. **相位映射**：按目标 `cycle_frames` 建立源帧到目标帧的可查询映射；目标周期长度必须与“源周期长度 × 腿长比例”一致（允许 1 帧取整误差），三类相位占比偏差不超过 `phase_ratio_tolerance`。
3. **腿部求解**：支撑期将足部固定在支撑点并贴合地面；摆动期按源抬足高度、腿长比例、趾关节数量计算抬足高度，并检查目标可达区间。使用双骨 IK 求髋/膝/踝，踝角限制在 ROM 内，再求解目标骨架声明的趾关节链。
4. **相位复判与校正**：用目标足部竖直高度和水平速度重新判定相位。与映射相位不一致时做一次局部校正，记录帧号、原因、映射相位和检测相位。
5. **周期状态机**：`pending -> aligning -> solving -> committed`；失败可进入 `failed -> aligning` 重试；`committed` 不可逆。
6. **原子提交**：按周期号顺序写入检查点，临时文件 `fsync + rename`，写入由互斥锁串行化。
7. **三路径比对**：全量、实时（带检查点）、中断后重启三条路径逐帧比较相位、根轨迹、足部位置/速度、关节、事件和校正记录。

状态迁移表：

| 当前状态 | 允许迁移 |
| --- | --- |
| `pending` | `aligning`, `failed` |
| `aligning` | `solving`, `failed` |
| `solving` | `committed`, `failed` |
| `failed` | `aligning` |
| `committed` | 无（终态） |

非法迁移返回 `E_STATE`，原状态保持不变。

## 检查点语义

每个周期写入 `cycle-000000.json` 一类文件，内容包括：

- 周期相位快照；
- 周期缩放比；
- 逐帧根平移、足部位置/速度、关节结果；
- 落地/离地事件与校正记录；
- 内容哈希；
- 输入哈希、参数版本、算法版本。

加载时逐文件校验。任何文件损坏、内容哈希不匹配、输入哈希不匹配、参数版本不匹配或算法版本不匹配，都会删除整个快照集合，所有周期回到待处理状态并全量重算。

运行中修改参数只影响未提交周期；已提交周期的相位、缩放比、根轨迹和事件保持冻结。若使用不同参数版本重启，则按上述规则整体作废，避免新旧周期混用。

## 错误码

| 错误码 | 触发条件 | 定位信息 |
| --- | --- | --- |
| `E_SCHEMA` | 必填字段缺失、真值帧数/周期数不匹配 | 帧号、字段 |
| `E_CYCLE` | 骨架父子引用成环 | 骨骼名/帧 0 |
| `E_PHASE` | 相位缺失、覆盖不全、非法相邻迁移 | 周期、帧号 |
| `E_ALIGN` | 目标周期数/时长与腿长比例不一致，或相位占比偏差超限 | 冲突周期、两侧占比 |
| `E_HEIGHT` | 抬足高度超出目标可达区间 | 周期、帧号、足部 |
| `E_PARA` | 阈值、并行度、缓冲上限越界 | 字段名 |
| `E_STATE` | 非法周期状态迁移 | 周期 |
| `E_JUMP` | 足部相邻帧位置跳变 | 周期、帧号、足部 |
| `E_RATIO` | 腿长比例非法 | 周期、帧号、字段 |
| `E_DUR` | 周期时长为零或负 | 周期、帧号 |
| `E_DIVERGE` | 三路径或自动比对首个样本不一致 | 帧号、足部、字段 |
| `E_BUFFER` | 单周期帧数超过缓冲上限 | 周期、帧号 |

## CLI

```text
gaitretarget generate [--seed 42] [--out data]
gaitretarget validate [--data data]
gaitretarget run --mode full|realtime|resume [flags]
gaitretarget verify [flags]
```

常用 `run` 参数：

- `--mode full`：无检查点全量重算。
- `--mode realtime`：逐周期提交并写检查点。
- `--mode resume`：从指定 `--store` 已提交周期续跑。
- `--interrupt-after N`：最多推进 N 个周期，用于模拟暂停/退出。
- `--reset`：运行前清理指定检查点目录。

`verify` 会在 `--store` 下创建 `realtime/` 和 `resume/` 两个隔离目录，自动比较三条路径；不一致时返回 `E_DIVERGE`。

## 测试与验收

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

测试覆盖：

- 固定种子逐字节确定性；
- `E_SCHEMA`、`E_CYCLE`、`E_PHASE`、`E_ALIGN`、`E_HEIGHT`、`E_PARA`、`E_STATE`、`E_JUMP`、`E_RATIO`、`E_DUR`、`E_DIVERGE`、`E_BUFFER`；
- 相位占比与源帧可追溯映射；
- 支撑脚地面约束和滑动速度上界；
- 趾关节数量、踝 ROM、根轨迹和事件时间轴；
- 周期失败隔离、参数只影响未提交周期、失败重试；
- 检查点恢复、内容篡改整体失效、版本不匹配整体失效；
- 串行/并行结果一致、检查点文件顺序稳定；
- 暂停、恢复、三路径逐帧一致及首个不一致定位。

实际命令输出、覆盖矩阵和校验和见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。算法与实现细节见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。
