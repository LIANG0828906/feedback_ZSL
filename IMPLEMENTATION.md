# 技能资源预算与冷却排程实现说明

模块 `skillbudget` 只使用 Go 标准库。领域组件位于 `budget/`，命令行与本地诊断服务位于 `cmd/skillbudget/`。

## 快速验证

```bash
go test ./... -count=1
go test -race ./budget -count=1
go run . demo
go run ./cmd/skillbudget demo
go run ./cmd/skillbudget serve -dir demo-out -addr 127.0.0.1:8080
```

模块根目录和 `cmd/skillbudget` 提供等价入口；前者可直接使用 `go run . demo`。

演示生成：

- `demo-out/rules-v1.json`：snake_case、千分比、tick、扁平常量表；共享组通过 `cooldown_groups[].member_skill_ids` 表达，技能上没有共享组字段。
- `demo-out/rules-v2.json`：camelCase、万分比/BP、毫秒、扁平常量表，技能带 `sharedCooldownGroupCode`。
- `demo-out/rules-v3.json`：kebab-case、myriad/万分比、毫秒、表达式树，技能带 `shared-cooldown-group`。
- `demo-out/checkpoints/`：逐窗口原子检查点。
- `demo-out/cache/result-cache.json`：节点缓存键与逐 tick 持久化结果。
- `demo-out/diagnostics.html` 与 `index.html`：本地诊断页。
- `demo-out/demo-result.json`：三版本、三路径、失效计划、预算和故障探针的结构化结果。

## 精度口径

- 内部资源单位是 permyriad/BP，分母 `10000`；v1 permille 乘 `10` 转 BP。
- 内部时间单位是 tick；固定样本的 `tick_duration_ms=100`。
- 降分辨率换算保留源刻度残差。例如 `4234‱ -> 423‰` 得到低值 `423` 与残差 `4‱`，反向恢复为 `423*10+4=4234‱`。
- 所有换算均为整型运算，没有浮点累积。int64 乘法/加法溢出返回 `E_OVF`，错误中包含字段路径和两个操作数。
- 毫秒到 tick 支持文档声明的 `floor_toward_negative_infinity`、`ceil_toward_positive_infinity`、`trunc_toward_zero`、`nearest_half_up` 与 `exact_zero_residual`。

## 表达式与冷却

表达式节点支持 `lit`、`var`、`add`、`sub`、`mul`、`div`、`min`、`max`、`neg`。算术边界是 int64 上下界；溢出返回 `E_OVF`。除法默认向零截断，也可由节点 `rounding` 声明其他方向。除零返回 `E_EVAL`，并给出节点 ID 与实参。负资源回复返回 `E_EVAL`。

CDR 系数使用 BP，合法区间 `[0,10000]`。有效冷却为：

```text
effective = base - floor(base * cdr_bp / 10000)
```

因此有效冷却一定处于 `[0,base]`；逐 tick 递减最低到零，不会为负或超过声明上界。

扁平常量表与表达式树由 `CompareFlatAndTree` 按节点 ID 字典序比较，首个分歧节点会同时出现在返回对象和 `E_DIFF`/`E_EVAL` 错误中。v1/v2 扁平装载与 v3 表达式树装载后的逐 tick 报告均与同一份解析真值比较。

## 排程与确定性仲裁

每个 tick：

1. 回复资源并截断到上限；
2. 所有技能冷却和共享组冷却减 1，最低为 0；
3. 按显式 `priority` 升序、源意图 `ordinal` 升序、技能 ID、意图 ID 建立全序；
4. 依次检查技能冷却、共享组冷却、资源余额；
5. 接受则扣资源、写入有效冷却和共享组冷却；拒绝则记录原因；
6. 输出 tick 末各技能可用状态、资源余量、冷却剩余、组冷却、施放和依赖。

仲裁记录包含 `tick`、`skill_id`、`contention`、`decision`、`reason` 与 `basis`。默认全序不依赖 Go map 遍历。严格优先级模式下，若同 tick 出现相同显式优先级，返回 `E_ARB` 并附带排序后的争用技能集合。

## 依赖图与缓存

显式节点包括资源上限/初值、资源回复表达式、技能优先级/消耗/冷却、CDR 表达式、共享冷却组和 `tick:<n>`。每个 tick 的 `dependencies` 是实际读取的节点集合，且 `tick:n` 依赖 `tick:n-1`。

缓存键为：

```text
sha256(
  "cache-key",
  param_version,
  node_id,
  scoped_rule_content_hash,
  sorted(dep_id + "=" + upstream_cache_key)
)
```

失效分析沿反向边传播：

- 修改 `fireball` 冷却：样本中 tick 1 首次施放时读取该节点，因此失效区间为 `[1,11]`，tick 0 从持久化缓存复用。
- 修改没有意图引用的 `unused_ward` 冷却：规则节点改变，但没有 tick 依赖它，12 个 tick 全部复用。
- 修改资源回复：tick 0 起每个 tick 都读取该表达式，因此 `[0,11]` 全部重算。

失效清单包含变更节点、传播节点、tick 列表和合并后的 tick 区间。

## 原子检查点与三路径

检查点位于输出目录的 `checkpoints/window-000000.json` 等文件。写入流程为：同目录临时文件 → 写入 → `Sync` → 关闭 → `os.Rename`。`CheckpointStore` 互斥锁和包级原子写锁保证串行；重命名后读者只能看到完整旧文件或完整新文件。

检查点包含：

- 窗口序号、首/末 tick；
- 当前资源；
- 各技能冷却状态；
- 共享组冷却状态；
- 已消费意图偏移；
- 规则内容哈希；
- 源版本标识；
- 算法版本与参数版本；
- 窗口内完整 tick 状态和仲裁明细。

恢复只接受算法版本、参数版本、内容哈希和源版本均匹配的检查点。损坏 JSON 返回 `E_DECODE`，版本/哈希不匹配返回 `E_SCHEMA`。

三条执行路径是：

1. `RunRealtime`：实时逐 tick 结算并逐窗口提交；
2. `RunFirstWindows` + `ResumeFromStore`：提交部分窗口后模拟取消/强杀，再从兼容检查点恢复；
3. 空目录 `RunRealtime`：一次性全量重算并提交全部窗口。

`CompareReports` 依次比较内容哈希、每个 tick 的资源/偏移/技能状态/组状态/施放/仲裁以及窗口提交序号和区间；首个不一致会返回带 `tick` 与 `field/path` 的 `E_DIFF`。

`RunRealtimeContext` 支持用户取消：取消后在当前已提交窗口之后的下一窗口边界返回 `context.Canceled`。进程被强杀时不承诺写出退出报告，重启只依据已经原子重命名且版本匹配的窗口。

## 诊断页面

`budget.RenderDiagnosticsHTML` 使用 `html/template` 转义并展示：

- 预算占用和各技能累计消耗；
- 逐 tick 资源、可用状态、技能冷却和共享组冷却；
- 全部仲裁明细及依据；
- 失效节点、tick 集合与区间；
- 窗口提交序列。

`skillbudget serve` 使用标准库 `net/http` 提供静态诊断目录，不访问外部网络。

## 实测记录

环境：Linux/WSL，Go 1.22.12。

```text
$ go test ./... -count=1
ok  	skillbudget/budget
?   	skillbudget/cmd/skillbudget	[no test files]

$ go test -race ./budget -count=1
ok  	skillbudget/budget

$ go test ./budget -cover
ok  	skillbudget/budget	coverage: 77.5% of statements
```

演示实际输出确认：

- v1/v2/v3 均加载 12 个 tick 和 12 条意图，语义哈希一致，逐 tick 匹配真值；
- v1 产生 18 条字段转换痕迹，v2 产生 6 条，v3 产生 5 条；
- 定点往返 `4234‱ -> 423‰ residual=4 -> 4234‱`；
- 实时、断点恢复、空缓存全量重算三条路径一致，默认窗口大小 4 时提交 3 个窗口；
- 修改 `fireball` 冷却后复用 `[0]`、重算 `[1..11]`，增量结果与独立全量参考结算一致；
- `E_DECODE`、`E_ORDER`、`E_SCALE`、`E_STAGE`、`E_ARB`、`E_DIFF`、`E_OVF`、`E_EVAL` 探针全部通过；
- 本地 HTTP 页面包含“预算占用 / 逐 Tick 状态 / 裁决明细 / 精确失效清单”。
