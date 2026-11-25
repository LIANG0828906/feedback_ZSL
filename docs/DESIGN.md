# MacroKit 设计说明

MacroKit 是战斗输入宏编排工具的 Go 领域组件与本地示例。模块名是 `macrokit`，Go 版本为 1.22；领域核心只依赖标准库，SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，因此 Windows/amd64 可交叉编译，不需要 GCC 或容器。

## 目录与类型

- `model.go`：`Project`、`Branch`、`Condition`、`Step`、事件、帧轨迹、分支状态等内部模型。
- `format1.go`：格式号 `1`，snake_case、扁平常量条件表、帧时间。
- `format2.go`：格式号 `2`，lowerCamelCase、嵌套条件树、毫秒时间。
- `format3.go`：格式号 `3`，PascalCase、嵌套 `Type/Nodes` 树、帧时间。
- `format_export.go`：三格式导出与 `ConvertProject` 往返转换。
- `conditions.go`：扁平表解析后的统一条件树求值。
- `time.go`：纯整数帧/毫秒换算与溢出检查。
- `executor.go`、`realtime.go`：逐帧执行核心和逻辑实时入口。
- `conflict.go`：同帧步骤区间冲突检测与优先级裁决。
- `graph.go`、`incremental.go`：依赖图、失效清单和持久化分支缓存。
- `store_sqlite.go`、`checkpoint_runner.go`：SQLite 原子窗口、恢复、失败重试和缓存表。
- `compare.go`、`truth.go`：三路结果比对、真值校验和 `E_DIFF` 定位。
- `generator.go`、`sampleio.go`：固定种子样本生成器与样例落盘。
- `cmd/macrotool`：生成、验证、异常注入 CLI。

## 统一加载

`LoadProjectBytes` 先读取顶层 `format`：

- 缺少 `/format` 或格式号未知：`E_SCHEMA`，错误包含 JSON 指针风格 `Path`，例如 `/format`。
- 必填字段缺失或类型不合法：`E_SCHEMA`，路径精确到 `/branches/0/branchId` 等字段。
- 扁平表的 `when_ref`、`refs[i]` 或嵌套 `branchId` 指向不存在节点：`E_REF`，`Ref` 为越界标识。
- 分支前置结果形成环：返回 `E_REF`，避免求值和依赖传播进入环。

加载成功后的 `Project.FormatNo`、`FormatName`、`FormatVersion` 和 `SourcePath` 保留源格式身份。

## 确定性条件求值

所有格式都归一为 `Condition` 树。支持：

- `all` / `any` / `not`
- 按键 `down` 与单帧 `pressed`
- 在 `trigger_window_frames` 内基于 `PressedAt` 的整型按键窗口
- 静态/注入标志位
- 前置分支的 `completed`、`canceled`、`suppressed`、`triggered/any`、`running` 结果

执行第 `f` 帧时，条件只能看到 `CommitFrame < f` 的已提交分支结果；当前帧新提交的结果在下一帧才可见。求值器不修改状态，详细节点值写入 `FrameTrace.ConditionSnapshot`。分支按 ID 排序，因此 map 遍历、并发求值或调度顺序不会改变结果。

## 时间换算

内部统一使用帧。默认策略：

- 帧到毫秒：`floor(frames * 1000 / frameRate)`
- 毫秒到帧：`ceil(ms * frameRate / 1000)`

全程使用 `int64`，乘法前用 `math.MaxInt64` 检查溢出；溢出返回 `E_OVF` 和 `BranchID`。在 60 FPS、帧率不大于 1000 且样本取值范围内，floor/ceil 配对保证整数帧经“帧 → 毫秒 → 帧”不变。测试覆盖 0 到 1000 帧以及高格式 → 低格式 → 高格式后的延时、窗口、步骤间隔、触发帧和完整轨迹。

## 逐帧执行

`Executor.Step` 是实时、回放和检查点恢复共用的唯一状态推进函数：

1. 清理上一帧 `Pressed/Released` 边沿。
2. 按输入顺序应用本帧按键事件，维护 `Down`、`PressedAt` 和已消费事件数。
3. 用当前快照和历史已提交结果求值所有未触发分支。
4. 检测候选分支的步骤 lane/时间区间冲突并裁决。
5. 登记被压制分支和裁决明细。
6. 先处理取消键，再执行到期步骤，保证取消帧不会继续执行后续步骤。
7. 按步骤 `GapFrames` 调度后续步骤；`GapFrames=0` 时在同一帧顺序执行完零间隔链。
8. 输出已执行步骤、下一未完成步骤的总延时/剩余延时、取消记录、条件快照和快照哈希。

样本真值由生成器在构造场景时同步给出，包含期望触发帧、步骤 ID、步骤帧、间隔、完成/取消状态和压制关系；`VerifyTruth` 逐项校验。

## 冲突裁决

步骤在相同 lane（未指定 lane 时回退到 `action:<action>`）且半开时间区间重叠时才视为步骤冲突；同帧触发但 lane/时间不冲突的宏仍可一起执行。

默认 `ArbitrationDeterministic` 的排序键为：

1. 显式或默认有效优先级，数值大者胜；
2. 条件节点数（specificity），复杂者胜；
3. 稳定分支 ID 字典序。

`Adjudication` 记录触发帧、胜方宏/分支、被压制宏/分支和可解释 `Basis`，例如 `explicit_priority:20>5` 或 `stable_id:p_high<p_low`。

设置 `ArbitrationStrict` 后，若冲突边两端有效优先级相同，直接返回 `E_CONFLICT`，错误的 `ConflictSet` 为冲突连通集合，不依赖 map 顺序。

## 依赖图与增量

`BuildDependencyGraph` 显式建立以下节点和边：

- `key:<key>` / `flag:<flag>` → `condition:<branch>`
- `result:<predecessor>` → `condition:<downstream>`
- `condition:<branch>` → `step:<branch>:0` → 后续步骤
- `delay:<branch>` → 首个步骤和 `result:<branch>`
- `gap:<branch>:<stepIndex>` → 该步骤及后续步骤
- 最后步骤 → `result:<branch>`
- `result:<branch>` → 引用它的下游条件

修改条件、分支延时或步骤间隔时，`Invalidate` 从参数节点做反向依赖闭包，输出 `InvalidationReport`，其中含分支 ID、步骤 ID、节点 ID 和失效原因。未出现在报告中的分支不会从 `ResultCache` 删除；SQLite `result_cache` 表可跨进程重启复用。

`IncrementalRunner.Recompute` 会做一次确定性对账回放：未失效分支命中并校验持久化哈希，受影响分支重新写入缓存。测试断言增量路径与编辑后从空缓存全量回放的 `Commits` 序列完全相等。

## 三条执行路径

- 一次性全量重放：`RunProject`
- 逻辑实时逐帧：`NewRealtimeSession` + `Tick`，或 `RunRealtime`
- 断点恢复：`RunCheckpointed` 每 N 帧写 SQLite 窗口；可用 `stopAfterWindow` 模拟进程停止，重新打开数据库后从 `ExecutionState.NextFrame` 继续

`CompareResults` / `CompareTraces` 按帧比较触发分支、执行步骤、剩余延时、取消、裁决、消费偏移、条件快照和哈希。首个差异返回 `E_DIFF`，包含 `Frame`、`BranchID` 和 `Field`（如 `frames[4].executed_steps[0].action`）。

## SQLite 检查点

`frame_windows` 表保存：

- 项目和窗口索引、起止帧
- `consumed_offset`
- `format_no`、`format_version`
- `branch_results_json`
- `condition_snapshots_json`
- `execution_state_json`
- 对上述规范化 JSON 计算的 `content_hash`
- `status`、`error_code`、`error_message`、`attempt`

`SQLiteStore` 使用 `sync.Mutex` 且 `SetMaxOpenConns(1)`；成功窗口在单个 SQL 事务内 upsert，因此串行且不会交错。加载快照时逐行校验格式版本和内容哈希；任一行不匹配就删除该项目的全部窗口并返回 `E_VERSION` 或 `E_CORRUPT`。

注入窗口失败时，该窗口写入 `status='failed'` 和错误信息，已提交的其他窗口保持不变；再次调用会从最后一个连续的已提交窗口恢复，成功后同一主键转为 `committed`。

## 常用命令

```powershell
go env -w GOFLAGS=-mod=mod
$env:GOPROXY = "off"   # 仅在已准备好本地模块缓存时使用
go test ./...
go run ./cmd/macrotool generate
go run ./cmd/macrotool verify
go run ./cmd/macrotool inject -case all
```

也可直接运行仓库中的 `scripts/verify.ps1` 或 `scripts/verify.sh`。
