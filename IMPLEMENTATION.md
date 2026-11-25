# 多分支总线响度包络内核

本目录是一个可集成的 Go 1.22+ 实现，使用纯 Go SQLite 驱动 `modernc.org/sqlite v1.29.10`。
依赖已写入 `go.mod` / `go.sum`，并附带 `vendor/`，因此本机或 Windows 上可离线运行。

## 快速运行

```bash
go test ./... -count=1
go run ./cmd/audioctl verify
go run ./cmd/audioctl review -db artifacts/db/resume.db -truth artifacts/sample/truth.json
go run ./cmd/audioctl faults
go run ./cmd/audioctl gen -out artifacts/generated -seed 20261125
```

Windows CMD：

```bat
set CGO_ENABLED=0
go test .\... -count=1
go run .\cmd\audioctl verify
```

`verify` 会完成：

- 加载 `artifacts/sample` 的 manifest 与 `alpha`、`beta`、`gamma` 三分支；
- 校验三分支映射后的通道、采样偏移、持续数、增益、优先级和条件标志完全等价；
- 合成 3×4800 个定点帧并逐点比对 `truth.json`；
- 生成 36 个设备包并比对边界、补齐方式、时序标记和 SHA-256；
- 运行实时、窗口 1 后断点恢复、SQLite 持久化重放三条路径并比对；
- 用独立 review 包复核新数据库和随样本提供的 `artifacts/db/resume.db`；
- 演示事件 6 的精确失效清单。

## 三分支语义

| 分支 | 时间 `ts` | `ld` | `tg` | `lst` | 通道 |
|---|---|---|---|---|---|
| `alpha` | 内部采样点 | 持续采样数 | 增益 | 包络模板/事件类型序列 | `ch` |
| `beta` | 帧，1 帧=48 采样 | 增益 | 持续采样数 | 通道路由序列 | `lst[0]` |
| `gamma` | 毫秒，48kHz 下 1ms=48 采样 | 帧数持续值 | 增益 | 包络模板/事件类型序列 | `ch` |

帧长由 `sampleRate / 1000 = 48` 的 1ms 帧基准得到；毫秒转换为
`floor(ms * sampleRate / 1000)`。所有乘法先做 `int64` 溢出检查，溢出返回
`E_OVF`，错误中保留来源分支、字段路径、事件 ID 和原始值。样本中的偏移和持续数
均为 48 的整数倍，因此采样点 → 帧 → 采样点往返无损。

条件事件由 `cd=1` 标记，`fl & manifest.flagMask == flagMask` 时为真；
`cd=0` 的普通事件不受 `fl=0` 影响。

## 合成与切包

通道内先按 `(start asc, priority desc, arrival asc, id asc)` 稳定排序，再执行：

1. 条件为假的条件事件丢弃；
2. 与 `ni=1` 的禁止打断事件重叠的新事件丢弃；
3. 新事件优先级不高于已存在事件时丢弃；
4. 更高优先级事件把旧事件的有效持续区间截断到新事件起点；
5. 有效事件按通道模板渲染，原始整型振幅先叠加，再统一量化。

模板系数在本样本中使用 1024 定点分母：`attack = duration * attackPermille / 1024`。
斜坡振幅用整数比例四舍五入构造，移动段量化采用最近值且平局向下，持续峰值采用
最近值且平局向上。该规则与给定 `truth.json` 的全部 14400 个通道采样点一致。

每个逻辑窗口为 1600 采样，按 `maxSamplesPerPack=480` 切成
`480/480/480/160` 四包；末包按 `tailRule=pad` 补齐到 480 个 `int64`。
包哈希是补齐后小端 `int64` 字节流的 SHA-256，因此与真值中的包哈希一致。
时序标记为 `floor(startSample * 1000 / sampleRate)` 的毫秒值。

## 依赖图与缓存

每个包记录：

- 与包区间相交的全部源事件 ID（包括当前被优先级压制的候选事件，保证编辑优先级后可追踪）；
- 整组通道模板哈希；
- `quantUnit`；
- 前包占位偏移 `seq * maxSamplesPerPack`；
- 内容哈希、分支版本和算法版本。

`internal/graph` 对事件编辑找到该事件首次影响的包，然后只失效该包及其后同通道包；
之前的包和其他通道保留在缓存。调整 `maxSamplesPerPack` 会改变所有边界，因此全量失效；
调整 `quantUnit` 时静默包可保留，首个非零变化点及其后同通道包失效。

## 状态机与 SQLite

包和通道窗口共享五态：

```text
pending_quantization -> quantizing -> sliced -> committed
                              \           \--> failed
                               \-------------> failed
failed -> pending_quantization（显式重试）
```

非法迁移返回 `E_STATE`，状态保持不变。

`internal/store` 使用单连接和互斥提交锁；一个逻辑窗口内所有通道、所有包在同一个
SQLite 事务中写入。表包含定点帧 BLOB、边界、disposition、mark、事件依赖 JSON、
模板哈希、quant unit、消费事件偏移、内容哈希、分支版本和算法版本。重启时校验
meta 版本和每个包的 SHA-256；任一不匹配即清空快照并由引擎全量重放。

## 三路径与独立复核

- 实时路径：`Engine.RunRealtime` 并行合成通道、串行提交窗口。
- 断点恢复：先提交窗口 1 后停止，再由 `pipeline.Resume` 打开 SQLite、载入已提交包并继续。
- 持久化重放：`ReplayFromStore` 只读取 SQLite 包 BLOB 和边界重建帧，不调用合成器。

`pipeline.CompareReports` 比较逐采样帧、包边界和哈希；首个差异返回 `E_DIFF`，
带通道、采样偏移和包序号。

`internal/review` 是独立复核工具，只依赖 SQLite、标准库和 `truth.json`，
不导入 `internal/synth` 或 `internal/pipeline`。它直接核对持久化 BLOB 的逐点值、
包采样数、补齐包哈希和通道指纹。

## 并发与异常

`ResourcePool` 同时限制 worker 数和每任务采样缓冲；缓冲不足时调用方阻塞排队，
并记录 `buffer_exhausted`、通道和等待纳秒。SQLite `SetMaxOpenConns(1)` 加应用互斥锁，
保证提交串行。

异常链：

| 注入 | 错误码 | 定位 |
|---|---|---|
| 损坏 JSON / 无法读取样本 | `E_DECODE` | `path` |
| 同通道源到达时序倒置、越界 | `E_ORDER` | `channel` / `frame` / `eventId` |
| 条件标志位矛盾 | `E_FLAG` | `channel` / `frame` / `eventId` / `path` |
| 注入某包阶段失败 | `E_STAGE` | `channel` / `window` / `packet` |
| 包长度或时序窗口越限 | `E_PACK` | `channel` / `window` / `packet` |
| 非法状态迁移 | `E_STATE` | `channel` / `window` / `packet` |
| 整数换算溢出 | `E_OVF` | `branch` / `path` / `eventId` / `rawValue` |

## 实际验证

完整输出保存在：

- `artifacts/verification/test-output.txt`
- `artifacts/verification/verify.json`
- `artifacts/verification/faults.json`

最近一次结果：`go test ./... -count=1` 全部通过，`go vet ./...` 无输出，
`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/audioctl` 通过。
`verify` 报告 14400 个真值帧、36 个包、三路径相等，新 SQLite 与随样本
`artifacts/db/resume.db` 的独立复核均为 36 包 / 3 通道通过。
