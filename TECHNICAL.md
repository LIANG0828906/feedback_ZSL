# 观战投屏会话内核技术说明

## 1. 模块与入口

- Go 模块：`obsproj`
- 最低版本：Go 1.22
- 第三方依赖：无
- 演示入口：`go run ./cmd/obsdemo -out artifacts/generated`
- 完整测试：`go test ./...`
- 并发竞态检查：`go test -race ./...`

主要包：

| 包 | 职责 |
| --- | --- |
| `internal/model` | 内部模型、整型换算、状态机、结构化错误、内容哈希 |
| `internal/schema` | v1/v2/v3 编解码、默认补齐、有损降级清单 |
| `internal/kernel` | 事件裁决、区域映射、裁剪、误差上界、打包、窗口提交 |
| `internal/checkpoint` | 纯 Go 嵌入式原子检查点、哈希链校验、有效前缀恢复 |
| `internal/runner` | 实时、恢复、全量重放三条路径与首差异比较 |
| `internal/scenario` | 固定种子样本、三版本产物、真值清单、有损样本 |
| `cmd/obsdemo` | 本地端到端复核入口 |

## 2. 内部统一模型

### 2.1 坐标

内部逻辑坐标固定为 `1/1000` 逻辑单位，即整数微坐标。v1/v3 的绝对逻辑坐标与 v2 的屏幕原点偏移都先映射到该单位。所有除法均向下取整（floor），余数单独用于损失或误差报告，不使用浮点。

### 2.2 时间

内部 tick 频率为：

```text
internal_rate = lcm(1000, declared_tick_rate)
```

因此毫秒和声明 tick 都能无损装入内部 tick；向外导出时若有余数，按 floor 取值并产生 `E_LOSSY` 说明。内部事件时刻、窗口时刻、误差上界均使用内部 tick。

### 2.3 枚举

- 缩放档内部值：`100/125/150/200`，表示 `1x/1.25x/1.5x/2x`
- 色度采样内部值：`420/422/444`
- 色度包预算：分别按 `2/3/4` 字节每像素的保守整数预算计算

## 3. 三版本 Schema

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 字段命名 | snake_case | camelCase | 紧凑短字段 |
| 区域坐标 | 绝对逻辑坐标 | 受控屏原点偏移 | 绝对逻辑坐标 |
| 时间基准 | 毫秒 | 声明 tick | 声明 tick |
| 缩放编码 | 字符串 `"1.5x"` | 整数 `150` | 序号 `2` |
| 色度编码 | 字符串 `"4:2:2"` | 整数 `2` | 序号 `1` |
| 最大包尺寸 | 无字段，装载补默认值 | 可选 | 必填 |

版本判别字段统一为 `schema_version`。未知版本、未知枚举和必填字段缺失返回 `E_SCHEMA`，并带版本与内部字段路径。非法 JSON 返回 `E_DECODE`。

v2 的帧偏移使用帧时刻生效的屏幕原点。编解码器会按与内核一致的事件优先级重建原点时间线，因此拓扑事件改变原点后仍可精确往返。

## 4. 升级与降级

### 4.1 升级补齐

v1 每个屏幕缺少最大包尺寸字段，装载时补 `32768`，并为每个屏幕记录：

```json
{
  "path": "screens.A.capability.max_packet_size",
  "old_value": "<missing>",
  "default_value": "32768",
  "source": "schema:v1.defaultMaxPacketSize"
}
```

v2 最大包尺寸缺失时也记录 `schema:v2.defaultMaxPacketSize` 来源。v3 该字段必填，缺失即 `E_SCHEMA`。

### 4.2 降级损失

v1 不表达 `1.25x` 与 `4:4:4`。导出 v1 时：

- 缩放档降为最近的较低受支持档位
- 色度降为 `4:2:2`
- 从能力列表移除无法表达的枚举
- 非默认最大包尺寸无法表达时列出默认值处置
- 坐标或时间有截断余数时逐字段列出 floor 处置

有损导出仍返回可被 v1 加载器读回的产物，同时返回 `E_LOSSY` 和逐项 `Loss` 清单。无损导出读回后语义哈希与源模型相同。

## 5. 区域映射

对屏幕 `S` 与源区域 `R`：

```text
relative = R.coord - S.origin
pixelEdge = floor(relative * scalePercent / (1000 * 100))
```

矩形左右、上下四条边分别取整，再求宽高，随后裁剪到 `[0, resolution)`。裁剪后为空、请求缩放不在能力列表、请求色度不在能力列表均返回 `E_PARA`，附带屏标识、请求档位与允许集合。

映射只使用整数乘除与显式溢出检查；同一输入的原始像素区域、裁剪边界与参数逐字段稳定。

### 5.1 四项误差与上界

每个屏幕逐窗口记录：

- 逻辑坐标换算残差
- 缩放取整偏差
- 裁剪截断误差
- 时间基准换算残差（内部 tick）

前三项和最终比较使用微像素；时间残差的上界按保守运动包络折算为微像素。上界由已观测的边余数、裁剪边数、分辨率、缩放分子分母、时间残差推导。实测映射偏差超过上界时返回 `E_BOUND`，带屏标识、实测值和上界。

## 6. 事件裁决与端集合

同一内部 tick、同一屏幕的事件按固定优先级：

```text
exit(40) > join(30) > topology(20)
```

同优先级按事件 ID 字典序升序决胜。每条裁决记录窗口、时刻、类别、胜出事件、被抑制事件和裁决依据。重复事件 ID 或无法唯一决胜返回 `E_CONFLICT`。

新加入屏幕在该窗口分发前回放最近已提交窗口的源区域，其包标记 `replay=true` 并记录 `source_window`。退出事件胜出后，该屏幕不会出现在当前窗口的受控屏提交集合中，不会再收到新包。

## 7. 打包与并发预算

每个屏幕按色度采样预算和最大包尺寸切分矩形。包记录：

- 屏幕内序号与窗口全局序号
- 覆盖像素区域
- 字节长度
- 确定性 payload 哈希
- 是否回放与来源窗口

映射工作线程池限制：

- 最大并行度
- 每线程像素缓冲容量
- 大区域按缓冲容量分块
- 工作线程全部占用时阻塞排队，不丢弃任务
- 记录排队原因与实际等待微秒

输出按屏幕 ID 重排，因此并行度或调度顺序不改变提交结果。

## 8. 状态机

会话：

```text
established -> negotiating -> streaming <-> paused -> ended
```

受控屏：

```text
pending -> negotiating -> committed -> negotiating
                         \-> failed
pending/negotiating ------> failed
```

帧窗口：

```text
pending -> distributing -> committed
       \-> failed        \-> failed
```

非法迁移返回 `E_STATE`，并保持原状态与进度。

## 9. 原子检查点

项目未引入 CGO 或第三方 SQLite 驱动，而采用题目允许的纯 Go 本地嵌入式检查点：

- 单文件 `session.ckpt`
- 每个窗口提交时构造完整新文档
- 同目录临时文件写入、`fsync`、原子 `rename`、目录 `fsync`
- 进程内互斥锁 + Linux 文件锁，避免写入交错
- 记录链包含前序哈希、记录哈希、会话语义哈希、算法版本
- 每窗口记录已提交区域、映射参数、端集合快照、拓扑版本、状态快照与内容哈希

加载时逐条校验记录序号、会话哈希、算法版本、提交内容哈希与记录链。首个不匹配窗口及其后记录全部丢弃，返回有效前缀和 `RecoveryReport`；恢复运行只从最后有效窗口的快照继续。

## 10. 三条执行路径

- 实时推进：逐窗口处理并逐窗口提交检查点
- 断点恢复：先提交前缀，关闭并重开检查点，装载最后一致快照后继续
- 全量重放：不依赖已有检查点，从会话起点一次执行

`runner.CompareResults` 按窗口、屏幕、字段顺序比较，返回首个差异的窗口号、屏标识与字段名，错误码为 `E_DIFF`。

## 11. 异常诊断覆盖

| 错误码 | 注入或触发方式 |
| --- | --- |
| `E_SCHEMA` | 未知版本、缺失必填字段、未知枚举 |
| `E_DECODE` | 会话 JSON 损坏、检查点文档损坏 |
| `E_OVF` | 坐标/时间整型换算溢出，带字段与两个操作数 |
| `E_LOSSY` | v1 导出不可表达缩放/色度，带损失清单 |
| `E_PARA` | 缩放能力不匹配或裁剪为空 |
| `E_CONFLICT` | 同刻事件重复 ID，无法裁决 |
| `E_BOUND` | 实测映射偏差超过推导上界 |
| `E_STATE` | 三层状态机非法迁移 |
| `E_ORDER` | 指令窗口时间倒置或同窗口时间不一致 |
| `E_CAP` | 拓扑与能力声明冲突 |
| `E_SEG` | 单窗口内部流水线失败 |
| `E_DIFF` | 路径结果首处字段不一致 |

## 12. 实际验证结果

环境：Linux/WSL Ubuntu，`go1.22.12 linux/amd64`。

```text
$ go test ./...
ok  obsproj/internal/kernel
ok  obsproj/internal/runner
ok  obsproj/internal/scenario
ok  obsproj/internal/schema

$ go test -race ./...
ok  obsproj/internal/kernel
ok  obsproj/internal/runner
ok  obsproj/internal/scenario
ok  obsproj/internal/schema

$ go vet ./...
(no output)
```

固定种子 `20261126` 的演示结果：

```text
windows: 8
decisions: 14
packets: 1627
semantic hash: 三个 schema 装载后完全一致
path agreement: true
lossy diagnostic: E_LOSSY，产物可读回
injected diagnostics: E_DECODE / E_ORDER / E_CAP / E_SEG
```

