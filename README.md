# 口型同步可核验内核

这是一个只依赖 Go 标准库与本地 SQLite 驱动的口型同步领域组件。固定种子生成器同时产出单声道 PCM、三个 schema 版本的对白工程、解析真值和参考包/帧结果；主流水线、断点恢复与独立复核工具都可离线重复运行。

## 快速运行

环境：Go 1.22+，无需 Docker、外部服务或账号。

```bash
go version
go mod download
go test ./...

# 生成样本、跑三版本装载、实时/全量/断点恢复三路径，并做独立复核
go run ./cmd/lipsync-demo

# 可选：启动 8000..9000 中第一个空闲端口的诊断服务
go run ./cmd/lipsync-demo -serve

# 只读复核一个或多个持久化结果
go run ./cmd/lipsync-verify \
  -truth artifacts/generated/truth.json \
  -db artifacts/checkpoints/realtime.db,artifacts/checkpoints/full.db,artifacts/checkpoints/resume.db
```

多库严格对比面向同一源版本的实时/全量/恢复路径。`v1`、`v2`、`v3` 因可表达字段不同，应分别单库复核；三版均应与同一真值给出相同包和帧。

默认产物：

- `artifacts/generated/{v1,v2,v3}/project.json`：三版本工程。
- `artifacts/generated/truth.json`：封存指纹的解析真值。
- `artifacts/generated/pcm/*.pcm`：确定性 `s16le` 单声道 PCM。
- `artifacts/checkpoints/*.db`：SQLite 检查点。
- `artifacts/verification-report.json`：本次运行与复核摘要。

演示命令会先清空对应输出目录，固定种子为 `42`。

## 包结构

| 包 | 职责 |
|---|---|
| `domain` | 内部帧单位模型、版本转换痕迹、错误码、稳定哈希 |
| `quant` | 采样点/毫秒/帧的纯整型换算，权重千分比/万分比换算 |
| `schema` | 三版本加载、字段路径校验、显式导出/转换与降级损失报告 |
| `merge` | 静音边界、亲和度、包上限约束下的确定性口型包归并 |
| `schedule` | 每帧预算、每通道并发槽位、延后/重试/拒绝调度 |
| `stage` | 六态录音分片状态机 |
| `store` | SQLite 单事务分片检查点、版本/哈希失效 |
| `pipeline` | 实时、全量、断点恢复三条路径与异常隔离 |
| `edit` | 按顺序应用边界、权重、帧率编辑，生成可重跑工程 |
| `graph` | 音素→包→逐帧下发依赖图与编辑失效传播 |
| `verify` | 不调用主调度路径的独立只读复核器 |
| `generate` | 确定性 PCM、三版本 schema、真值及独立参考构造 |
| `diagnostics` | `net/http` 只读诊断接口 |

## 三版本 schema

三个版本描述同一组对白，但刻意采用不同的字段和表达：

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 时间主字段 | `start_frame` / `end_frame` | `start_ms` / `end_ms` | `from_frame` / `to_frame` |
| 时间基准 | 帧 | 毫秒 | 帧 |
| 权重刻度 | 千分比 `weight_milli` | 万分比 `weight_bp` | 万分比 `duration_weight` |
| 类别表 | 扁平 `code -> string` | 扁平 `code -> {kind}` | `category_groups` 分组 |
| 过渡亲密度 | 无字段，按默认 `1` 补齐 | 可选 `transition_affinity` | 可选 `affinity` |
| PCM 字段 | `pcm.samples/sha256` | `audio.sample_count/content_sha256` | `pcm_meta.sample_count/sha` |

三版还都携带逐音素采样点副字段（v1/v2 为 `start_sample/end_sample`，v3 为 `sample_start/sample_end`）。加载器会按工程声明的取舍方向执行 `sample -> frame`，并要求结果与主时间字段一致，否则返回 `E_SCHEMA`。

加载后内部模型统一为整数帧、整数万分比权重。`Project.Traces` 保存字段路径、源版本、旧值、转换值、截断余数和除数；`Project.Defaults` 单独记录缺省补齐路径、旧值与默认值来源。

`schema.Marshal` / `schema.Convert` 显式导出到目标版本。降级到无法表达分组类别或亲密度的版本时返回 `LossReport`，其 `AsError()` 为 `E_LOSSY`，每项包含字段路径、源/目标版本和处置动作。导出结果可立即被目标版本加载器读回。

## 确定性规则

### 量化

- 全部使用 `int64` 有理数运算，不做浮点时间累积。
- 支持 `floor`、`ceil`、`nearest` 三种取舍。
- 每个换算结果带 `Truncated/Remainder/Divisor/Field`，加载 trace 同步记录。
- 乘法溢出返回 `E_OVF`，错误中保留字段和两个操作数。
- 生成器权重均为可无损往返的万分比值；音素边界在 50 fps 下可精确映射到毫秒。

### 口型包归并

- 先按 `(start_frame, end_frame, id)` 建立稳定全序，输入枚举顺序不影响结果。
- 包内不能混合静音与非静音；静音段独立成包。
- 非静音相邻音素仅在过渡亲和度达到阈值、未超过每包最大音素数、时间连续时合并。
- 亲和度必须在 `[0,1]`，否则返回 `E_PARA` 并带音素 ID、分片和取值。
- 分块解析完成顺序不影响结果；`PartitionBatched` 收集全部块后重新全序归并。

### 逐帧下发

- 输入包按自然起始帧、通道、对白 ID、包 ID 稳定排序。
- 同一帧受全局 `frame_budget` 和每通道 `channel_slots` 双重限制。
- 无法放入时整包延后一帧重试，持续记录 `delayed` 与下一次 `retry_frame`。
- 超过 `max_delay_frames` 仍无容量则记录 `rejected`，不会静默丢弃。
- 被受理包保持原持续时间；若短于 `min_visible_frames`，下发区间补足到最短可见帧数。
- 发生延后时整个下发区间右移；逐帧集合按包 ID 排序。
- 外部分片结果若超预算，`ValidateSchedule` 返回 `E_BUDGET`，带首个违规帧号和通道。

## 检查点与恢复

分片状态固定为：

```text
pending_load -> quantizing -> merging -> dispatching -> committed
       |             |           |            |
       +-------------+-----------+------------+--> failed -> pending_load
```

非法迁移返回 `E_STATE`，状态和进度保持不变。

SQLite 每个分片一次事务原子写入：

- 包划分摘要和完整调度包。
- 该分片逐帧下发记录。
- 内容哈希、源版本、量化参数版本、算法版本、真值指纹。
- 延后/重试/拒绝事件。

写操作由互斥锁与单连接双重串行化，提交序号连续且不交错。恢复时逐分片比较内容哈希和版本字段；任一不匹配会整体删除该分片及其帧/事件，再重新装载。强制终止只会留下完整提交的分片。

## 编辑失效

依赖图层级为：

```text
phoneme -> package -> dispatch(package, frame)
package -> 下游竞争包
```

- 帧率编辑使全部音素、包和下发帧失效。
- 边界编辑使该音素所在包及同对白下游时间线包失效。
- 权重编辑使所在包及存在通道/全局预算竞争关系的下游包失效。
- 输出 `Invalidation`，同时列出 `FrameNodes`、`PackageIDs` 和 `ReusedPackages`。
- `pipeline.ReusePersisted` 只从已提交分片读取未失效包及其逐帧记录，供编辑后拼接复用。

## 独立复核

`verify` 只读取 SQLite 的 `shards`、`frames` 表与真值清单，不导入 `merge`、`schedule` 或 `pipeline`：

- 校验真值清单指纹、分片元数据、包摘要、自然包区间、受理状态、调度区间和并集逐帧集合。
- 三路径比较同时检查内容哈希、源版本、量化/算法版本、分片提交序号和逐帧集合。
- 首个差异返回 `E_DIFF`，字段包含 `dialogue_id`、`shard_index`、`frame`、`field`、`expected`、`actual`。

## 错误码

| 码 | 场景 |
|---|---|
| `E_SCHEMA` | 未知版本、必填字段缺失、字段/时间基准/采样点矛盾 |
| `E_LOSSY` | 降级无法表达分组类别、亲密度或精度 |
| `E_OVF` | 整型换算溢出，带字段与操作数 |
| `E_PARA` | 亲和度等参数越界 |
| `E_BUDGET` | 每帧预算或通道并发槽位超限 |
| `E_STATE` | 非法分片状态迁移 |
| `E_DIFF` | 持久化产物或三路径首个差异 |
| `E_ORDER` | 音素区间倒置、重叠或顺序冲突 |
| `E_RATE` | 帧率、采样率、`samples_per_frame` 矛盾 |
| `E_CLASS` | 类别编码未声明或类别非法 |
| `E_STAGE` | 分片内部失败或意外阶段错误 |
| `E_CANCELED` | 取消/主动停止；只保留完整提交分片 |

## 诊断接口

`-serve` 会在 `127.0.0.1` 的 8000..9000 范围内选取第一个可用端口：

- `GET /healthz`
- `GET /v1/summary`
- `GET /v1/packages?dialogue=d01`
- `GET /v1/frames?from=0&to=30`
- `GET /v1/truth`

仅监听本机，不访问网络外部目标。

## 实测结果

环境：`go1.22.12 linux/amd64`，SQLite 驱动 `modernc.org/sqlite v1.34.5`。

固定种子 `42` 的当前结果：

- 生成 4 条对白、4 个录音分片、17 个口型包、58 个下发帧。
- `v1`、`v2`、`v3` 分别独立装载并执行后，均得到 17 包/58 帧。
- 三版真值复核通过，真值指纹：
  `04e2d376037e245c7f8a7d999a4c3e6c7c814c1e799db7074bb79789eb9e8c50`。
- 实时、一次性全量、终止后断点恢复三条路径提交序列均为 `[1,2,3,4]`，逐帧集合和包集合一致。
- 对同一编辑序列（边界、权重、50→40 fps）重跑三路径，持久化包/帧/提交序列仍完全一致。
- 全有效恢复复用 4 个分片，未追加新提交。
- `go test -count=1 ./...` 与 `go test -race ./...` 均通过。

测试覆盖三版本装载与损失报告、整型溢出与舍入、枚举/分块确定性、静音边界、预算与槽位、并发 SQLite 提交、哈希失效重算、断点恢复、取消、依赖失效、独立复核首个差异以及全部指定异常码。
