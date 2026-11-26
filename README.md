# MirrorLab：观战投屏会话工程

MirrorLab 是在空目录中独立成立的 Go 实现，用确定性会话样本验证观战投屏的
三版本 schema、区域映射、逻辑时间拓扑、逐窗口分发、SQLite 检查点和三路径
一致性。整个实现不依赖真实投屏设备、RTP/H.264 服务、Docker 或外部网络。

主包为 `mirrorlab`，命令行在 `cmd/mirrorlab`。所有样本由仓库内生成器按固定
种子产出；精度真值只来自生成器构造的 `Truth` 清单。

## 快速开始

环境：Linux（WSL Ubuntu），Go 1.22+。SQLite 驱动为 `modernc.org/sqlite`。

```bash
go mod download
go test ./...
go vet ./...

# 生成固定种子样本
go run ./cmd/mirrorlab generate -seed 42 -out samples/seed42

# 三版本装载/往返、三路径一致、真值核对，并写 verification.json
go run ./cmd/mirrorlab demo -seed 42 -dir artifacts/demo

# 本地诊断页
go run ./cmd/mirrorlab serve -seed 42 -dir artifacts/demo -addr 127.0.0.1:8090
```

诊断页地址为 `http://127.0.0.1:8090/`，另有只读 JSON 接口
`/api/state` 与健康检查 `/healthz`。页面展示拓扑、逐窗口逐屏包明细、四项
误差累计上界、裁决/失效记录和检查点摘要。

仓库已包含固定种子 42 的生成物：

- `samples/seed42/session.v1.json`
- `samples/seed42/session.v2.json`
- `samples/seed42/session.v3.json`
- `samples/seed42/truth.json`
- `artifacts/seed42-verification.json`

## 目录

| 文件 | 职责 |
|---|---|
| `model.go` | 内部会话模型、枚举、归一化和交叉校验 |
| `schema_load.go` / `schema_common.go` | V1/V2/V3 解析、字段路径错误和转换痕迹 |
| `schema_export.go` | 显式版本导出、损失清单、往返等价检查 |
| `generator.go` | 固定种子样本、三版本文档和真值构造入口 |
| `mapping.go` | 整型区域映射、色度对齐、裁剪与误差观测 |
| `distribute.go` | 按包上限切分、时序标记和内容哈希 |
| `timeline.go` | 同刻事件裁决、端集合与拓扑演进 |
| `state.go` | 会话/受控屏/帧窗口三层状态机 |
| `checkpoint.go` | `database/sql` + SQLite 原子检查点与组件校验 |
| `scheduler.go` | 最大并行度、线程缓冲和队列预算 |
| `engine.go` | 实时、断点恢复、全量重放与 E_DIFF 定位 |
| `truth.go` | 真值清单与结果逐字段比对 |
| `web.go` | `net/http` 诊断页与 `/api/state` |

## 内部模型与单位

所有执行层只使用统一内部模型：

- 坐标：`int64` 绝对逻辑像素，窗口区域为 `{x,y,w,h}`。
- 时间：内部 tick；`tick_ms` 由会话声明，样本为 10ms。
- 缩放：整数百分比 `Scale`，支持 `100/125/150/200`。
- 色度：`4:4:4`、`4:2:2`、`4:2:0`、`4:1:1`。
- 包大小：字节数，逐屏上限与窗口上限取较小值。

加载器保留 `SourceVersion`，并对缩放、色度、时间、坐标、拓扑、默认值补齐等
转换写入 `Session.Traces`。升级时缺失的 `default_scale` / `default_chroma`
按声明默认值补齐，痕迹包含字段路径、旧值 `null`、来源规则和新值。

## 三版本 Schema

三份文档表达同一内部会话。样本只使用三版本共同可表达的枚举值，因此三版本
装载后语义完全等价。

| 维度 | V1 (`1.0`) | V2 (`schemaVersion: 2`) | V3 (`schemaVersion: "v3"`) |
|---|---|---|---|
| 受控屏布局 | 扁平 `screens`，域通过 `domain` 引用 | `topology[]` 按域分组，屏嵌套在组内 | `topology.roots[]` 递归树，含 `children[]` |
| 坐标 | 绝对 `x/y/width/height` | 相对 `originScreenId` 的 `dx/dy` | 相对 `originScreenId` 的 `dx/dy` |
| 区域偏移范围 | `int64` 绝对值 | 有符号 32 位 | 有符号 32 位 |
| 时间 | tick | 毫秒 | `{sec,ms}` |
| 超时 | tick | 毫秒 | `{sec,ms}` |
| 缩放编码 | `"100%"` / `"125%"` | `10000` / `12500` / `15000` | `"S100"` / `"S125"` / `"S150"` / `"S200"` |
| 色度编码 | `"444"` / `"420"` | `0` / `1` / `2` | `"YUV444"` / `"YUV422"` / `"YUV420"` / `"YUV411"` |
| 可选默认值 | 可省略，由 `defaults` 补齐 | 可省略，由能力表首项补齐 | 可省略，由能力表首项补齐 |

V1/V2/V3 的枚举支持集不同，这是显式版本能力边界。导出到低版本时若遇到
低版本无法表达的缩放、色度或 32 位偏移，返回 `E_LOSSY` 和完整损失清单，
首项路径即首个不可表达字段；不返回不可还原的伪产物。

坐标与时间换算全部使用整数。乘法/除法以 `math/big` 计算中间值并检查
`int64` 边界，溢出返回 `E_OVF`，错误中带字段和操作数。V2/V3 毫秒到 tick
的余数按文档 `rounding.time` 的 `floor` / `ceil` / `nearest` 规则处理，并将
源取整残差写入转换痕迹和窗口时间误差报告。

## 区域映射

对受控屏 `s` 和窗口 `w`：

```text
local.x   = w.region.x - s.origin.x
scaled.x  = floor(local.x * w.scale / 100)
aligned.x = floor(scaled.x / chromaAlignX) * chromaAlignX
output    = clip(aligned, [0, s.width) x [0, s.height))
```

色度对齐参数：444 为 1x1，422 为 2x1，420 为 2x2，411 为 4x1。裁剪后会再
修剪到对齐边界，保证输出仍满足色度采样块约束。

映射要求：

- 请求缩放必须在屏的能力集合内，否则 `E_CAP`，错误携带屏 ID、请求缩放和
  支持集合。
- 请求色度必须在屏能力集合内，否则 `E_CAP`。
- 裁剪后为空返回 `E_CAP`。
- 相同输入重复映射逐字段相同，与屏遍历顺序、线程调度无关。

逐屏误差字段：

| 字段 | 单位 | 含义 |
|---|---|---|
| `coordinateResidualX/Y` | 逻辑像素 | schema 坐标转换残差 |
| `scaleRoundingX/Y` | 1/100 像素 | floor 缩放余数，分母固定 100 |
| `cropLeft/Right/Top/Bottom` | 像素边 | 色度对齐与边界裁剪量 |
| `timebaseResidualMs` | 毫秒 | 源毫秒到内部 tick 的取整残差 |

累计上界只由已观测残差与声明分母相加，不使用任何未声明常数或反推结果：

- 坐标：`Σ |coordinateResidual|`
- 缩放：`Σ scaleRounding`，分母 100
- 裁剪：`Σ cropTruncation`
- 时间：`Σ |timebaseResidualMs|`

## 包切分

每屏有效包上限为 `min(window.maxPacketBytes, screen.maxPacketBytes)`。每像素
字节数为 444=3、422=2、420/411=3/2。最大像素数按整数向下计算，随后以确定
的行优先顺序切 tile。每包记录序号、覆盖区域、窗口内起止 tick、字节数和
稳定内容哈希。

任一包超过上限返回 `E_PACK`，携带包序号、实测字节和上限。时序标记始终落在
窗口 `[start,end]` 内。没有真实编码载荷；本版本用区域、大小、时间和哈希
表达可诊断的镜像包元数据。

## 时间裁决与状态

同一 tick 的事件按显式优先级降序处理，同优先级再按事件 ID 字典序打破平局：

| 事件 | 优先级 |
|---|---:|
| leave | 400 |
| topology | 300 |
| negotiate | 200 |
| join | 100 |

每条裁决记录包含 tick、事件类别、胜出/抑制对象、结果和依据。若同一 tick
出现重复事件 ID，无法形成唯一顺序，返回 `E_ARB`，携带 tick 和冲突事件集合。

加入先把屏放入端集合并标记待协商；negotiate 前不参与映射。退出立即移出
端集合，并把包含退出时刻且未完成的窗口标记为失效。退出后重新加入会推进
该屏代次，允许安全重试，且不会重新接收失败前未完成窗口。

状态迁移：

| 层 | 合法迁移 |
|---|---|
| 会话 | established→negotiating→mirroring；mirroring↔paused；任意未结束→ended |
| 受控屏 | pending→negotiating→committed；pending/negotiating/committed→failed；failed→pending；重新加入可回到 pending |
| 帧窗口 | pending→distributing→committed；pending/distributing→failed；failed→pending |

非法迁移返回 `E_STATE`，原状态与进度不变。

## 原子检查点

SQLite 表：

- `checkpoints`：会话、窗口序号、窗口 ID、内容哈希、源版本、算法版本、
  已消费事件游标和完整状态 JSON。
- `checkpoint_components`：`endpoints`、`generations`、`events`、
  `screen:<id>`、`window:<id>` 的独立载荷与哈希。

每次窗口提交在一个串行化事务中写入主行和全部组件；写入由进程内互斥锁与
数据库事务共同串行，窗口序号必须连续。内容哈希和组件哈希都基于 `UseNumber`
规范化后的 JSON，避免结构体字段顺序影响校验。

恢复从最新窗口向前验证主行、内容哈希、版本、事件游标和每个组件。任一项
不一致就整体删除该窗口并回退到上一检查点，返回被拒窗口列表。每屏代次水位
也持久化，保证重启后重入事件产生相同代次。

## 三路径与预算

`RunThreePaths` 在相同会话上执行：

1. 实时路径：逐窗口提交并写 SQLite。
2. 断点路径：先提交前一半窗口，关闭存储，再打开同一数据库恢复后继续。
3. 全量重放：无持久化一次性执行。

比对包含逐窗口提交序列、逐屏区域/映射参数/包、端集合、裁决明细、失效、
误差报告和最终状态；忽略运行模式、检查点副本和调度耗时统计。首个差异返回
`E_DIFF`，并填入窗口 ID、屏 ID 和字段名。

`BudgetConfig` 限制最大工作线程数、每线程 inbox 缓冲和共享队列预算。预算
耗尽时生产者在队列上阻塞，并记录任务 ID、原因和等待微秒数；任务不静默
丢失。单屏映射/切包错误只写入该屏失败记录，其他屏与已提交窗口继续推进。

## 错误码

| 错误码 | 触发场景 |
|---|---|
| `E_SCHEMA` | 未知版本、必填缺失、枚举越界、引用/拓扑非法 |
| `E_LOSSY` | 低版本无法表达字段，或导出后无法还原 |
| `E_OVF` | 整数换算溢出，含字段和操作数 |
| `E_CAP` | 缩放/色度能力不符或裁剪为空 |
| `E_ARB` | 同刻事件无法唯一裁决 |
| `E_PACK` | 包尺寸或时序标记越限 |
| `E_STATE` | 非法状态迁移、检查点版本/顺序错误 |
| `E_DIFF` | 三路径或真值首项不一致 |

`*mirror.Error` 是统一错误类型，可直接读取 `Code`、`Path`、`ScreenID`、
`WindowID`、`Field`、`Tick`、`Operands`、`Losses` 等结构字段。

## API 示例

```go
sample, err := mirror.Generate(42)
if err != nil { /* handle */ }

v1, err := mirror.LoadVersion(sample.Documents[mirror.SchemaV1], mirror.SchemaV1)
if err != nil { /* E_SCHEMA ... */ }

restored, data, losses, err := mirror.RoundTripSession(v1, mirror.SchemaV1)
if err != nil { /* E_LOSSY ... */ }

report, err := mirror.RunThreePaths(context.Background(), sample.Session,
    "/tmp/mirrorlab-checkpoints", mirror.DefaultBudget())
if err != nil { /* E_DIFF ... */ }

if err := mirror.CompareResultToTruth(report.Replay, sample.Truth); err != nil {
    /* truth mismatch */
}
```

## 实测结果

本机 Linux（WSL Ubuntu）Go 1.22.12 实测：

```text
go test ./... -count=1
ok  	mirrorlab	~0.4s

go test -race ./... -count=1
ok  	mirrorlab	~4s

go vet ./...
# 通过，无输出
```

测试共 32 个顶层用例（含子用例共 40 个），主包语句覆盖率约 74.6%。覆盖三版本
等义/往返、默认补齐痕迹、错误路径、`E_LOSSY`、`E_OVF`、`E_CAP`、`E_ARB`、
`E_PACK`、`E_STATE`、`E_DIFF`、检查点篡改回退、单屏隔离与重试、预算背压、
诊断页和真值核对。

固定种子 42 的验证记录见 `artifacts/seed42-verification.json`，摘要：

```text
session: mirror-session-0000000042
windows: 6
packets: 430
packet bytes: 44,161,875
schema 1/2/3 load: true
schema 1/2/3 equivalent: true
schema 1/2/3 round-trip: true
three paths matched: true
truth realtime/resume/replay: true/true/true
last checkpoint sha256: ef3c1733334cbce4cb663d26ed3380bd9a577eea0e58bce2f90fc3213828e0c2
```

文档哈希：

```text
V1 6bba9e1d2f9f5c71806f55f29a7e834e86a0a891bd208e802d442795c39b2aed
V2 8168f0f940a5d0794f54f172fa4ace8b07d254527129241fbed178dbb4df4fdc
V3 80016f85f841ef21e016d068c8c95c5bb383261913b0d1d9f8ecc10654b4378c
```

另以种子 `1,2,7,42,99,2026` 运行同一验收命令，三路径、三版本和真值核对均
通过；样本生成的文档与真值在同一 seed 下逐字节稳定。

## 边界

- 这是可验收的会话编排与精度验证工程，不是 RTP、RTSP、WebRTC 或 H.264
  编解码实现。
- 镜像包保存覆盖区域、字节数、时序和稳定内容哈希，不生成真实像素载荷。
- 进程被强制终止后只承诺从已提交 SQLite 检查点继续，不承诺生成退出报告。
- schema 语义、枚举支持集和偏移位宽由本仓库三个版本定义描述，外部实现应
  按此显式规则读写。
