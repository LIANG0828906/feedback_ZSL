# DialogueCal 验证与技术说明

## 范围

DialogueCal 是一个无外部服务依赖的 Go 领域组件，用于：

- 装载 `dialogue.flat.v1`、`dialogue.tree.v2`、`dialogue.sample.v3` 三种 schema；
- 在同一条确定性 PCM 对白上切帧并提取 RMS、零交叉率、自相关 F0 候选；
- 在 Q15/Q8 定点整数域执行片段增益、能量限幅、响度换算、平滑修正和状态迁移；
- 输出增量失效来源、传播闭包、并行耗时与背压码；
- 使用纯 Go SQLite 或原子文件检查点恢复逐帧结果；
- 可集成领域包为 `dialoguecal/core`，根目录 `main.go` 与 `go run .` 提供本地可重复演示和异常注入。

固定种子 `42` 的样本为 16 kHz、单声道、25,600 samples、4 个片段、16 个音素。
帧参数为 50 FPS、320 samples 帧移和 320 samples 窗长，共 80 帧。`ph-03`
从 sample 4800 延伸到 6720，显式跨过 `seg-0/seg-1` 的 6400 sample 边界。

## 三版 Schema 归一

| 版本 | 字段风格 | 时间 | 增益 | 目标响度 | 音素组织 |
| --- | --- | --- | --- | --- | --- |
| `dialogue.flat.v1` | snake_case | ms，片段内绝对时间 | 线性千分比 | dB×100 | 扁平 `phonemes_flat` |
| `dialogue.tree.v2` | camelCase | tick，嵌套相对偏移 | 线性万分比 | 线性幅度万分比 | 音节树 `syllablesTree` |
| `dialogue.sample.v3` | camelCase | 绝对采样点 | dB×100000 | dB×100 | 扁平采样点表，含可选水位 |

所有版本先映射到 `Project`、`Segment`、`Phoneme`，再由 `NormalizeProject`
统一推导 Q15 增益/目标能量、Q8 dB、音素起始片段和覆盖校验。测试用
`reflect.DeepEqual` 断言三个装载结果完全一致，并比较归一后的帧序列。

语义无法消歧时返回 `E_SCHEMA_CONFLICT`；无法识别版本返回
`E_SCHEMA_UNKNOWN`。混合片段同时给出 dB 与线性目标且二者冲突时，可选
`calibration_watermark_db_x100` 能匹配其中一种表达才允许消歧。

## 定点口径与容差

- 容器：有符号 32 位整数；Q15 表示线性幅度/能量，Q8 表示 dB。
- 乘法中间值使用 int64，收窄前显式检查 int32 溢出并返回饱和标记。
- dB/线性换算使用整数 log2 归一化、Q31 Taylor/atanh 级数和范围归约，不依赖运行时浮点。
- 声明真值容差：能量 `6 Q15`、ZCR `2 Q15`、F0 `5×256 Q8 = 5 Hz`、
  响度/修正量 `12 Q8`；校准状态必须逐帧相同。
- 独立真值在 `truth.go` 中使用 float64 参考公式生成，再与整数内核逐项比较。

## 校准状态

每帧依次执行：

1. `gained = rms × segment_gain`；
2. 若 `gained > energy_cap`，能量钳到 cap，并写入包含片段、全局帧号、原始值和 cap 的 `LimitSource`；
3. Q15→Q8 dB；
4. 按帧中心音素的响度权重计算有效目标；
5. 定点一阶平滑：`smooth = floor_average(previous_smooth, desired_correction)`；
6. 根据电平和容差推进 `UNCALIBRATED → RECALIBRATE → CALIBRATED`。

每个片段第一帧为 `UNCALIBRATED`；电平均在容差内且连续 `SettleFrames=2`
帧后进入 `CALIBRATED`；电平跳变或残差越界进入 `RECALIBRATE`。静音帧保持
上一修正量和状态，同时更新响度基准，因此检查点可由最后一帧完整重建 carry。

## 增量闭包

- 增益/目标响度：直接失效范围为整个片段；因状态逐帧传导，闭包也是整个片段。
- 音素编辑：取旧/新区间并集映射到重叠帧；对每个涉及片段，从首个受影响帧传播到片段尾。
- 跨片段音素：闭包包含两个片段的尾区间。种子样本中的 `ph-03` 会产生
  `seg-0:[15,20)` 与 `seg-1:[0,20)`，共重算 25 帧、复用 55 帧。
- 帧率/帧移/窗长：所有帧坐标均改变，闭包为全部帧。
- 每个重算帧的 `Provenance` 记录 revision、closure ID 和编辑来源；复用帧标记
  `cache_state=reused` 与原 revision。测试断言增量结果与新工程一次性全量结果
  在所有数值和状态字段上等价。

## 并发与背压

`Budget{Workers, FramesPerWorker}` 声明线程预算和每线程缓冲上限，总缓冲为
`Workers × FramesPerWorker`。非法 worker 数返回 `E_BUDGET_WORKERS`，非法缓冲
返回 `E_BUDGET_BUFFER`；`FrameGate.TryAcquire` 在缓冲耗尽时返回确定的
`E_BACKPRESSURE` 和容量/在途上下文，不扩容、不丢帧。

片段由有界 worker 池并行计算，结果写入按全局帧号索引的切片，因此调度顺序和
并发度不影响结果。报告记录每片段 `time.Duration` 观测值。`go test -race`
覆盖当前实现。

## 检查点顺序

文件存储在 `FileCheckpointStore.CommitFrame` 中严格按以下顺序提交：

1. 原子写 `frames/frame-XXXXXXXX.json`；
2. 原子写 `segments/<segment>.json`；
3. 最后原子写 `meta.json` 全局序号和工程版本。

SQLite 实现在一个事务中按相同顺序执行 frames upsert、segment progress
upsert，最后更新 `project_version` 和 `last_frame_seq`。进程在事务提交前被
强杀时，SQLite 回滚；文件实现中旧 `meta.json` 仍指向最后完整提交点，孤立的
新帧文件会被忽略。

恢复只接受 `ContentHash(Project)` 匹配的检查点。版本不匹配返回
`E_CHECKPOINT_VERSION`，随后清除旧全局游标并从第 0 帧整段重算，不部分复用。
截断的 `meta.json` 返回 `E_CHECKPOINT_TRUNCATED`，其他损坏返回
`E_CHECKPOINT_CORRUPT`。`context` 取消或 SIGINT/SIGTERM 时，运行循环不会再
开始下一帧，磁盘上保留最后一个完整提交点。

## 异常码

演示和测试覆盖：

- `E_SCHEMA_UNKNOWN`
- `E_SCHEMA_CONFLICT`
- `E_GAIN_NEGATIVE`
- `E_TARGET_LOUDNESS_RANGE`
- `E_FRAME_RATE_ZERO` / `E_FRAME_RATE_RANGE`
- `E_PHONEME_ORDER` / `E_PHONEME_GAP` / `E_PHONEME_OVERLAP`
- `E_FRAME_BOUNDS`
- `E_BUDGET_WORKERS` / `E_BUDGET_BUFFER` / `E_BACKPRESSURE`
- `E_FIXED_OVERFLOW`
- `E_SNAPSHOT_VERSION`
- `E_CHECKPOINT_CORRUPT` / `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_VERSION`

## 实际验证结果

下列命令已于 Linux/WSL Ubuntu、`go version go1.22.12 linux/amd64` 执行通过：

```text
$ go version
go version go1.22.12 linux/amd64

$ go test -count=1 ./...
?       dialoguecal     [no test files]
ok      dialoguecal/core     0.159s

$ go test -count=1 -race ./...
?       dialoguecal     [no test files]
ok      dialoguecal/core     3.744s

$ GOPROXY=off go test -count=1 ./...
?       dialoguecal     [no test files]
ok      dialoguecal/core     0.159s

$ go vet ./...
# 无输出，退出码 0

$ go run . --sqlite=false
# cross_schema=OK parallel=true incremental=true truth_failures=0
# checkpoint ... recovered=true matches_full=true
# 11/11 injection checks [OK]
# DEMO_EXIT=0
```

完整命令记录见 `artifacts/validation-commands.log`，文本演示输出见
`artifacts/demo-output.txt`。

常用命令：

```bash
go test ./...
go test -race ./...
go run .                 # 默认纯 Go SQLite 检查点
go run . --sqlite=false  # 使用原子本地文件检查点
go run . --json          # 完整机器可读结果、闭包和容差标注
go run . --limit=20      # 缩短文本帧表
```
