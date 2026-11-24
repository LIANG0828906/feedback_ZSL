# RetargetLab：骨骼动画与动作重定向诊断工作台

RetargetLab 是一个完全本地运行的 Go 项目，用确定性样本验证角色动画重定向。它包含样本生成、结构校验、正向运动学与迭代投影求解、帧窗口状态机、依赖失效、增量/全量一致性比对、SQLite 版本化检查点、断点恢复、受限并行调度和可视化诊断面板。

项目只使用标准库与纯 Go SQLite 驱动 `modernc.org/sqlite`，不需要 Docker、外部服务或网络目标。

## 快速开始

环境要求：Go 1.22+，Windows amd64 可直接构建。

```bash
go mod download

# 固定种子生成 32 帧样本（源骨架、3 个缩放目标、映射、解析真值）
go run ./cmd/retargetlab generate -out data/sample.json -seed 42 -frames 32 -phase 8

# 运行完整验收链
go run ./cmd/retargetlab verify -sample data/sample.json -target reference

# 启动诊断面板（SQLite 检查点写入 data/checkpoints.db）
go run ./cmd/retargetlab serve \
  -sample data/sample.json \
  -target reference \
  -db data/checkpoints.db \
  -addr 127.0.0.1:8080 \
  -workers 4 -window 4 -queue 8 -buffer 2
```

浏览器打开 `http://127.0.0.1:8080`。面板不会在启动时自动求解；可点击“求解待处理帧”，随后编辑映射/比例、查看失效传播、逐帧诊断、重试失败帧以及执行增量/全量一致性校验。

Windows 交叉构建检查：

```bash
GOOS=windows GOARCH=amd64 go build -o retargetlab.exe ./cmd/retargetlab
```

## 工程结构

```text
cmd/retargetlab/         CLI：generate / verify / serve / crash-helper
internal/geom/           三维向量、四元数、限位
internal/model/          数据合同、严格 JSON 加载、E_* 错误、结构校验
internal/generator/      SplitMix64 固定种子样本与解析真值
internal/kinematics/     拓扑序遍历、正向运动学、足部接触校正
internal/solver/         比例归一、局部旋转映射、角度限位、CCD 迭代投影、比对
internal/fsm/            pending → solving → committed/failed 状态机
internal/engine/         缓存、依赖失效、并行调度、提交协调、恢复
internal/store/          SQLite WAL/FULL、原子窗口检查点、整体版本失效
internal/panel/          内嵌 HTML/Canvas 诊断面板与 JSON API
internal/verify/         可重复的八项验收检查
```

## 样本与真值

`generate` 使用 SplitMix64 固定种子，输出：

- 单根人形源骨架（23 关节）及其逐帧局部四元数、根平移、动作阶段和足部接触区间。
- 3 个比例不同的目标骨架 `small`、`reference`、`large`；均包含 `cape_01` 一对多目标与 `weapon_socket` 降级关节。
- 映射描述：一对一、`head -> head + cape_01` 一对多、`weapon_socket` 的 `inherit_parent` 缺失关节降级、末端约束和比例系数。
- 每个目标的关键帧世界坐标、源/目标骨骼长度、接触起止帧，容差 `1e-6`。
- 真值通过生成器自己的解析正向运动学与接触校正式构造，不调用求解器。

映射规则示例：

```json
{
  "source": "head",
  "targets": ["head", "cape_01"],
  "weight": 1,
  "phases": [0, 1, 2, 3]
}
```

缺失关节降级示例：

```json
{
  "joint": "weapon_socket",
  "policy": "inherit_parent",
  "sourceAncestor": "hand_r"
}
```

合法降级策略为 `identity`、`inherit_parent`、`nearest_mapped_ancestor`。其余策略返回 `E_SCHEMA`。

## 错误与校验

- `E_SCHEMA`：字段缺失、非有限 T-pose 偏移、未知映射/降级引用、非法一对多/缺失声明等。
- `E_CYCLE`：父子引用成环，错误中包含参与环的关节名。
- `E_FRAME`：帧序号错位、四元数数量与关节数不一致、接触/阶段范围错误等，错误中包含帧号。
- `E_STATE`：非法帧状态迁移、冻结窗口重叠写入、非 failed 帧重试等。
- `E_DIFF`：增量/全量或真值比对失败，包含首个不一致帧号、关节名、两侧实测值。
- `E_PARAM` / `E_IO`：参数与本地 I/O 错误。

严格 JSON 加载会检查关节 `name/parent/localOffset`、帧 `index/rootTranslation/localRotations` 及四元数分量是否缺失，并将加载错误归入 `E_SCHEMA`。

## 求解核心

1. 校验源/目标骨架、动画与映射。
2. 按目标/源骨架高度求身体比例，按动作阶段合成可编辑比例系数。
3. 按拓扑序把源局部旋转映射到目标；一对多目标复用同一源旋转。
4. 对缺失关节按 `identity` / 父关节继承 / 最近已映射祖先首选降级。
5. 应用每关节最大旋转角限位并累计角度违背。
6. 正向运动学得到世界位置与旋转。
7. 对接触足执行根节点 Y 轴校正，使最低接触足位于 `y=0`。
8. 对声明末端约束执行固定迭代次数、固定步长上限的 CCD 投影；不可达时保留残差统计。
9. 再次执行足部接触校正，输出根轨迹、世界坐标摘要、约束统计和冻结快照。

求解只依赖骨架、动画、映射、参数和算法版本，不读取线程 ID、完成顺序或墙钟时间。所有可迭代集合在输出或归约前排序。

## 帧窗口、缓存与失效

每帧只有四个状态：

| 当前状态 | 允许迁移 |
|---|---|
| `pending` | `solving` |
| `solving` | `committed` / `failed` |
| `committed` | 无（冻结） |
| `failed` | `solving`（重试） |

进度为 `committedFrames / totalFrames`。已提交帧冻结映射快照、比例系数、算法/参数版本和逐帧结果，后续编辑永不回写这些帧。

编辑映射或比例时：

- 依赖图显式记录规则 → 关节子树 → 帧/动作阶段，以及比例键 → 帧/阶段。
- 除声明的下游阶段外，还会用编辑前后局部旋转或根比例逐帧实测差异，避免漏失效。
- 仅未提交且实际/声明受影响的帧被标记 `pending`（或保留 `failed`）并附加失效原因。
- 未失效的已提交帧直接复用内存/SQLite 缓存。
- 失效清单输出 `{frame, joints[], reasons[], phase}`。

映射编辑后从空缓存全量求解，与复用缓存的增量结果逐帧比较旋转、世界坐标、约束统计、根轨迹和接触状态；不一致时返回 `E_DIFF`。自动比对会比较所有“当前配置快照”帧，并显式列出因提交后冻结而跳过的帧，避免把合法旧窗口误报为差异。

> 说明：提交后冻结是业务约束，因此“全量重算”用于校验当前活动配置产生的结果；被冻结的旧窗口不会因后续编辑改变。验收链中的映射编辑在全量提交前完成，因此可比较 32/32 帧；比例阶段编辑则验证 16 帧缓存复用和 8 帧失效重算。

三条一致性路径均直接使用同一纯函数求解核心：

1. 一次性全量：空缓存按帧序求解全部帧。
2. 实时增量：保留已提交缓存，仅重算失效帧及其动作阶段下游。
3. 断点恢复：从 SQLite 校验通过的已提交窗口恢复，再求解剩余帧。

自动比对按当前配置快照逐帧核对关节局部/世界旋转、世界坐标、约束统计、根轨迹和接触状态。

## SQLite 检查点与恢复

- 驱动：`modernc.org/sqlite`，WAL + `synchronous=FULL`。
- 每个提交窗口在单个事务中写入：帧范围、骨架联合哈希、映射快照/哈希、比例快照、逐帧结果、内容哈希、参数版本、算法版本、单调提交序号。
- SQLite 连接最大打开数为 1，存储层互斥锁 + 事务保证写入串行、不交错。
- 正常并行运行由提交协调器按帧顺序分段提交，单线程和多线程产生相同窗口内容与提交序号。
- 重启恢复会重算每个窗口内容哈希，并检查所有窗口的源+目标骨架联合哈希、参数版本、算法版本、窗口连续性和逐帧快照一致性。
- 任一检查失败即在一个事务内删除全部窗口，所有帧回到 `pending`，绝不混用部分旧结果。
- 强制退出测试使用真实子进程 `os.Exit(23)`，不关闭数据库；父进程重新打开后只恢复 12 个已提交帧，其余帧为 `pending`。

`crash-helper` 子命令用于手工复现强制退出：

```bash
go run ./cmd/retargetlab crash-helper \
  -sample data/sample.json -db data/crash.db -prefix 12
# 进程以 23 退出且不 Close 数据库

go run ./cmd/retargetlab serve -sample data/sample.json -db data/crash.db
```

## 并行与资源边界

- `-workers` 控制最大工作线程数，硬上限 64。
- `-queue` 是每线程有界任务队列；队列满时生产者阻塞，不丢弃任务。
- `-buffer` 是每线程状态缓冲额度；全局缓冲信号量为 `workers × buffer`，耗尽时排队等待。
- 单帧 panic/异常只把该帧置为 `failed`，同窗口其他成功帧仍按帧序提交；失败帧可单独或批量重试。
- SQLite 写入由协调器串行执行，窗口按帧顺序推进。

面板“提交窗口/资源边界”区域显示 worker 数、队列/缓冲上限、峰值排队、峰值并行和阻塞派发次数。

## 诊断面板

面板所有数据来自 `engine.StateSnapshot()` 和 `engine.FrameDetail()`，不使用独立副本：

- 源/目标骨架层级树与角度限位。
- 一对一/一对多/降级映射规则，可在线编辑源、目标、权重和阶段。
- 逐帧角度/末端约束违背曲线。
- 足部接触时间轴与接触校正后的根轨迹 XZ 投影。
- 帧窗口状态矩阵：四态颜色、失效斜纹、缓存命中白点。
- 依赖失效传播清单与依赖数量。
- 逐帧局部旋转、世界坐标、约束、接触、快照哈希和失败原因；failed 帧可直接重试。
- 提交窗口、内容哈希、提交序号和线程池资源统计。
- 真值核验与增量/全量自动比对按钮。

JSON API：

```text
GET  /api/state
GET  /api/frame?frame=12
POST /api/mapping   {"index":0,"rule":{"source":"...","targets":["..."],"weight":1,"phases":[]}}
POST /api/scale     {"key":"gesture","value":1.08}
POST /api/solve
POST /api/retry     {"frame":5} 或 {}
POST /api/validate
GET  /api/truth
```

## 实测验证

环境：Go `1.22.12`，Linux amd64；Windows amd64 交叉构建通过。

```bash
go test ./...
go test -race ./...
go run ./cmd/retargetlab verify -sample data/sample.json -target reference
GOOS=windows GOARCH=amd64 go build ./cmd/retargetlab
```

实际结果：

- `go test ./...`：engine、model、panel、solver 全部通过，包含强制进程退出恢复测试。
- `go test -race ./...`：全部通过，无数据竞争。
- Windows amd64 交叉构建：通过。
- 验收链 8/8 通过，详见 `artifacts/verification.json`：
  - 工作区校验：23 关节 / 32 帧。
  - 全量求解与解析真值：32 帧通过。
  - 映射编辑增量 vs 全量：32/32 帧一致。
  - 阶段比例编辑：16 帧缓存命中、16 帧缓存未命中、8 帧失效，重算段 16/16 一致。
  - 并行 vs 串行：32 帧结果与 8 个提交窗口一致。
  - SQLite 恢复：恢复 24 帧 / 6 窗口，补齐后 32/32 帧与全量一致。
  - 参数版本不匹配：所有窗口整体作废，剩余窗口数 0。
  - 单帧故障隔离与重试：帧 5 失败，邻帧与已提交窗口不受污染，重试后提交。
- 强制退出测试：子进程提交 12 帧后以 23 退出；重启仅恢复 12 帧 / 3 窗口，帧 12–31 保持 `pending`，补齐后与全量一致。

测试日志保存在：

- `artifacts/test-results.txt`
- `artifacts/race-results.txt`
- `artifacts/verification.json`

## 假设与边界

- 骨架为单根层级；允许多个叶子和 1:N 映射。
- 源动画的局部旋转是相对 T-pose 的增量，目标按拓扑序直接映射。
- 比例系数用于根平移/末端目标的阶段缩放；目标骨骼自身长度来自目标 T-pose。
- 已提交窗口不可编辑或覆盖，这是冻结语义的一部分。
- 全量一致性校验以当前活动配置为准；旧冻结窗口与编辑后配置的差异属于预期状态，面板会明确显示每帧快照哈希。
