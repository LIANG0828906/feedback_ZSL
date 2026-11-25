# 室内一次反射点可见性求解内核

这是一个从空工作区构建的 Go 领域组件与本地示例，用确定性矩形房间关卡验证室内一次反射点可见性。组件覆盖严格加载、几何校验、门洞房间链、镜像法一次反射、定点误差报告、房间状态机、精确失效传播、SQLite 检查点恢复和三条运行路径比对。

## 快速运行

```bash
go build ./...
go test ./...
go run ./cmd/acoustic-demo -serve=false
go run ./cmd/acoustic-demo -serve -addr 127.0.0.1:8080
```

页面入口为 `http://127.0.0.1:8080/`，只允许回环地址。页面叠加显示房间、共享墙、门洞、最短房间链和反射点，并展示预览、恢复、全量重算三条路径的提交序列及比对结果。

常用参数：

```bash
go run ./cmd/acoustic-demo \
  -seed 42 \
  -workers 4 \
  -buffer 2 \
  -sample-out examples/sample_seed42.json \
  -report-out /tmp/acoustic-report.json
```

SQLite 使用 `modernc.org/sqlite v1.29.10`，该版本已在本地模块缓存中，可离线执行 `GOPROXY=off go mod tidy`。

## 工程结构

| 路径 | 作用 |
| --- | --- |
| `model.go`、`load.go`、`validate.go` | 领域模型、缺失字段检查、结构/几何校验 |
| `chain.go` | 门洞图、最短房间链、逐段整型净空检查 |
| `reflection.go` | 镜像法、反射点量化、入射角、声压级与误差上界 |
| `state.go`、`worker.go`、`session.go` | 五状态机、有界工作池、提交边界与失败隔离 |
| `invalidation.go` | 显式依赖图与精确失效传播 |
| `checkpoint.go`、`internal/sqlstore` | 快照契约、SQLite 原子检查点与恢复 |
| `compare.go`、`run_compare.go` | 真值比较与三路径结果比较 |
| `internal/samplegen` | 固定种子样本及独立解析真值 |
| `internal/demo`、`cmd/acoustic-demo` | 三路径运行器、HTTP 页面与命令行入口 |

## 定点约定

- 坐标与路径长度均为 `int64` 定点 tick，`FixedScale = 1000`，即 1 tick = 0.001 世界单位。
- 两点的整型路径长度按欧氏距离四舍五入到最近 tick；房间链逐段取整后累加。
- 反射面法线 `(nx, ny)` 也按 1000 缩放，校验平方长度接近 `1000²` 且垂直于反射面。
- 反射面吸收系数先量化到 `1e-6`。
- 声压级使用确定公式：

```text
L = LevelDB - 20*log10(d) - 8.685889638*alpha*d
```

其中 `d = pathTicks / 1000`，`alpha` 为量化后的吸收系数。一次反射每条路径只应用一个反射面吸收项。

## 镜像法

对反射面直线上的点 `A`、单位法线 `n`，计算：

```text
Q = n·n
d = n·(S-A)
e = n·(T-A)
R = [Q(eS + dT) - 2de n] / [Q(e+d)]
```

有效反射要求：

- `d`、`e` 非零且同号，即声源与听者在反射面同侧；
- 有理数交点 `R` 量化后落在反射面有效线段及听者房间内；
- `S -> R` 与 `R -> T` 只在真实门洞处穿越共享墙；
- 声源到听者存在门洞连通链。

交点先以 `math/big` 做精确整数有理运算，再按半远离零规则量化到 tick。入射角余弦以百万分之一整型输出，路径长度与坐标不落成浮点存储。

## 加载与错误契约

支持字段：

- 房间：矩形 `minX/minY/maxX/maxY`
- 共享墙段：两端点、两侧房间
- 门洞：从墙段低坐标端起始的 `start` 与正宽度 `width`
- 声源、听者：房间编号与定点坐标
- 反射面：端点、单位法线、吸收系数
- 解析真值：每个听者对每个声源、反射面路径的结果

错误码：

| 错误码 | 场景 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、JSON 类型错误、引用不存在、真值清单不完整 |
| `E_GEO` | 房间正面积重叠、门洞越界、墙段不是共享边、法线非单位、点不在唯一房间 |
| `E_NOCHAIN` | 声源与听者之间无门洞连通路径，携带源点/听者编号 |
| `E_STATE` | 非法房间状态迁移；迁移失败时原状态不变 |
| `E_CHECKPOINT` | SQLite 打开、提交、读取或版本一致性失败 |
| `E_TRUTH` | 求解结果与解析真值越界 |

`LoadProject` 会区分缺失字段与零值坐标。缺失的顶层或嵌套字段返回 `E_SCHEMA` 并带 JSON 路径；房间重叠与门洞越界返回 `E_GEO` 并带相关房间、墙段、门洞编号。

## 房间链与状态机

房间图为无向图：节点是房间，边是具体门洞。求解优先级为：

1. 房间数最少；
2. 房间数相同时定点路径长度最短；
3. 再按房间/门洞编号稳定决胜。

每条链记录实际门洞编号、逐段端点、整型总长度和取整残差。线段与墙的交点用整数有理比较判断，只有交点位于真实门洞区间内才允许穿越。

房间状态：

```text
pending -> analyzing_chain -> solving_reflection -> committed
                   \                \
                    +---------------> failed -> pending
```

已提交房间不可再次迁移，快照保存在只读副本中。失败房间可单独 `failed -> pending` 重试，其他未提交房间保持 `pending`，不会被标为失败。

## 有界并行

一次反射求解按“听者 × 声源 × 反射面”拆成任务，由固定工作线程池并行执行：

- `MaxWorkers` 默认 4，硬上限 8；
- `PerWorkerBuffer` 默认 2，硬上限 32；
- 每个 worker 使用固定容量本地缓冲区，满后批量发送；
- 结果按任务索引排序，保证并发不影响最终快照与提交顺序；
- `WorkerStats` 输出配置上限、实际观察到的最大 worker 数量和任务数。

## 失效传播

依赖图显式记录以下边：

```text
room -> reflector
room/listener -> listener_solve
reflector -> listener_solve
source -> chain
door -> chain
chain -> listener_solve
```

编辑时先做真实链比较，而不是看到门洞改动就盲目失效：

- 增加一个未被任何最短链使用的门洞，连接关系实际未变，所有房间可复用；
- 反射面吸收系数变化只失效该反射面房间及其监听求解；
- 链实际变化只标记该链下游的听者求解与听者所在房间；
- 报告逐房间输出 `invalidated/reused`、原因、失效听者编号和全部依赖边。

SQLite 检查点与项目指纹绑定，所以重新载入发生编辑的项目时整个持久化集合作废并全量重算。这避免混用旧房间结果；同一在线编辑周期内的局部复用由失效传播器负责。

## SQLite 检查点

每个已提交房间一行，事务写入以下内容：

- 房间编号与严格递增提交序号
- 反射面快照、该房间相关门洞快照
- 该房间全部听者的房间链摘要与一次反射求解结果
- SHA-256 内容哈希
- 参数版本 `ParamsVersion`
- 算法版本 `AlgorithmVersion`
- 项目指纹

存储使用单连接、互斥锁和 SQLite 事务，房间提交顺序必须严格递增。读取时逐行检查：

- 序号连续且房间顺序递增；
- 行内容哈希可重算且一致；
- 行内版本和项目指纹一致；
- 集合级元数据一致。

任一项不一致会在同一事务里删除整组快照，返回 `invalidated=true`，调用方只能全量重算。正常取消或进程强杀后，只有已提交前缀可见。

## 确定性样本与真值

固定种子 42 的样本包含：

- 3 个连续矩形房间：`0..4000`、`4000..8000`、`8000..12000`，高度均为 3000；
- 2 个共享墙段、2 个门洞；
- 3 个声源、6 个反射面、3 个听者；
- 每个听者对每个声源均有解析真值。

真值生成器按解析镜像公式独立计算结果，不调用求解器。样本容差：

```text
坐标：2 tick
路径：4 tick
入射余弦：0.001
声压级：0.05 dB
```

当前样本的有效一次反射路径数为 `8`：

| 听者 | 房间 | 有效路径数 |
| --- | --- | ---: |
| 21 | 1 | 1 |
| 22 | 2 | 3 |
| 23 | 3 | 4 |

其中包含同房间路径、跨一个门洞路径和跨两个门洞路径，能覆盖房间链参与和门洞可见性。

## 误差传播报告

每条有效路径输出四项声压级上界：

1. 房间链整段取整：`segmentCount * 0.5 tick` 转换到 dB；
2. 镜像交点二维量化：最大位移 `sqrt(2)*0.5 tick`，两段路径影响上界 `sqrt(2) tick`；
3. 反射点到听者及声源到反射点的距离取整：合计 `1.0 tick`；
4. 吸收系数定点量化：`8.685889638 * d * 0.5e-6` dB。

距离到声压级的转换使用：

```text
dBPerTick = (20/(ln10*d) + 8.685889638*alpha) / 1000
```

报告同时给出实际观察到的链残差、镜像坐标残差和距离残差。累积上界严格等于四项之和，不引入未声明参数。`CountTolerancePasses` 只按累积上界和坐标残差判定，因此容差收紧时通过项数单调不增。

## 实测结果

本机 Go 版本：`go1.22.12 linux/amd64`。

执行：

```bash
GOPROXY=off go mod tidy
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./...
go run ./cmd/acoustic-demo -serve=false
```

实际验收输出：

```text
seed=42 paramsVersion=5 algorithm=reflection-fixed-v1
rooms=3 walls=2 doors=2 sources=3 reflectors=6 listeners=3

preview commits=[1 2 3] validPaths=8
resume  commits=[1 2 3] validPaths=8
full    commits=[1 2 3] validPaths=8
worker max=4 perBuffer=2 tasksPerRoom=6 observed<=4
allEqual=true

resume loadedRooms=[1] invalidated=false fullRecompute=false
compare preview-vs-full equal=true differences=0
compare resume-vs-full  equal=true differences=0
compare preview-vs-resume equal=true differences=0

truth passed=true expectedPaths=8 actualPaths=8
maxCumulativeUpperBoundDb=0.022473
pass@1dB=8 pass@0.1dB=8 pass@0.01dB=3

invalidation connectivityChanged=false
invalidatedRooms=[] reusedRooms=[1 2 3]
```

测试覆盖：

- 顶层和嵌套字段缺失、房间重叠、角点接触、门洞越界、非单位法线、听者落在共享边界；
- 全部声源/听者房间链与真值、门洞中心路径、无链 `E_NOCHAIN` 上下文；
- 8 条反射路径与真值逐项比对，四项误差和累积上界；
- 五状态合法迁移、非法迁移保持原状态、失败房间隔离与重试；
- 未使用门洞添加不失效任何房间、反射面修改只失效对应房间；
- 预览、正常取消后恢复、全量重算三路径深度一致；
- SQLite 内容哈希篡改和算法版本失配整组作废；
- 子进程提交首房间后直接 `os.Exit`，重启只恢复该持久化前缀；
- 本地 HTTP 页面、报告 API 与健康检查。

`go test -race ./...` 与 `go vet ./...` 均通过，无 Docker 或其他容器依赖。

## 集成示例

```go
package main

import (
    "context"
    "log"

    acoustic "levelacoustic"
    "levelacoustic/internal/sqlstore"
)

func main() {
    project, err := acoustic.LoadProjectFile("examples/sample_seed42.json")
    if err != nil {
        log.Fatal(err)
    }
    store, err := sqlstore.Open("checkpoints.db")
    if err != nil {
        log.Fatal(err)
    }
    defer store.Close()

    session, err := acoustic.NewSession(project, acoustic.SessionOptions{
        Store:         store,
        WorkerOptions: acoustic.WorkerOptions{MaxWorkers: 4, PerWorkerBuffer: 2},
    })
    if err != nil {
        log.Fatal(err)
    }
    if _, err := session.Restore(context.Background()); err != nil {
        log.Fatal(err)
    }
    result, err := session.Run(context.Background())
    if err != nil {
        log.Fatal(err)
    }
    comparison := acoustic.CompareTruth(project, result.ListenerSolutions)
    if !comparison.Passed {
        log.Fatal(comparison.Error())
    }
}
```

`internal/sqlstore` 是本地示例实现。外部集成方可实现 `acoustic.CheckpointStore` 接口接入自己的持久化设施；领域包本身不依赖 SQLite。

## Windows 准备

1. 安装 Go 1.22 或更高版本；
2. 使用官方安装包并确认 `go version` 可用；
3. 按官方说明配置 `GOPATH`，确保 Go bin 目录在 `PATH`；
4. 在项目根目录执行 `go build ./...` 和 `go test ./...`。
