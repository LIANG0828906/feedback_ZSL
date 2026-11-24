# 固定点经济与资源调度内核

这是一个用 Go 1.22 从零实现的模拟经营游戏经济内核。它把三种经济规则工程格式统一加载为内部模型，以整型定点逐周期结算，支持精确的上下游缓存失效、增量/全量一致性比对，以及基于 SQLite 的原子窗口检查点和重启续跑。

仓库自带固定种子样本、三版本工程、逐周期真值、集成示例和端到端验收报告。

## 快速开始

```bash
go version                 # go1.22.12
go mod download
go build ./...
CGO_ENABLED=0 go test ./...
go test -race ./...

go run ./cmd/econctl demo --dir artifacts/demo --seed 42
```

`demo` 会重新生成 `artifacts/demo` 并写入 `validation-report.json`。当前固定种子 42 的实际结果：

- 20 项检查全部通过。
- v1/v2/v3 三个格式加载后均与 12 周期真值逐项相等。
- v3 → v1 → v3 无损往返后周期跨度、初始存量、节点速率和配方严格相等，3 条升级痕迹序列一致。
- 速率编辑只失效 `forest → sawmill → workshop`，独立支链 `farm`、`bakery` 的缓存被复用；增量运行复用 16 个节点周期、重算 36 个节点周期，并与空缓存全量结果逐项相等。
- SQLite 窗口提交在取消、失败和重启场景下只认可已持久化且版本匹配的连续前缀。
- 实测错误码：`E_VER`、`E_SCHEMA`、`E_LOSSY`、`E_OVF`、`E_DIFF`、`E_UNIT`、`E_RES`。

完整机器可读结果：`artifacts/demo/validation-report.json`。样本真值：`artifacts/demo/samples/truth.json`。

## 目录

| 路径 | 职责 |
| --- | --- |
| `fixed/` | 溢出检查、取整模式、周期与定点刻度换算 |
| `model/` | 统一领域模型、表达式树、依赖图、校验、内容哈希 |
| `format/` | v1/v2/v3 加载、导出、升级痕迹与降级损失清单 |
| `engine/` | 逐周期结算、意图、缓存失效、增量/全量比对 |
| `checkpoint/` | SQLite 窗口检查点、失败隔离、全局版本识别 |
| `runner/` | 窗口编排、暂停/取消、重启续跑与报告 |
| `sample/` | 固定种子工程生成器与真值 |
| `demo/` | 端到端验收流程 |
| `cmd/econctl/` | 命令行工具 |
| `examples/integrate/` | 组件嵌入示例 |

## 三个外部格式

同一经济模型可无损表达为三个版本：

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 版本字段 | `schema_version: economy-v1` | `formatVersion: v2` | `specVersion: v3` |
| 命名 | snake_case | camelCase | 分组对象 + camelCase |
| 周期 | tick，250ms/tick | 毫秒 | tick |
| 速率/工人刻度 | 千分比 | 万分比 | 万分比 |
| 产出公式 | 扁平常量 `rate_permille` | 表达式树 `productionFormula` | 表达式树 `rateFormula` |
| 资源类型 | 字符串 `kind` | 整数 `resourceType` | 对象 `resourceType.code/category` |
| 可选字段 | 无 | `workforce.maximum` | `storage.overflowPolicy`、`upgradeDiscountBasisPoints` |

加载器保留 `SourceVersion`，并把字段名、资源类型、公式、周期和刻度全部规范化为内部模型。未知版本返回 `E_VER` 和版本字段路径；必填字段缺失返回 `E_SCHEMA` 和精确 JSON Pointer，例如 `/window_count`、`/project_name`。

### 内部单位

- 逻辑周期跨度：毫秒整数 `CycleMs`。
- 批次速率：`10000 = 1 batch/cycle`。
- 工人效率：`10000 = 1.0 个名义工人产量`。
- 周期快照净速率：资源单位/周期 × `10000`。
- 所有换算只使用 `int64`、`math/big` 中间量和声明取整方向 `floor`/`ceil`/`nearest`；没有浮点累积。

### 升级与损失

升级链固定为 `v1 → v2 → v3`。新增字段按声明默认值补齐，每条痕迹包含路径、旧值、默认值、默认来源、转换规则版本和起止版本。无损降级后的再次升级会产生相同顺序的痕迹。

严格导出在目标无法表达语义时返回 `E_LOSSY` 和完整损失清单。调用方可用 `AllowLossy` 生成可重新加载的降级产物；处置方式会记录为：

- `rounded_floor`/`rounded_ceil`/`rounded_nearest`
- `flattened_into_upgrade_costs`
- `flattened_at_export_worker_count`
- `forced_cap_drop`
- `dropped`

降级仍保证节点速率与配方在目标格式可表达范围内被明确保留或记录损失，不会静默丢弃。

## 结算语义

每个逻辑周期按依赖拓扑序执行：

1. 应用该周期全部玩家意图，升级立即切换速率与容量。
2. 基于本次运行时实际选中的配方重新确定当前已建造节点的拓扑序，再按 `rate × workers × workerEfficiency` 累积 1e8 刻度的批进度。
3. 仅在整批输入满足时消费输入并产出整批资源。
4. `cap_drop` 策略按容量截断并记录来源节点；`reject` 策略在容量不足时停滞而非丢弃。
5. 输入不足或工人数为零时停滞，并保留首次停滞周期。
6. 输出各节点资源视图、产消量、净速率、溢出、全局存量、全局总量和闲置工人数。

节点输出是整批资源，跨周期的小数批进度保存在 `Progress` 中并以整数推进，因此不会出现浮点漂移。

窗口号、窗口内周期和逻辑周期号均从 0 开始；样本真值的 `keyCycles: [3,7,11]` 表示三个窗口的最后一个周期。

## 增量缓存

缓存键为 `(nodeId, cycle)`，缓存项包含配置签名、批进度、消费量、毛产出量和停滞状态。速率、配方或工人数编辑时：

- 记录编辑节点及传递下游的失效原因。
- 配方编辑只更新该节点关联的依赖边，不重建无关图部分。
- 未失效节点直接复用缓存。
- 若共享资源竞争改变了输入前置条件，缓存项会安全检查并回退重算。
- 全部缓存先在暂存表中构建，成功后才一次性替换；失败运行不会部分污染旧缓存。

`RunFull` 从空缓存计算，`RunIncremental` 复用缓存。`CompareRuns` 依次比较逐周期快照、溢出归集和节点提交序列，并在首个差异返回 `E_DIFF`，包含周期、窗口、节点和字段名。

## SQLite 检查点

检查点保存在纯 Go SQLite 驱动 `modernc.org/sqlite v1.29.10` 中。

每个窗口记录：

- 已消费意图偏移
- 下一周期与完整节点运行状态
- 内容 SHA-256
- 源格式版本
- 转换规则版本
- 算法版本
- 窗口结果快照

写入由进程内互斥和单连接串行化，并通过事务提交；窗口号必须连续。读取时只要发现任一指纹字段不同、内容不同、窗口缺口，或恢复状态与窗口边界不一致，就原子删除全部检查点和失败记录，要求从空状态全量结算，禁止混合旧窗口。

失败窗口写入独立 `window_failures` 表，不推进检查点。重试成功后删除该失败记录。用户取消时 Runner 丢弃当前窗口的内存副本，只保留已提交窗口。进程被强制终止后，重启只能从数据库中连续且版本匹配的窗口恢复；报告只有在全部窗口都提交后才设置 `complete: true`。

## 命令行

```bash
# 端到端验收
go run ./cmd/econctl demo --dir artifacts/demo --seed 42

# 仅生成三版本样本和真值
go run ./cmd/econctl gen --out artifacts/samples --seed 42

# 加载/校验
go run ./cmd/econctl validate --in artifacts/demo/samples/v3/project.json

# 显式转换；严格模式遇到损失会返回 E_LOSSY
go run ./cmd/econctl convert \
  --in artifacts/demo/samples/v3/project.json \
  --to v1 --out artifacts/demo/converted-v1.json

# 使用 SQLite 窗口执行或续跑
go run ./cmd/econctl run \
  --in artifacts/demo/samples/v2/project.json \
  --db artifacts/demo/run.db \
  --out artifacts/demo/run-report.json
```

## 嵌入示例

```go
m, err := format.LoadFile("project.json")
eng, err := engine.New(m, engine.Options{})
baseline, err := eng.RunFull(ctx)

report, err := eng.EditRate("forest", 15000)
incremental, err := eng.RunIncremental(ctx)

fresh, _ := engine.New(eng.Model(), engine.Options{})
full, _ := fresh.RunFull(ctx)
if err := engine.CompareRuns(incremental, full); err != nil {
    // E_DIFF: 周期、节点、字段和首个不同值在 model.Error.Diff 中
}

store, _ := checkpoint.Open("state.db")
r, _ := runner.New(m, store, runner.Options{})
final, err := r.RunAll(ctx)
```

可运行版本：

```bash
go run ./examples/integrate \
  --project artifacts/demo/samples/v2/project.json \
  --db artifacts/integrate.db
```

## 测试覆盖

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go test ./...
```

自动化覆盖包括：

- 三版本加载、内容哈希一致、源版本保留
- 未知版本/缺失字段路径与 `E_VER`/`E_SCHEMA`
- 千分比/万分比、tick/毫秒、取整方向与 `E_OVF`
- v3→v1→v3 严格往返和升级痕迹一致性
- 严格/允许损失降级及产物回读
- 溢出截断、`reject` 策略、停滞起始周期
- 同周期升级速率/容量生效
- 精确失效传播、缓存复用和失败时原子缓存交换
- 增量/全量逐项相等及 `E_DIFF` 定位
- SQLite 顺序提交、重启续跑、取消、失败隔离、重试和全局版本作废
- 固定种子样本字节级确定性

## 约束与边界

- 严格无损往返要求数值在目标刻度可表达；例如 v2 的 `10001` 万分比无法精确写成 v1 千分比，此时必须显式选择取整并接受 `E_LOSSY` 记录。
- v1 固定使用 250ms/tick；无法整除的周期在严格模式产生损失，在允许损失模式按声明方向取整。
- 内容相同但源格式版本不同仍视为不同检查点身份，这是“整体版本识别”的显式要求。
- 本仓库不使用容器、外部数据库服务、网络账号或远端真值。
- `E_RES` 用于资源消耗型玩家意图（建造/升级成本）在执行时库存不足；普通生产批次的输入不足按结算规则进入停滞，而不是让整个窗口失败。
