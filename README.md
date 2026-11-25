# vpcompose：虚拟制片层栈合成迭代核

纯 Go 实现的本地确定性虚拟制片合成组件。它把同一份前景工程在三个格式版本下
统一加载成有序层栈，执行整数时间换算、预乘 alpha 归一、线性化合成、窗口状态
推进、精确失效、SQLite 原子检查点和三路径一致性校验。

不依赖 Docker、外部数据库、图形设备或网络账号。SQLite 使用纯 Go 驱动
`modernc.org/sqlite v1.35.0`。

## 快速开始

环境要求：Go 1.21 或以上。当前实测为 `go1.22.12 linux/amd64`。

```bash
go mod download
go test ./... -count=1

# 一次跑完加载、真值、往返、失效、恢复和三路径验收
go run ./cmd/vpcompose verify --seed 42

# 保留确定性样本和 SQLite 检查点
go run ./cmd/vpcompose demo --seed 42 --work artifacts/demo

# 仅生成三版本工程、真值清单与 sidecar
go run ./cmd/vpcompose generate --seed 42 --out artifacts/samples
```

`generate` 产出：

```text
artifacts/samples/project-v1.json
artifacts/samples/project-v2.json
artifacts/samples/project-v3.json
artifacts/samples/truth.json
```

三份工程来自同一个固定种子、同一套图层内容与同一批解析真值。实际帧摘要均为
`55f6085b91f1...`（seed 42，第 0 帧）。

## 代码结构

```text
.
├── codec / loader / export     # 三版本加载与升降级
├── color.go / compose.go       # 16 位线性预乘合成核心
├── convert.go                  # tick/ms、预乘通道整型换算
├── window.go                   # 四态窗口、快照、依赖与缓存键
├── engine.go                   # 调度、失效、提交、恢复、重试
├── checkpoint.go               # SQLite 原子检查点
├── compare.go                  # E_DIFF 三路径定位
├── sample/                     # 固定种子样本器与独立解析真值
├── cmd/vpcompose/              # generate / verify / demo
└── docs/formats.md             # 三版本字段与转换规则
```

领域包可直接导入：

```go
import "vpcompose"

project, err := vpcompose.LoadProject(data)
err = vpcompose.VerifyTruth(project)
frame, err := vpcompose.ComposeFrame(project, 0)
```

## 版本与内部模型

| 版本 | 字段命名 | 层级 | 可见性终点 | alpha | 时间 |
| --- | --- | --- | --- | --- | --- |
| v1 | snake_case | 平坦层表 | `visible_until_ms` | straight | ms |
| v2 | camelCase | 平坦 nodes + parentId | `visibleUntilTick` | premultiplied | tick |
| v3 | kebab-case | 嵌套 tree/children | `until-tick` | premultiplied | tick |

内部统一为 4 通道、16 位线性、预乘 RGBA，以及 `90000 tick/s` 的整数时间基。
图层顺序固定为从底到顶。加载后保留 `SourceVersion`、`SourceFormat` 和逐字段
`ConversionTraces`。

详细字段样例见 `docs/formats.md`。

## 关键行为

**加载与转换**

- 未知版本、必填字段缺失返回 `E_SCHEMA`，`EngineError.Path` 精确到字段。
- 组层级成环返回 `E_CYCLE`，`LayerIDs` 给出参与环节点。
- v1 缺失 alpha 约定或组信息时按声明默认值补齐，并记录旧值、新值和默认来源。
- v3 -> v2 可无损读回；v3 -> v1 会生成 `E_LOSSY` 损失清单，同时仍返回可加载
  的产物，处置为 `flatten-preorder` 与 `convert-premultiplied-to-straight`。

**整数换算**

- `MillisecondsToTicks` / `TicksToMilliseconds` 支持六种显式舍入方向。
- 预乘/反预乘仅使用整数乘除和半入舍入。
- 复用溢出返回 `E_OVF`，包含路径和两个操作数。

**合成**

- 使用 sRGB 到线性 16 位 LUT、W3C alpha 合成方程和整数舍入。
- 支持 `normal`、`add`、`multiply`、`screen`。
- 帧尺寸、通道数、时间基完全来自内部工程模型。
- 输出为逐像素稳定的线性预乘 RGBA；同输入无随机源、无墙钟依赖。
- 与样本真值逐采样点精确比对，超差返回 `E_PIX`，带坐标、实测值、期望值和偏差。

**窗口与失效**

- 状态：`pending -> compositing -> committed|failed`，`failed -> compositing` 允许重试；
  其他迁移返回 `E_STATE`，原状态不变。
- 提交时冻结层参数和源版本快照；后续编辑只进入未提交窗口。
- 缓存键按图层顺序串联，每级包含上游键、层 ID、内容哈希和参数版本。
- 编辑 opacity/visibility/end/content/blend/timebase 时按真实参与帧计算失效范围；
  报告窗口编号、层 ID、原因与下游阶段。

**SQLite 检查点**

- 每个提交窗口在一个事务内写入摘要、参数快照、内容哈希、源版本、参数版本、
  算法版本、整体版本和结果像素。
- 单连接加互斥锁保证提交串行、写入不交错；查询按窗口号稳定排序。
- 整体版本、行元数据、行摘要或内容哈希任一不匹配，整库快照原子作废。
- 强制终止后只恢复已提交窗口，再从首个未提交窗口继续。

**三路径与并发**

- `realtime`、`recover`、`full` 产出相同像素、采样统计与提交序列。
- `CompareRuns` 自动定位首个不一致窗口和字段，返回 `E_DIFF`。
- 工作线程池限制并行度与单工作线程像素缓冲；排队会阻塞并记录原因与等待时长。
- 单窗口故障只会把该窗口置为 `failed`，其他窗口照常提交，可单独重试。
- 样本 JSON 损坏返回 `E_DECODE`。

## 实测结果

执行：

```bash
go test ./... -count=1
go run ./cmd/vpcompose verify --seed 42
```

结果：

```text
?   	vpcompose/cmd/vpcompose	[no test files]
?   	vpcompose/sample	[no test files]
ok  	vpcompose	0.488s
SAMPLE seed=42 files=4
LOAD v1 source=v1 layers=4 nodes=4 truth=exact frameDigest=55f6085b91f1
LOAD v2 source=v2 layers=4 nodes=8 truth=exact frameDigest=55f6085b91f1
LOAD v3 source=v3 layers=4 nodes=8 truth=exact frameDigest=55f6085b91f1
ROUNDTRIP v3->v2->v3 layers=4 alpha=premultiplied exact=true
LOSSY v3->v1 losses=5 dispositions=[convert-premultiplied-to-straight flatten-preorder]
INVALIDATE windows=[1] layer=beauty-glow committed-before=1/3 committed-after=3/3
RECOVERY committed-before=[0] total=3/3
THREE-PATH PASS windows=[0 1 2] sampleStats=exact pixels=exact
```

测试覆盖三版本装载、独立解析真值、`E_SCHEMA/E_CYCLE/E_DECODE`、整数换算与
`E_OVF`、无损/有损往返、`E_PIX`、非法状态迁移、精确失效、SQLite 版本失效与
行内容篡改、强制终止恢复、并发排队、预算失败隔离、重试，以及三路径 `E_DIFF`
定位。`go vet ./...` 与 `go test -race ./... -count=1` 也均通过。

## 设计说明

- 真值时间戳统一用内部 tick 表达，便于三版本直接比较。
- v1 无法表达分组和预乘约定；转换会显式报告损失，而不是静默丢弃。
- 运行中编辑被拒绝并返回 `E_STATE`；编辑应发生在一次窗口推进完成后。
- 整体版本绑定工程结构、帧表、源版本与算法版本；每行的内容哈希和参数版本对
  自身冻结快照校验。提交后的窗口即使后续编辑发生，也能在重启时恢复其冻结
  快照；行内容或结构版本被篡改时则整库作废。

