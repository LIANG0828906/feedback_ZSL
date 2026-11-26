# 骨骼动画与动作重定向首个验收版本

本仓库在空 Go 模块上独立实现了确定性样本、三格式装载/导出、整数定点四元数、重定向、误差上界、依赖失效、预算线程池、区间状态机、原子检查点和三路径逐帧比对。

## 复现命令

```bash
gofmt -w *.go cmd/bonerig/*.go
go vet ./...
go test ./...
go test -race ./...
go run ./cmd/bonerig -out artifacts/run
```

CLI 会生成：

- `artifacts/run/samples/rig-v1.json`
- `artifacts/run/samples/rig-v2.json`
- `artifacts/run/samples/rig-v3.json`
- `artifacts/run/samples/truth.json`
- `artifacts/run/dependency-graph.dot`
- `artifacts/run/verification-report.json`
- `artifacts/run/checkpoints/{realtime,recovery,full}/checkpoint.json`
- `artifacts/run/checkpoints/{realtime,recovery,full}/checkpoint.sql`

## 实测结果

以下结果来自当前工作区：

- `go vet ./...`：通过，无输出。
- `go test ./...`：通过；根包与 `cmd/bonerig` 均编译成功。
- `go test -race ./...`：通过（约 1.6s）。
- `go run ./cmd/bonerig -out artifacts/run`：退出码 0。
- 用 Python 标准库 `sqlite3` 对三份 `checkpoint.sql` 执行 `executescript` 均成功；每份包含 3 个区间、72 条关节摘要和 72 条精度分量记录。

CLI 记录的三格式最大 L1 偏差/上界：

| 格式 | 帧数 | 最大实测偏差 | 最大上界 | 真值容差 |
| --- | ---: | ---: | ---: | ---: |
| `rig.v1` | 12 | 35,253 | 34,659,719 | 8,000,000 |
| `rig.v2` | 12 | 8,714 | 13,449,886 | 8,000,000 |
| `rig.v3` | 12 | 395 | 392,072 | 8,000,000 |

三条路径的规范区间提交顺序均为：

```text
frames-0000-0004 -> frames-0004-0008 -> frames-0008-0012
```

实时路径与强杀恢复路径的持久化写入序列一致：

```text
frames-0000-0004, frames-0004-0008, frames-0008-0012,
frames-0008-0012, frames-0008-0012
```

后两个 `frames-0008-0012` 分别对应约束编辑和重定向系数编辑。一次性全量路径的历史写入序列是三个区间各提交一次；其当前规范提交顺序和最终帧结果与另外两条路径完全一致。

## 需求映射

1. **确定性生成与三格式装载**：`generator.go` 用固定种子 42 生成 6 个源/目标关节、12 帧、3 个区间、约束、根位移、映射和比例。`formats_export.go`/`formats_load.go` 实现：
   - V1：`$.bones` 平坦表、`xyzw`、刻度 10000、毫度角度、tick、无重定向缩放。
   - V2：`$.armature.nodes` 平坦表、`wxyz`、刻度 32768、弧度、毫秒、可选千分比系数。
   - V3：`$.rig_tree.roots[].children` 嵌套树、显式 `qw/qx/qy/qz`、刻度 1048576、弧度、毫秒、显式分子/分母系数。
   - 未知版本或必填字段缺失返回 `E_SCHEMA` 和 `$.path[index].field`。
2. **升降级痕迹**：`errors.go` 的 `TransformTrace`/`LossyItem` 记录字段路径、旧值、默认值、处置方式和 `schema-rules-v1`。V1 降级清单包含每个父关节的嵌套树扁平化和每条映射的缩放字段省略；测试验证清单无重复且同版本导出/加载往返后层级、约束、旋转逐字段一致。
3. **定点四元数**：`fixed.go` 在声明刻度上做整数模长检查、整数平方根、四舍五入除法、分量截断位置和余数记录。公共工作刻度 `655,360,000` 是 `10000`、`32768`、`1048576` 的公倍数，格式扩宽无除法。运行时重定向不使用浮点累积；只有度/弧度 schema 边界转换使用一次 `math.Round`。非法刻度、模长超差或溢出返回 `E_QUAT`，含关节、帧和两个操作数。
4. **重定向**：`retarget.go` 按父子链执行整数 Hamilton 乘法得到源世界朝向，按主源+附加权重重新分配到目标世界朝向，再求目标局部旋转；位移按比例声明换算。`reference.go` 使用 `math/big` 的 40 位高精度整数生成独立真值。缺映射或父链断点返回 `E_MAP`、关节和 `breakpoint`。
5. **精度传播**：`PrecisionRecord` 逐帧逐目标关节记录归一化残差、链乘法截断、比例取整偏差、映射重分配偏差、实测偏差和总上界。上界只使用 `PrecisionParams.Bound` 中声明的分量数/L1 增益、实际刻度比、链长、系数和观测余数。超界返回 `E_UB`，错误体含四项上下界、实测值和合计上界。
6. **依赖图与精确失效**：`graph.go` 显式建立 action/topology/mapping/proportion/precision -> interval -> pose -> export DAG；缓存键包含动作哈希、区间 ID、拓扑版本、参数版本、算法版本、依赖集合和上游键。样本中 `hand_l` 仅在 `frames-0008-0012` 运动，因此其约束或映射系数编辑只失效该区间；全局精度编辑失效全部区间。失效项含区间、原因和命中依据。
7. **预算与状态机**：`budget.go` 限制最大 worker、每 worker 关节状态缓冲和待处理帧队列；队列/缓冲满时阻塞并记录原因与等待纳秒。`state.go` 强制 `pending_load -> pending_retarget -> retargeting -> committed/failed -> pending_retarget`；非法迁移返回 `E_STATE` 且保留状态和进度。测试注入第 8 帧失败，验证前两个区间已提交、第三区间未提交，重试后成功。
8. **检查点与三路径一致**：`checkpoint.go` 的 `SQLiteCheckpointStore` 用互斥锁串行化提交，以临时文件、`fsync` 和原子 rename 写 JSON 快照；每次提交包含关节摘要、四项误差、依赖集合、各类哈希/版本和完整帧结果。同名 `.sql` 导出 SQLite DDL/DML，并按区间输出 `BEGIN IMMEDIATE ... COMMIT`。`scenario.go` 编排实时、强杀恢复和一次性全量三条路径；`reconcile.go` 比较帧旋转、位移和全部误差分量，首个不一致返回带关节、帧、字段、期望值和实际值的 `E_DIFF`。

## 关于 SQLite 驱动的环境约束

任务同时要求 SQLite 检查点且禁止第三方包/系统安装。Go 标准库有 `database/sql` 接口但不内置 SQLite 驱动，因此当前交付不下载 `mattn/go-sqlite3` 或 `modernc.org/sqlite`，而是提供：

- 可恢复的原子事务快照 `checkpoint.json`；
- 可直接载入 SQLite 的 `checkpoint.sql`；
- `SQLiteCheckpointStore` 类型和稳定的表结构，后续若允许仓库内纯 Go 驱动或批准依赖，可把 `CommitInterval` 的同一事务载荷切换为真实 SQL 写入，上层恢复/比对协议无需变化。

本环境已用 Python 标准库 SQLite 3.46.1 验证导出的 SQL；产品代码和 `go test` 不依赖 Python 或任何外部服务。
