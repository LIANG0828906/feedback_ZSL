# 观战投屏确定性分发内核技术说明

## 范围与运行

本交付只使用 Go 1.22 标准库，没有第三方 Go 依赖、Docker、真实设备或网络服务。

```bash
go test ./...
go run ./cmd/screenrelay verify
go run ./cmd/screenrelay sample --dir artifacts/samples
go vet ./...
```

`verify` 会生成三版本样本、运行三条推进路径、写入本地检查点，并复现全部异常链路；JSON 结果写到 `artifacts/reports/verification-report.json`。

## 内部统一模型

- 坐标：`1 logical px = 256 internal coordinate units`，常量为 `model.InternalCoordUnit`。
- 时间：`1 second = 90000 internal ticks`，毫秒倍率为 `90 tick/ms`。
- 全链路只使用 `int64` 与整数除法，不使用 `float32/float64` 累积。
- 所有乘法/加法/减法经 `xconv.MulChecked`、`AddChecked`、`SubChecked` 检查；溢出错误为 `E_OVF`，结构化字段包含 `field`、`op_a`、`op_b`。

## 三版本 Schema

| 版本 | 版本字段 | 命名风格 | 坐标表达 | 时间基准 | 缩放枚举 | 色度枚举 |
| --- | --- | --- | --- | --- | --- | --- |
| v1 | `ver: "1.0"` | `sid/panels/pid/wpx/hpx/zoom/color` | `anchor` 屏原点 + `dx/dy` 偏移 | `ms` | `1=100%`, `2=200%` | `0=4:2:0`, `1=4:4:4` |
| v2 | `schemaVersion: "2.3"` | camelCase | `absX/absY` 绝对逻辑坐标 | 60Hz tick，`1 tick=1500` 内部 tick | `x1/x1_25/x1_5/x2` | `420/422/444` |
| v3 | `schema_version: "3.0"` | snake_case | 嵌套 `region.x/y/w/h` | 90kHz `tick90k`，等同内部 tick | `"100"/"125"/"150"/"200"` | `"4:2:0"/"4:2:2"/"4:4:4"` |

加载入口是 `schema.Load`，返回 `Project` 与 `ConvertReport`。未知主版本或必填字段缺失返回 `E_SCHEMA`，错误的 `Path` 是 JSON 字段路径，例如 `$.panels[0].zoom_modes[1]`。

固定种子 `20261127` 的样本包含 6 个屏标识、7 个拓扑事件和 6 个窗口。初始活动屏为 `scr-a` 到 `scr-d`；`scr-e` 在第 3 窗加入并回放第 2 窗，`scr-f` 同时刻加入/退出时由退出事件胜出，`scr-c` 的两个同刻拓扑事件由更高 revision 胜出，并在第 6 窗退出。

## 升降级痕迹

- v1 缺少 `zoom` 时补 `1(100)`，缺少 `color` 时补 `0(4:2:0)`；`ConvertReport.Fills` 记录 `path`、`old_value=<missing>`、`default_value` 和规则来源。
- 导出 v1 时，v1 不能表达的 `125/150` 缩放档按“不高于原值的最高可表达档”归一下调到 `100`；`4:2:2` 下调到 `4:2:0`。
- 每个无法表达的当前值或能力项形成一条 `LossyItem`，错误码为 `E_LOSSY`，包含字段路径、原值、处置规则和替代值。
- 兼容子集（仅 `100/200` 与 `4:2:0/4:4:4`）导出 v1 不产生 lossy，再加载后拓扑、分辨率、原点和能力枚举一致。

## 坐标、时间往返

绝对坐标减屏原点得到偏移，偏移加屏原点恢复绝对坐标。样本坐标均为 256 对齐，事件时间均为 50ms 对齐，因此同时满足：

- v3 → v1 → v3 的窗口区域和事件 tick 完全相同。
- v3 → v2 → v3 的窗口区域和事件 tick 完全相同。

非对齐输入仍使用声明的舍入方向处理余数。`DivModFloor` 保证负坐标也得到 `0 <= remainder < denominator`。

## 映射与裁剪

对屏 `S` 和主控区域 `R`，先求本地坐标：

```text
local_x = R.x - S.origin.x
local_y = R.y - S.origin.y
local_right  = (R.x + R.w) - S.origin.x
local_bottom = (R.y + R.h) - S.origin.y
```

缩放档百分比为 `pct` 时，物理像素有理数分母为 `D = 256 * 100 = 25600`：

```text
left/top  = floor (local * pct / D)
right/bottom = ceil(local_edge * pct / D)
width  = right - left
height = bottom - top
```

随后裁剪：

```text
x1=max(0,x), y1=max(0,y)
x2=min(pixel_width, x+w), y2=min(pixel_height,y+h)
clip_w=max(0,x2-x1), clip_h=max(0,y2-y1)
```

请求缩放档不在 `scale_capabilities` 中返回 `E_PARA`，错误包含 `screen_id`、请求值、实际支持的缩放/色度能力和分辨率。裁剪后为空也返回 `E_PARA` 与未裁剪矩形。映射结果按屏 ID 排序，核心函数不遍历 Go map，因此与输入顺序无关。

## 误差上界

每份 `mapping.ErrorReport` 记录：

- `coord_remainder_units`：本地四边对 256 取模后的余数和；上界 `4*(256-1)=1020`。
- `scale_rounding_numerator`：四边 floor/ceil 到精确有理数的整数偏差和；每边小于 `D`，上界 `4*(D-1)`。
- `clip_truncation_pixels`：左、上、右、下被裁掉的像素长度和；上界直接由观测到的未裁剪矩形和屏分辨率按同一 max/min 规则推出。
- `time_remainder_ticks`：窗口 tick 对当前版本时间量子取模；v1 量子 90，v2 量子 1500，v3 量子 1，上界为量子值。
- `bounds.geometry_chain_numerator`：坐标量化误差经缩放倍率传播后的上界与缩放舍入上界之和。
- `bounds.max_packets`：`ceil(clip_w*clip_h / floor(max_packet_bytes/4))`，只由观测像素区域和包尺寸推导。

所有实测残差在引擎和测试中都会再次断言不越界。

## 拓扑裁决与回放

同一 `(tick, screen_id)` 的事件成组裁决，总优先级为：

```text
leave(安全退出) > join(成员建立) > topology(已存在成员的配置变更)
```

同类事件先比较 revision（高者胜）；同 revision 且 canonical payload 不同则无法裁决，返回 `E_CONFLICT`；同 revision 且 payload 相同按事件 ID 字典序做幂等去重。每条 `topology.Decision` 记录 tick、屏、胜出事件、胜出类别/revision、抑制事件及依据。

新加入屏不查找不存在的旧分发记录，而是用加入声明中的能力，把最近一个已提交窗口的主控区域重新映射/切包为 replay bundle；因此它立即得到最新已提交画面。退出事件生效后，该屏从活动集合删除，不再产生映射或镜像包。

## 状态机

- 会话：`establishing -> negotiating -> casting -> paused/ended`，`paused -> casting`。
- 受控屏：`pending_negotiation -> negotiating -> committed/failed`，允许 `committed -> negotiating` 处理拓扑 revision，`failed -> negotiating` 重试。
- 帧窗口：`pending_dispatch -> distributing -> committed/failed`，失败可重新分发，已提交窗口不可再迁移。

非法迁移返回 `E_STATE`，方法只在校验通过后修改当前状态，因此原状态保持不变。

## 包切分与摘要

`dispatch.SplitPacketsBudgeted` 按 `max_packet_bytes / 4` 得到每包最大像素数，再受工作线程像素缓冲上限约束；使用确定性 tile 顺序生成包序号和覆盖像素矩形。每个包有 SHA-256 摘要，每屏有包含矩形、种子和包摘要列表的 `content_hash`。

## 受限并行与异常隔离

`engine.RunScreenPool` 将屏按 ID 排序，按 `MaxParallel` 分成确定性 wave。每 wave 内 goroutine 并行映射/裁剪/切包，最大并行度不超过配置；每个 worker 持有 `BufferPixelsPerThread*4` 字节缓冲，tile 像素数不得超过该上限。后续 wave 记录 `queued=true` 和阻塞/排队原因。单个屏返回 `E_PARA` 或 panic 时转成该 task 的错误，不影响同 wave 其他屏；引擎收集全部 outcome 后才裁决窗口失败。

## 检查点

Go 标准库没有内置 SQLite 驱动，而题目禁止第三方包，因此 `internal/litedb` 注册名为 `sqlitefile` 的 `database/sql/driver` 本地文件实现。它支持本项目使用的受限 SQL 子集、事务、`INSERT OR REPLACE` 和 `ORDER BY`，提交时执行：

1. 写临时文件；
2. `file.Sync`；
3. 原子 `rename`；
4. 同步目录项。

`checkpoint.Store` 设置 `SetMaxOpenConns(1)`。每个窗口在单个事务中写窗口序号、tick、源版本、算法版本、内容哈希、前一窗口哈希和完整 JSON payload；payload 含已提交区域、映射参数、活动屏快照、包摘要、误差报告和端集合演进。另有 meta 表更新最新序号。

重启时按窗口序号升序验证：

- payload JSON 的 SHA-256 是否等于 `content_hash`；
- `algorithm_version` 是否匹配 `mapping-v1.0.0`；
- `source_version` 是否匹配当前工程；
- `prev_hash` 是否衔接前一条有效记录。

任一窗口不匹配，则该窗口及其后哈希链不作为恢复前缀；`LatestValidVersions` 返回最后一个完整有效窗口和失效序号/原因。

## 三路径一致性

- 全量重放：无检查点，从第 1 窗运行到第 6 窗。
- 实时/检查点推进：按逻辑 tick 顺序运行，每窗提交本地文件检查点。
- 断点恢复：先以 `StopAfter=2` 模拟中断，关闭并重新打开存储，再由 `Resume` 只装载有效已提交前缀并继续。

`engine.CompareResults` 按结构体/JSON tag 路径递归比较；不一致时返回第一个路径，例如 `$.windows[2].screen_results[0].mapping.pixel_region.x`。验收测试断言三条路径完全一致。

## 错误码

| 码 | 触发点 |
| --- | --- |
| `E_SCHEMA` | 未知版本、必填字段缺失、字段类型错误 |
| `E_LOSSY` | 低版本无法表达的枚举或精度 |
| `E_OVF` | 整数运算溢出 |
| `E_PARA` | 缩放超能力、裁剪区域为空、切包参数非法 |
| `E_CONFLICT` | 同 tick/屏/revision 但 payload 冲突且无裁决规则 |
| `E_STATE` | 状态机非法迁移 |
| `E_DECODE` | JSON 工程或检查点文件/payload 损坏 |
| `E_ORDER` | 窗口或事件时刻/序号倒置 |
| `E_CAP` | 屏能力声明自相矛盾 |
| `E_BUFFER` | 像素缓冲预算小于一个像素等预算错误 |
