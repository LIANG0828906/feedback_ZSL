# 逐采样点色带溯源管线

本交付是零第三方 Go 模块依赖的领域组件与本地诊断程序。模块名是 `colortrace`，
唯一的非 Go 依赖是本机运行库 `libsqlite3.so.0`；`sqlitecheckpoint` 通过 cgo
直接调用 SQLite C API，因此不需要 `go get`，也不需要 Docker 或网络服务。

## 目录

- `types.go`、`errors.go`：工程、级配置、台账、真值、错误码等领域类型。
- `sample/generate.go`：固定种子样本、源平面、目标贴图、两级配置和真值生成。
- `validate.go`、`decode.go`：Schema/语义校验、倍率相容性、位深范围和 `E_DECODE`。
- `engine.go`：分块、全整型累加、定点量化、截断台账和逐级累积偏差。
- `attribution.go`：首现色带扫描、四类份额和 `E_AMBIG`。
- `bounds.go`：仅由观测配置、刻度、倍率、位深、核支撑域和邻域值推导误差包络。
- `dag.go`、`cache.go`：平面级依赖图、精确失效和内容缓存键。
- `state.go`：分块级与处理级双层状态机。
- `checkpoint.go`：检查点接口、内存实现和快照哈希复核。
- `scheduler.go`：预算受限并行池、有序提交、取消恢复、异常隔离。
- `sqlitecheckpoint/store.go`：真实 SQLite 原子事务和重启快照作废。
- `reference/reference.go`：不调用主执行器的全平面参考实现与真值构造。
- `review/review.go`：只读取持久化产物/配置/真值指纹的独立复核器。
- `cmd/diagnostic/main.go`：可重复的本地诊断演示。

## 定点口径

样本为 `24×16` 的 `Y/Cb/Cr` 平面，块大小为 4，固定种子 `20261126`。两级均为
`2×2` 下采样，因此共有 6 + 2 = 8 个块级单元。

- 源位深：`BaseBits=10`。
- 第 0 级：输出 8 bit，`AccumScale=256`，`2×2` 取整盒核。
- 第 1 级：输出 6 bit，`AccumScale=256`，`3×3` 支撑核。
- 级间用 BaseBits 对齐振幅：`a = code << (BaseBits - stageBits)`。
- 级内定点输入：`v = a * AccumScale`。
- 主累加：`acc = Σ wᵢvᵢ`，整型除法记录商和余数。
- 量化格：`grid = AccumScale * 2^(BaseBits-Bits)`，四舍五入后重建定点值。

每个采样点都保存锚点、实际输入坐标、权重和、归一化刻度、原始累加值、余数、
量化格、输出码、输出定点值、截断记录、缓存键和四类分解。

四类分解精确望远镜相加：

```text
Chroma       = accC / W - accR / W
ResampleNorm = accC / NormScale - accC / W
AccumTrunc   = floor(accC / NormScale) - accC / NormScale
BitRound     = OutputFixed - floor(accC / NormScale)
----------------------------------------------------------------
Total        = OutputFixed - accR / W
```

其中 `W` 是核权重和，`NormScale` 是声明的定点归一化刻度。所有分数用
`math/big.Rat` 表达，测试逐点检查 `Breakdown.Sum() == TotalDeviation`。

## 色带与归因

扫描顺序固定为：级序号、平面声明顺序、先所有水平邻接对（行优先），再所有
垂直邻接对。跳变严格大于 `BandTolerance` 才判定为色带。

当前固定样本的解析结论：

```text
first band: stage=1 plane=Y coord=(4,1) neighbor=(5,1)
chunk=c01x00 jump=8192 tolerance=5120 dominant=bit_round
```

份额按四类贡献的绝对值归一化。最高份额达到工程声明的
`dominant_share_threshold=0.60` 才给出单一算子；否则返回 `E_AMBIG`，错误对象
列出四个算子的实测份额。

## 误差上界

边界不使用拟合常数或反推误差：

- 归一化残差：`|acc| * |W-NormScale| / (NormScale*W)`，`acc` 来自台账。
- 累加截断：`(NormScale-1)/NormScale`。
- 位深舍入：`QuantGrid/2`。
- 色度重采样：在主核与色度核并集支撑域内，用观测到的振幅极差乘以
  `AccumScale`；正权重平均不会放大该极差。
- 级间传播：按当前核权重对各上游点的有符号区间加权，并按
  `AccumScale_current / AccumScale_previous` 换算刻度。

每级记录倍率、刻度、位深和支撑半径。实测累积偏差或真值期望累积偏差越界、
与边界方向冲突时，返回带级序号和坐标的 `E_BOUND`。

## 依赖图与缓存

图节点粒度是“级 × 平面”的处理节点和色带判定节点，节点保存输出平面区域和
上游边。编辑色度核只失效色度平面及其下游/判定；编辑位深、倍率、主核等会
失效所有平面。调整级顺序时，从第一个顺序变化的位置起失效，前缀级可复用。

块缓存键由以下内容的规范化 SHA-256 共同决定：

```text
input region content hash + stage index + full stage parameters
+ algorithm version + sorted upstream cache keys
```

## 状态机与 SQLite

块状态：

```text
pending -> processing -> committed
                      -> failed -> processing
committed -> pending（仅显式失效/快照重建）
```

非法迁移返回 `E_STATE`，原状态不变。处理级也有同样的独立状态机。

SQLite 表 `units` 在同一事务中写：

- 输出 JSON 与 `output_digest`
- `content_hash`、`input_content_hash`
- `cache_key`
- `truncation_digest/count/total`
- `stage_sequence_fingerprint`
- 每级 `param_version`
- `algorithm_version`
- 单调提交序号

`commit_log` 与 `units` 行在一个 `BEGIN IMMEDIATE` 事务中提交；Go 互斥锁保证
写入不交错。重开数据库时校验元数据指纹、算法版本、每级参数版本，并重算持久化
JSON 的内容哈希和截断哈希；任一不符即清空旧 `units/commit_log/artifacts` 后
重建快照身份。

## 预算调度与三条路径

`Budget` 显式限制：

- `MaxParallel`：worker 数和实测最大并行度。
- `PendingTasks`：待处理任务 channel 容量；满时阻塞入队并写
  `pending_queue_budget`、开始时间和等待时长。
- `PerWorkerSamples`：worker 采样缓冲容量；缓冲满即 flush，测试验证 flush 次数。

提交循环按块的规范顺序提交，即使块完成顺序不同也不会打乱提交序列。失败点之后
不越过失败块提交，从而保证重试后的提交日志仍是规范顺序。

三条一致性路径：

1. 实时/受限并行 + SQLite 持久化。
2. 提交 3 块后取消；关闭并重开 SQLite，再断点恢复。
3. 全新存储上的一次性全量强制重算。

`CompareResults` 自动比较码值、定点值、余数、缓存键、色带归因和提交序列；
首个差异带级序号、块 ID、平面、坐标和字段名。

## 独立复核

`review.RunStore` 只读取：

- SQLite 中持久化的分块 JSON；
- `project.json` 中的级配置序列；
- 调用方给出的真值指纹。

它调用 `reference` 包重算，不调用 `ProcessChunk`、`Scheduler` 或
`ExecuteSequential`。发现差异时返回首个 `Violation{Stage, ChunkID, Plane,
Coord, Field, Expected, Actual}`。

## 常用命令

```bash
go build ./...
go vet ./...
go test ./...
go test -race .
go run ./cmd/diagnostic -out artifacts/diagnostic
```

诊断输出包括 `report.json`、`project.json`、`truth.json`、`review.json`、
`commit-sequence.json`、真实 SQLite 文件和控制台摘要。完整实测记录见
`VALIDATION.md` 与 `artifacts/verification.log`。

## 环境说明

当前容器没有 `sqlite3.h` 和 `libsqlite3.so` 开发软链，但存在
`/usr/lib/x86_64-linux-gnu/libsqlite3.so.0`。cgo 指令直接链接该版本化运行库。
若发行版路径不同，可增加对应的 `-L<libdir>`，或安装 `libsqlite3-dev` 后把
链接参数改为 `-lsqlite3`。`CGO_ENABLED=0` 时模块仍可编译，但 SQLite `Open`
会明确返回需要 cgo 的错误。
