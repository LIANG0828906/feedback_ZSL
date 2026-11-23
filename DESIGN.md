# captionx 技术说明

面向游戏过场与互动媒体无障碍呈现的字幕排版领域组件。纯 Go 标准库实现，
无第三方依赖、无网络访问、无容器环境。全链路时间单位为**整型采样点数**。

## 构建与验证

```sh
go build ./...          # 编译
go test ./...           # 单元与集成测试
go run ./cmd/gensamples -out samples -count 3   # 确定性样本生成
go run ./cmd/verify -samples samples -work verify-work  # 三路径一致性验收
go run ./cmd/demo -samples samples              # 桌面示例（浏览器窗口）
```

## 组件结构

| 包 | 职责 |
| --- | --- |
| `internal/errs` | 稳定错误码（E_SCHEMA/E_PARA/E_SCHED/E_DECIDE/E_BOUND/E_STATE/E_DIFF/E_IO），携带台词编号、字段名、字符序号、误差项名与实测值 |
| `internal/model` | 清单 schema 与计算结果结构；所有时间量为整型采样点 |
| `internal/samplegen` | 确定性样本生成器（自带 xorshift64* PRNG，固定种子）；真值仅由生成器解析构造 |
| `internal/loader` | 工程加载与结构校验（E_SCHEMA/E_PARA），内容哈希 |
| `internal/envelope` | 逐帧能量、语音段边界检测；边界只在帧移整数倍取值，逐条记录量化误差 |
| `internal/window` | 显示窗口推导（顺序 Deriver）与最短可读时长约束、确定性取舍决策 |
| `internal/schedule` | 字符级高亮排程：整型有理运算 floor 累积规则，确定性余数分配 |
| `internal/report` | 误差传播报告装配与累积上界校验（E_BOUND） |
| `internal/segment` | 分段状态机（pending/processing/committed/failed，E_STATE） |
| `internal/checkpoint` | 原子（临时文件+rename）且串行（互斥锁）的检查点持久化；哈希/版本不匹配整体作废 |
| `internal/pipeline` | 三条处理路径（实时分块、断点恢复、全量重算）与一致性比对（E_DIFF/E_DECIDE） |
| `internal/api` | 纯函数式查询接口：取台词窗口、取字符高亮区间、取误差报告 |
| `cmd/gensamples` / `cmd/verify` / `cmd/demo` | 样本生成、验收器、桌面示例 |

## 关键设计

### 样本与真值
生成器先规划台词（文本、逐字权重、可选显式窗口），再按语速解析公式
`desiredLen = ceil(nChars * sampleRate * rateDen / rateNum)` 反推语音段长度，
保证清单参数下不需要最短时长取舍。真值窗口与逐字高亮起始时刻由生成器用
与流水线相同的整型规则在**真实段边界**上解析构造；流水线只能观测检测边界，
二者之差即为可测量的推导偏差。

### 边界检测与量化误差（需求 2）
逐帧能量（帧长/帧移可配，越界 E_PARA 并给出合法范围）定位候选帧后，
在帧内做采样级阈值细化得到 raw 边界，再量化到最近的帧移整数倍。
每条边界记录 `{raw, quantized, quantError}`，`quantError <= hop/2`。

### 窗口推导与最短可读时长（需求 2、4）
无显式窗口的台词在所属（量化）语音段内按语速顺序排布；显式窗口原样采用。
窗口短于 `minLen = ceil(nChars * sampleRate * maxDen / maxNum)` 时：
先向左（前一句之后的空隙）再向右（本段之后及段内尾部空隙）延长，
延长量不超过可回收空隙；仍不足时按确定优先级取舍——整段窗口满足最短时长
则回退整段，否则压缩字符停留。每次取舍记录台词编号、手段与理由；
决策序列由 `pipeline.Compare` 在任意两条执行路径间比对，不一致返回 E_DECIDE。

### 字符排程（需求 3）
字符 i 的高亮起点为 `window.Start + floor(L * cumW_i / W)`（W 为权重分母）。
floor 累积规则即确定性余数分配：不可整除的余数由靠后字符吸收，
区间首尾相接、总时长等于窗口、无重叠无空洞。权重非正（分母贡献为零）
或窗口短于字符数返回 E_SCHED 并指出台词编号与字符序号。

### 误差上界（需求 5）
上界仅由已观测参数推导，禁止从真值反推：

| 误差项 | 逐句上界 | 推导依据 |
| --- | --- | --- |
| 包络边界量化误差 | `hop` | 两条边界各自量化误差 ≤ hop/2 |
| 窗口推导累积偏差 | `min(hop, tolerance)` | 窗口端点只随量化边界移动 |
| 字符排程取整残差 | `nChars` | 每字符残差分子 < 权重分母 W，和 < nChars 采样点 |
| 时长偏差 | `min(hop, tolerance)` | `|Δend−Δstart| ≤ |Δend|+|Δstart|` |

四项沿台词序前缀累积为累积上界；整工程总朗读时长上界为逐句时长上界之和。
实测超出上界返回 E_BOUND（台词编号、误差项名、实测值）。
真值只对清单自身参数有效，因此参数覆盖重算时仍记录误差项但不做真值强制校验。

### 分段状态机与检查点（需求 6、7）
工程按 `segmentChunkSamples` 固定时长切分；台词归属其窗口起点所在分段，
跨段台词只被起始分段排程一次。合法迁移：pending→processing→committed/failed、
failed→processing；非法迁移 E_STATE 且状态不变。每提交一个分段，将该段台词窗口、
字符排程、误差统计连同内容哈希、时间单位参数版本（tu-v1）与算法版本（algo-v1）
原子串行落盘。重启加载时哈希或任一版本不匹配即整体作废快照并全量重算，
禁止部分混用。用户取消或进程被强制终止后，只能依据已持久化且版本匹配的分段
恢复；本组件不承诺强制终止后仍生成完整报告。
已提交分段被冻结，后续调整语速/权重只影响未提交分段。

### 三路径一致（需求 8）
实时分块（带检查点、可中断）、断点恢复（仅重放已提交记录）与一次性全量重算
共享同一顺序 Deriver 与排程规则；`pipeline.Compare` 逐句比对窗口、逐字符比对
高亮起止、比对误差统计与决策序列，定位首个不一致的台词编号、字符序号与字段名，
不一致返回 E_DIFF（决策序列不一致返回 E_DECIDE）。

## 实际验证结果

- `go build ./...`、`go vet ./...`：通过。
- `go test ./...`：9 个包全部通过，覆盖 E_SCHEMA/E_PARA/E_SCHED/E_BOUND/
  E_STATE/E_DIFF/E_DECIDE 异常码链路、检查点作废、冻结语义与三路径一致性。
- `cmd/verify`：种子 20261124 生成 3 条工程、种子 777 生成 12 条、种子 1 生成
  8 条，共 23 条工程全部通过三路径一致性比对；窗口推导偏差均在声明容差
  （1 个帧移）以内，总朗读时长偏差为 0（上界 = 台词数 × 帧移）。
- `cmd/demo`：分段提交、运行到完成、参数重算（已提交分段保持冻结）、
  作废检查点各端点冒烟通过。
