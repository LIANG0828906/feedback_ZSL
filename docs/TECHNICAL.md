# 技术说明

## 包结构

- `internal/model`：工程、块、边界、真值、错误码和 SHA-256 规范化摘要。
- `internal/sample`：固定种子 `42` 的确定性样本生成器，写出 `project.json`、`declarations.json`、`elevations.json`、`boundaries.json`、`truth.json`、`probes.json`。
- `internal/loader`：文件加载、必填字段、覆盖关系、预算/容差范围、共享边列长度和顺序签名校验。
- `internal/mesh`：规则幂次网格上的确定性递归三角形细分、预算深度选择和整数重心误差估计。
- `internal/boundary`：边界序列提取、强制对齐采样、折角计算、高度误差、顺序错位和 `E_ATTR` 判定。
- `internal/graph`：双向邻接/边界依赖、编辑影响范围和稳定调度顺序。
- `internal/engine`：失效清单、受限并行、资源排队、失败隔离、缓存、规范化比对、恢复和三路径验证。
- `internal/store`：`Store` 端口、内存实现和纯 Go SQLite 追加式检查点。
- `internal/web`：`net/http` JSON API 与内嵌单页诊断界面。
- `internal/verifier`：样本真值和三路径验收器。
- `cmd/terrainpatch`：`generate`、`verify`、`serve` 命令。

## 确定性剖分

样本由 2×2 个块组成，每块包含 `17×17` 个高程点，定点比例为 `1000`，覆盖 `16000×16000` 个定点坐标单位。递归深度 `d` 对应：

- 边分段数：`2^d`
- 顶点数：`(2^d+1)^2`
- 三角形数：`2·4^d`

算法从深度 `0` 开始构造两组覆盖矩形的三角形；若全块最大误差仍大于容差，则进入下一递归深度，直到误差满足容差、采样预算不允许更多顶点或达到栅格分辨率。三角形误差使用整数重心权重计算，输出 `maxAbsError` 与 `l1Error`，不依赖浮点误差判定。

顶点 ID 按规则网格的 `(y,x)` 顺序生成，三角形顺序固定。调度器、线程数、草稿/正式路径只改变何时计算，不改变输入和构造顺序，因此摘要逐项一致。

## 错误码

| 码 | 触发点 | 定位信息 |
| --- | --- | --- |
| `E_SCHEMA` | 文件/必填字段缺失、JSON 类型错误 | 文件名、块或边界字段 |
| `E_META` | 预算非正、容差为负、覆盖冲突、边界列长度/顺序不一致 | 块号/边界号、字段、实测值与限制 |
| `E_BUDGET` | 实测三角形数超过声明上限 | 块号、`actual`、`limit`、深度和预算 |
| `E_PARA` | HTTP/编辑序列参数越界 | 块号、字段、允许范围 |
| `E_ATTR` | 折角超限，但高度误差和对齐顺序均不满足归因判据 | 边界号、实测角度、两类判据实测值 |
| `E_DIFF` | 增量/恢复路径与全量结果不一致 | 首个块号/边界号、字段、期望值和实际值 |
| `E_RESOURCE` | 块超过单线程顶点缓冲硬上限 | 块号、实测/估算顶点数和上限 |

## 边界归因

诊断器先提取两侧按边方向排序的边界点，再合并两侧坐标并在线性段上采样高度。对每个内部位置计算两种跨边行进方向的偏折角，取最大者，角度以 `micro-degree`（度 × 10^6）持久化。

归因顺序：

1. 最大折角未超阈值：不标记断裂。
2. 任一侧边界高程误差超过该块容差：`undersampling`。
3. 否则两侧有序坐标序列不一致：`alignment_mismatch`。
4. 两者都不满足：返回 `E_ATTR`，错误详情包含最大折角、A/B 高度误差与容差、对齐差异数。

样本中 `E01`、`E02` 是真实块边界上的欠采样案例；`P-ALIGN` 是低误差但顺序交换的对齐错位探针；`P-ATTR` 是精确采样但存在锐折角、无法归入前两类的探针。

## 失效传播与提交

依赖图显式保存：

- `neighbors[tile]`：双向相邻块；
- `boundaries[tile]`：关联边界；
- `dependencies[tile]`：邻接块和边界 ID 的排序集合。

编辑某块的 `height`、`sampleBudget` 或 `tolerance` 时，影响集合固定为该块和全部共享边邻接块；其他块在报告中为 `reused=true`、`status=cache-hit`。

正式调度使用固定 worker 信号量限制最大并行度；每块先检查单线程顶点缓冲上限，再获取网格内存预算。内存不足时通过条件变量阻塞，并在 `queueReasons` 中记录 `memory-budget`；worker 槽不足记录 `worker-slot`。任务不会被静默丢弃。

所有 worker 完成后，主 goroutine 按稳定的块顺序逐个开启 SQLite 事务，将块检查点和提交日志一起提交。SQLite 连接池限制为一个连接，因此写入不会交错。`tile_checkpoints` 采用追加写入，不更新或删除旧块版本；重新剖分产生新的提交序号和内容哈希。

## 版本化恢复

每个检查点包含：

- 顶点集合规范化摘要；
- 三角形集合规范化摘要；
- 边界折角统计；
- 依赖集合；
- 参数版本和算法版本；
- 完整内容的 SHA-256 `content_hash`。

重启恢复时检查全局 schema/algorithm 版本、每个当前块的参数/算法版本以及内容哈希。只要一个当前快照块不匹配，就清空检查点并回退到全量重剖分，避免新旧块混用。全部匹配时恢复块网格和边界统计，只重跑失效集合。

`VerifyThreePaths` 的流程是：

1. 空内存缓存执行全量基准，再应用编辑序列得到草稿增量结果。
2. 另一份工程先写 SQLite 基准，在第一次编辑的首个块提交后注入“进程中断”。
3. 关闭并重开 SQLite，执行版本校验和恢复，再完成同一影响集合和后续编辑。
4. 第三份工程直接应用相同编辑序列，从空缓存一次性全量剖分。
5. 比较提交块顺序、顶点集合、三角形集合、逐三角形误差和边界统计；首个差异以 `E_DIFF` 返回。

最终快照的 `commitSequence` 始终是稳定的全块顺序 `T00,T10,T01,T11`；`writtenSequence` 是本轮实际串行写入的失效块，因此增量轮次可能只包含子集。

## HTTP 诊断面

启动 `scripts\serve.bat` 或 `go run ./cmd/terrainpatch serve -addr :8080` 后：

- `GET /`：SVG 分块着色、三角线框、折角/类别标注、失效状态、依赖、缓存耗时、预算和队列。
- `GET /api/state`：工程、图、最近报告和真值。
- `POST /api/edit`：编辑 `sampleBudget`、`tolerance` 或中心格点 `height`。
- `POST /api/retry`：清理瞬时注入失败并重试单块。
- `POST /api/failure`：注入单块内部异常以观察隔离。
- `POST /api/rebuild`：全量重剖分。
- `POST /api/verify`：运行三路径一致性验证。
- `GET /api/export`：导出工程、最近报告和 SQLite 提交历史。

## Windows 注意事项

- 需要 Go 1.22+；`go.mod` 固定 `modernc.org/sqlite v1.29.10`，已用 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/terrainpatch` 验证交叉编译。
- 8080 端口被占用时可改用 `-addr :18080`。
- 重复验证前可删除工作区下的 `data\*.db`、`data\*.db-wal`、`data\*.db-shm`；样本可由命令重新生成。
- 强制终止只能依赖已经提交的 SQLite 事务；未提交事务由 SQLite 回滚，程序不承诺在强杀时写出退出报告。
