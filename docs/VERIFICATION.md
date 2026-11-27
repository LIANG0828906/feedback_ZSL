# 验证结果记录

环境：Linux（WSL Ubuntu），Go 1.22.12，纯 Go SQLite 驱动
`modernc.org/sqlite v1.34.4`（经 `proxy.golang.org` 预取，无外部数据库服务）。

## 1. 自动化测试

执行 `go test ./... -count=1`，全部包通过：

| 包 | 结果 | 覆盖率 | 关键验证点 |
| --- | --- | --- | --- |
| `internal/fixed` | ok | 57.6% | 千分比/万分比、tick/毫秒往返、截断残差、E_OVF 双操作数、负数开方 |
| `internal/expr` | ok | 48.3% | 树/扁平等价、折叠往返、除零/负开方 E_EVAL、非线性 E_LOSSY |
| `internal/schema` | ok | 75.7% | 三版本装载、字段路径 E_SCHEMA、升级默认补齐、降级损失、再装载复原 |
| `internal/gen` | ok | 76.6% | 固定种子确定性、三版本 wire、v3 无损/v1 有损 |
| `internal/engine` | ok | 64.6% | 与真值逐 tick 一致、树/扁平等价、E_CONFLICT 回滚、状态机 E_STATE、三路径 |
| `internal/depgraph` | ok | 62.9% | 缓存键稳定、按 tick 区间精确失效、未失效区间复用、共享组传播 |
| `internal/checkpoint` | ok | 66.9% | 事务提交、重启加载、版本/哈希不匹配 E_ENTRY、窗口乱序 E_STATE |
| `internal/pool` | ok | 89.1% | 并行度/缓冲上限、阻塞排队原因、按提交序号排序、单任务失败隔离 |
| `internal/integration` | ok | 43.6% | 跨版本装载 + 三路径逐 tick 一致 + 断点恢复端到端 |

并发相关包通过竞态检测：

```
go test -race ./internal/pool/ ./internal/engine/ ./internal/integration/
# 全部 ok，无 data race
```

## 2. 验收项对照

- 固定种子（`20240517`）跨版本装载：v2/v3 装载结果与生成器真值逐 tick 一致；
  v1 因 schema 本身无共享组字段，验证其损失被显式 E_LOSSY/默认补齐记录而非静默。
- 逐 tick 排程：`RunFull` 与生成器参考模拟在可用状态、资源余量、冷却剩余、
  裁决明细上完全一致（见 `engine_test.go` 的 `TestFullMatchesGeneratorTruth`）。
- 三路径一致：全量、实时、断点恢复、并发受限预算路径经 `engine.Compare`
  自动比对，首个不一致定位到 tick/技能/字段；当前样本全部一致。
- 错误码：E_SCHEMA、E_LOSSY、E_OVF、E_EVAL、E_CONFLICT、E_STATE、E_ENTRY
  均有针对性单测断言。
- 断点恢复：提交前半窗口后从 SQLite 检查点续算，与全量对应区间逐 tick 相同；
  版本/哈希不匹配时整窗作废。

## 3. 工作台手工验证

```
go build -o /tmp/skillsched ./cmd/skillsched
/tmp/skillsched serve -addr 127.0.0.1:18081 -dbdir /tmp/sched-db
```

对以下路由发起 HTTP 请求均返回 200 并正确渲染：

| 路由 | 内容 |
| --- | --- |
| `/` | 场景总览：池、技能（tick/万分比）、共享组 |
| `/ticks?tick=N` | 单 tick 池余量、技能状态、冷却剩余、裁决明细 |
| `/compare` | 全量/树/并发三路径“逐 tick 一致”，线程池排队原因 |
| `/invalidate` | 编辑 `cdr:skill_1` 后 tick 10-19 的精确失效清单与缓存复用 |
| `/checkpoint` | 5 个窗口原子串行提交并重启加载，展示算法版本与内容哈希 |
| `/api/truth` | 生成器逐 tick 真值 JSON |

## 4. 命令行自检

```
$ go run ./cmd/skillsched demo
场景=cli-demo 种子=20240517 技能=6 池=3 组=2 意图=45 tick=40
与生成器真值: 逐 tick 一致
表达式树 vs 常量表: 求值一致
全量 vs 并发受限预算: 逐 tick 一致
```
