# dialogueaudio

游戏内语音对白音频领域组件与本地可重复诊断工具。项目只使用 Go 标准库，
不依赖第三方包、网络、外部服务或容器；样本、工程描述和逐帧真值全部由固定
种子的生成器在本地构造。

当前模块版本为 `go 1.21`，实际验证环境为 Go 1.22.12 / Linux amd64。

## 快速运行

```bash
go test ./...
go test -race ./...

# 只生成固定种子样本
go run ./cmd/dialoguekit generate \
  --seed 20261127 \
  --out var/samples

# 生成样本并执行完整验收诊断
go run ./cmd/dialoguekit demo \
  --seed 20261127 \
  --out var/demo \
  --max-parallel 2 \
  --queue-budget 1 \
  --sample-buffer 960
```

`demo` 会把 JSON 报告写到 `var/demo/report.json`，同时输出到 stdout。
核心音频值、校准值、提交序列、失效清单和样本哈希均由固定种子决定；队列等待
时长是运行时诊断量，不参与音频结果复算。

## 交付结构

| 包 | 职责 |
|---|---|
| `audioerr` | 稳定错误码、字段路径、操作数与结构化细节 |
| `fixed` | 全整型定点换算、舍入、残差、溢出检查、dB/线性幅度 |
| `model` | 唯一内部工程模型，统一采样点时间与 Q16 刻度 |
| `codec` | 三版本 schema 装载、升级默认值、降级与 `E_LOSSY` |
| `generator` | 固定种子 PCM、工程样本、逐帧真值与 manifest |
| `engine` | 逐帧能量/峰值、分段包络、增益校准、`E_LOUD` |
| `state` | 五态片段状态机、提交边界和进度 |
| `graph` | 片段/帧区间依赖图、缓存键和精确失效传播 |
| `checkpoint` | 文件型原子检查点，替代不可用的标准库 SQLite 驱动 |
| `resultcache` | 按缓存键持久化并复用未失效片段结果 |
| `pipeline` | 并行测量、有序提交、恢复、三条路径比较 |
| `diagnostics` | 本文件所述验收诊断报告 |
| `cmd/dialoguekit` | `generate` / `demo` 本地入口 |

## 三个 schema 版本

生成器同时输出：

- `schema_v1.json`
- `schema_v2.json`
- `schema_v3.json`
- `truth.json`
- `manifest.json`

| 项目 | v1 | v2 | v3 |
|---|---|---|---|
| 版本字段 | `schema_version` | `schemaVersion` | `revision` |
| 片段组织 | 根级扁平 `clips` | `scenes[].clips` | `scenes[].groups[]`，支持 `subgroups[]` |
| 时间 | 毫秒 | 微秒 | 采样点 |
| 帧参数 | `frame_ms` / `hop_ms` | `frameDurationUs` / `hopDurationUs` | `frame_samples` / `hop_samples` |
| 线性增益/权重 | 千分比 `q1000` | 万分比 `q10000` | 百万分比 `u` |
| 目标响度 | `q100` dB | `q10000` dB | 百万分比 dB |
| 校准能力 | 无，升级补默认值 | 会话级开关 | 会话级开关 + 片段校准标记 |
| 字段风格 | snake_case | camelCase | 混合短字段 |

装载后统一为：

- 时间：采样点 `int64`
- 线性增益、响度权重、dB：Q16 `int64`（`65536` 表示 `1.0` 或 `0 dB`）
- 每个换算写入 `conversion_traces`，包含输入、输出、商、余数、残差和舍入方向
- 缺失的可选低版本字段写入 `defaults`，记录字段路径、旧值、默认值和默认值来源

未知版本或必填字段缺失只返回 `E_SCHEMA`，例如：

```text
E_SCHEMA: schema_version: unknown schema version 99
E_SCHEMA: project.id: required field is missing
```

## 定点与换算口径

核心算法不做浮点累积。支持以下舍入方向：

- `trunc_zero`：向零截断
- `floor`：向下
- `ceil`：向上
- `nearest_ties_away_from_zero`：四舍五入，绝对值相等时远离零

`fixed.Trace` 的 `Remainder` 是整数除法余数，`Residual` 是
`output*fromScale - input*toScale`，即已量化输出在分母单位上的误差。高版本到
低版本再回高版本时：

- 生成器中的片段边界与增益按可表达刻度构造，故报告中的全部边界/增益/权重
  可精确还原，且两次换算残差稳定。
- 对任意不可表达值，第一次降级形成规范低精度值；之后的升降级保持该规范值和
  对应方向的残差不变，不会发生重复漂移。

溢出返回 `E_OVF`，包含字段名和两个操作数。线性幅度与 dB 使用整数
`log2/exp2`、16 位小数和查表，全程无浮点。

## 响度与校准

1. 按片段边界、帧长和帧移产生逻辑帧，最后一帧允许不足帧长。
2. 每帧计算整数均方能量、峰值位置与峰值样本。
3. 依次应用会话增益、片段增益、片段响度权重，再转为 Q16 dB 包络值。
4. 片段摘要能量是逐帧加权能量的整数平均，再转成片段响度。
5. 校准逐片段求出 `target - measured`，按声明上界 `±max_adj_db` 判定可达性。
6. 可达时把线性校准增益重新作用到每帧能量并重新求 dB；不可达返回 `E_LOUD`，
   携带目标值、可达上限/下限、全局边界和首个受限片段 ID。

生成器真值文件不读取引擎结果，只由生成器内部参考整数构造。默认样本为：

- 6 个片段，3 个场景
- 每片段 5760 采样点、12 帧，共 72 帧
- 48 kHz、480 采样点帧长/帧移
- 默认目标响度 `4532436 Q16 dB`，调整上界 `6 dB`

本次实测在常规容差 `4 Q16` 和收紧容差 `0 Q16` 下，72 帧最大偏差均为 `0`。

## 状态与提交

片段只允许以下主路径：

```text
pending -> measuring -> calibrating -> committed
             |              |
             +-------> failed <------+
```

`failed` 与 `committed` 为终态。非法迁移返回 `E_STATE`，状态和
`committed/total` 进度都不变化。提交后的片段冻结包络与校准结果，编辑接口只允许
未提交且未失败的片段。

## 依赖图与失效

缓存键公式由以下内容共同决定：

```text
SHA-256(
  content_hash,
  frame_interval_id,
  quantization_version,
  sorted(upstream_cache_keys...)
)
```

图中显式建立：

- 内容哈希、片段边界、类别、响度权重、片段增益、帧率、会话增益
  -> 片段包络
- 片段包络 -> 每个帧区间节点
- 包络、帧区间、目标响度、调整上界、片段增益 -> 校准节点

失效从被编辑输入沿有向边扩散：

- 编辑片段增益：只失效该片段包络、该片段全部帧区间和该片段校准。
- 编辑目标响度：只失效所有校准节点，不失效任何帧包络；包络缓存全部复用。
- 编辑帧率：失效所有片段包络、全部帧区间与全部校准。

报告包含片段 ID、帧号、节点 ID、按键摘要，以及实际复用的片段列表。未失效结果
来自 `resultcache` 的持久化文件，不重新测量。

## 文件型检查点

Go 标准库没有 SQLite 驱动，因此这里使用仓库内文件持久化，并保持题目要求的
SQLite 事务语义：

- `<checkpoint-dir>/checkpoint.json` 是单账本。
- 每提交一个片段，在 `flock` 排他锁内校验完整账本，追加一个带稳定序号的记录，
  写临时文件、`fsync`、原子 `rename`、再同步目录。
- 一个片段一次事务，写入不交错；提交顺序固定为规范片段顺序。
- 每个片段记录包络摘要、增益调整量、内容哈希、源版本、换算版本、量化版本、
  算法版本、完整包络与校准结果。
- 记录和账本均有 SHA-256 校验；任何内容哈希、项目指纹、版本、顺序、摘要或
  校验和不匹配都返回“整体重建”，保留原文件前不会拼接旧产物。
- 重启只依据已提交前缀恢复；取消或强制终止留下的临时文件不构成提交。

如果后续环境提供 SQLite 标准库实现，可在 `checkpoint.Store` 接口下替换存储，
其他领域包无需改动。

## 并行预算与取消

`engine.BatchOptions` / `pipeline.Options.Batch` 限制：

- `MaxParallel`：最大工作协程数
- `SampleBufferSamples`：单工作协程每个分块允许的采样缓冲上限
- `QueueBudget`：待处理片段队列预算

队列预算耗尽时生产者阻塞，并记录 `queue_budget_exhausted`、片段 ID 与实际等待
微秒数。分块大小和调度顺序不影响逐帧整数结果。

取消返回 `E_CANCELLED`；只有检查点中已原子提交的前缀可恢复，不承诺取消时生成
完整退出报告。

## 错误码

| 错误码 | 用途 |
|---|---|
| `E_SCHEMA` | 未知版本、缺字段、类型/路径错误 |
| `E_LOSSY` | 降级无法表达字段的非致命清单 |
| `E_OVF` | 整型溢出，含字段和操作数 |
| `E_LOUD` | 目标响度在全部片段调整上界内不可达 |
| `E_STATE` | 非法片段状态迁移 |
| `E_IO` / `E_CHECKPOINT` | 文件或检查点持久化失败 |
| `E_CANCELLED` | 上下文取消 |
| `E_MISMATCH` | 三条路径自动比对定位到差异 |

## 本次实测结果

固定种子 `20261127`、`--max-parallel 2 --queue-budget 1 --sample-buffer 960`
的实际结果：

- 三 schema 均装载 6 个片段，片段时间、增益、权重、类别和 PCM 内容哈希一致。
- v1 装载产生 29 条换算痕迹、16 条升级默认记录；v2 为 29/12；v3 为 15/0。
- v3 -> v1 生成 `E_LOSSY`，共 11 项；样本中的嵌套分组、场景名与校准标记均记录处置方式。
- v3 -> v1 -> v3 后 6 个片段的边界、增益、权重全部相等，残差稳定。
- 72 帧真值在容差 4 和 0 下最大偏差均为 0，收紧容差不增大偏差。
- 实时、断点恢复、一次性全量三条路径逐帧包络、校准结果和提交序列一致。
- 取消发生在 2/6 已提交时，恢复从第 2 个已提交片段之后继续，最终提交序列为
  `c01,c02,c03,c04,c05,c06`。
- 修改内容后检查点不拼接旧结果，整体重建并重新提交 6 个片段。
- 编辑 `c02` 增益只重算 `c02` 的 12 个帧区间，复用其余 5 个片段；
  修改目标响度不重算帧包络，只重算 6 个校准；修改帧率重算全部 72 帧。
- `E_SCHEMA`、`E_LOUD`、`E_STATE`、`E_OVF` 均可由 `demo` 稳定复现；
  `E_LOUD` 的首个受限片段为 `c01`。
- `go test ./...`、`go vet ./...`、`go test -race ./...` 全部通过。

更完整的机器可读结果由 `var/demo/report.json` 生成；该目录已被 `.gitignore`
排除，可随时用同一命令重建。
