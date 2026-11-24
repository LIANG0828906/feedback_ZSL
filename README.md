# 单机游戏存档互操作中间件

这是一个纯 Go、无外部服务的存档语义互操作实现。它把 `aurora`、`ember`、`nova`
三个发行分支的确定性样本加载到同一个内部语义模型，再交叉导出到任意分支，并用
语义读回、语义指纹、SQLite 分窗口检查点和三路径比对完成验收。

## 快速开始

```bash
go version
go mod download

go run ./cmd/savegen -out testdata/samples -seed 20261124 -count 6
go run ./cmd/savectl verify -dir testdata/samples -db testdata/checkpoints.db -window 3

go test ./...
go build ./...
```

Windows 命令行使用相同的 Go 命令；检查点数据库就是普通本地文件：

```bat
go run ./cmd/savegen -out testdata\samples -seed 20261124 -count 6
go run ./cmd/savectl verify -dir testdata\samples -db testdata\checkpoints.db -window 3
go test ./...
go build ./...
```

`testdata/samples` 已包含生成结果，可跳过重新生成。依赖固定为纯 Go SQLite 驱动
`modernc.org/sqlite v1.29.10`，兼容 Go 1.22。

## 三个分支的语义差异

物理容器深度一致，差异来自字段语义、时间刻度和条件标志：

| 语义 | aurora | ember | nova |
|---|---|---|---|
| 关卡 | `progress.level` | `progress.level` | `progress.level` |
| 金币/经验 | `stats.valueA` / `stats.valueB` | 同字段互换为经验/金币 | `wallet.amountA` / `wallet.amountB` |
| 道具/任务 | `inventory.primary` / `inventory.secondary` | 同字段互换为任务/道具 | `bags.listA` / `bags.listB` |
| 坐标 | `world.pos.x/y` | `world.pos.x/y` | `world.pos.x/y` |
| 时间 | 毫秒 tick，纪元 `1700000000` | 秒 tick，纪元 `1600000000` | 微秒 tick，纪元 `946684800` |
| 条件标志 | `flags.eventOn` | 同物理字段语义反转 | `switches.eventState` + `switches.legacyOk` |

内部时间统一为 Unix 纳秒，换算只使用有检查的 `int64` 乘加和显式取整模式
（`floor` 或 `toward-zero`）。样本时间按整秒生成，因此三种 tick 刻度都可无损表示；
导出时若目标刻度不能精确表示时间点，返回 `E_TIME`，不做浮点累积或静默截断。

映射表位于 `testdata/samples/mappings.json`，运行时可通过
`Middleware.MappingTable().Query(branch, role)` 和 `QueryAll()` 查询。

## 内部模型与指纹

内部模型只包含业务语义：关卡、金币、经验、道具、任务、坐标、时间点和整数刻度。
源分支标识在加载结果上，不在语义模型上。条件奖励会先求值并计入金币/经验总额；
条件分解只作为导出裁决的溯源信息，不参与语义指纹。

指纹为规范化 JSON 的 SHA-256，只包含语义值、规则版本和整数时间刻度，不包含源分支、
物理字段顺序、条件字段书写顺序或 JSON 对象遍历顺序。跨分支同一业务语义得到相同指纹。
`core.CheckFingerprintCollisions` 用于发现“不同规范化语义却相同指纹”的碰撞，
返回 `E_FP` 并给出两个条目和两侧指纹。

## 条件与冲突裁决

条件规则支持 `flag`、`and`、`or`、`not`，求值栈检测循环引用并返回 `E_CYCLE`
及完整引用链。条件标志先按分支规则反转，再参与求值；标志缺失返回 `E_COND`，
并携带物理字段路径、分支和实测值。

导出时优先保留源条件分解。若目标分支条件不成立（例如 `nova` 的金币条件还依赖
源分支不存在的 `legacy_enabled`），按声明优先级裁决：

1. `preserve`：保持原业务总额，把条件贡献折叠进基础字段；
2. `default`：使用目标分支声明的默认值；
3. `reject`：拒绝导出并返回 `E_CONFLICT`。

规则先按 `priority` 排序，不依赖配置切片或 map 的遍历顺序。每次裁决写入
`Ruling`，包含源/目标分支、条件 ID、原值、结果、策略和原因。

## 检查点与续跑

SQLite 表：

- `checkpoint_runs`：运行 ID、模式、源/目标分支、映射版本、窗口大小、输入哈希；
- `checkpoint_windows`：窗口索引、语义条目范围、增量指纹、累计指纹、内容哈希、窗口补丁。

每个窗口在独立 `BEGIN IMMEDIATE` 事务中串行写入；内容哈希覆盖引擎版本、运行元数据、
窗口范围、增量指纹和载荷字节。恢复时先验证整次运行：窗口必须从 0 连续、范围连续、
累计指纹链一致、内容哈希一致。任一版本或哈希失配会删除该运行的全部窗口，返回 `E_VER`（含冲突窗口
和期望/实际值）；`RecomputeOnConflict=true` 时自动作废并全量重算，同时在
`CheckpointResult.Recovery` 中保留 `E_VER` 通知。

导入和导出都按固定语义操作窗口推进。窗口内部失败不提交该窗口，已完成前缀仍可
在相同输入哈希、规则版本和窗口大小下重试；上下文取消只留下已提交且校验通过的窗口。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_SEM` | 语义字段缺失、类型错误或无法判定，含字段路径/分支/冲突语义 |
| `E_MAP` | 映射规则版本不匹配，含中断条目和两侧分支 |
| `E_TIME` | 时间整数换算溢出或目标刻度无法精确表示 |
| `E_COND` | 条件标志缺失/类型错误或条件规则无效 |
| `E_CYCLE` | 条件图存在循环，含引用链 |
| `E_SEMEQ` | 交叉导出读回后首个语义不等价项及两侧值 |
| `E_FP` | 指纹碰撞，含碰撞条目和两侧指纹 |
| `E_VER` | 检查点窗口/版本/哈希冲突 |
| `E_DIFF` | 三路径一致性比对失败，含首个内部语义名和窗口号 |
| `E_CONFLICT` | 目标分支按策略拒绝导入条件字段 |

## 三路径一致性

`savectl verify` 对每份样本执行：

- 实时导入：`LoadBytes` 一次性加载；
- 断点恢复：相同输入先写 SQLite，再重新打开数据库从已提交窗口恢复；
- 一次性全量：直接遍历全部语义操作。

`CompareThreePaths` 逐语义条目、映射痕迹和裁决明细比较，首个差异返回 `E_DIFF`，
并根据窗口大小计算窗口编号。测试还注入单窗口异常，确认失败窗口不落库、重试后
结果与全量导入逐项一致。

## 实际验证结果

本机使用 Go 1.22.12 验证；Windows 目标通过 `GOOS=windows GOARCH=amd64 go build ./...`
交叉编译。执行结果：

```text
go test ./...                         PASS
go vet ./...                          PASS
GOOS=windows GOARCH=amd64 go build    PASS
生成样本                              6 份 × 3 分支
交叉导出                              54 次，全部 E_SEMEQ=EQUAL
样本 sample-01 三分支指纹              fee36b7b146d37c6...
检查点窗口                            sample-01/aurora 共 5 窗口
三路径比对                            full=fresh=resumed, EQ, EQ, EQ
```

完整验收输出可重新运行：

```bash
go run ./cmd/savectl verify -dir testdata/samples -db testdata/checkpoints.db -window 3
```

当前执行环境为 Linux，无法运行 Windows 官方 `.msi` 安装器；因此未声称执行过安装器，
但已实际完成 Windows/amd64 目标编译验证。Windows 本机只需安装 Go 1.22+ 后运行上方命令。
