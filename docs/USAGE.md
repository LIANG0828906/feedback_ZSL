# skillsched — 战斗技能资源预算与冷却排程

可集成的 Go 领域组件 + `net/http` 本地诊断工作台。规则工程、施放意图与逐 tick
真值均由仓库内固定种子确定性生成器产出，不依赖真实战斗服或线上资源。

## 快速开始

```bash
go mod download
go test ./...              # 全量单元与集成测试
go run ./cmd/skillsched demo    # 命令行打印三路径一致性摘要
go run ./cmd/skillsched serve -addr 127.0.0.1:8080 -dbdir .
```

工作台页面：

- `/` 场景总览（资源池、技能、共享组）
- `/ticks?tick=N` 逐 tick 诊断（可用状态、余量、冷却剩余、裁决）
- `/compare` 全量 / 表达式树 / 并发三路径比对与线程池排队诊断
- `/invalidate` 依赖图编辑后的精确 tick 区间失效
- `/checkpoint` SQLite 原子窗口检查点与重启版本校验
- `/api/truth` 生成器逐 tick 真值 JSON

## 内部口径

- 时间统一为逻辑 tick（100ms/tick），比例统一为万分比，全部 `int64`，禁止浮点。
- 同一规则工程以 v1/v2/v3 三个 schema 描述，差异覆盖字段命名、定点刻度、时间单位、
  表达式形态与共享组字段。

## 文档

- 设计与口径：`docs/DESIGN.md`
- 验证结果：`docs/VERIFICATION.md`

## 代码地图

```
internal/
  errx/        结构化错误码（E_SCHEMA/E_LOSSY/E_OVF/E_EVAL/E_CONFLICT/E_STATE/E_ENTRY/...）
  fixed/       整型定点与时间换算、残差、溢出、开方
  expr/        表达式树与扁平常量表、整型求值
  model/       内部统一规则模型与内容哈希
  schema/      v1/v2/v3 装载、升降级、转换痕迹与损失报告
  gen/         固定种子生成器与独立参考模拟真值
  engine/      逐 tick 排程、争用裁决、双层状态机、三路径调度
  depgraph/    依赖图、组合缓存键、精确失效传播
  checkpoint/  database/sql + modernc.org/sqlite 原子检查点
  pool/        受限预算并发池与排队/异常隔离诊断
  web/         net/http 服务端渲染工作台
cmd/skillsched/ CLI 入口
```
