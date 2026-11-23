# 游戏对话无障碍呈现编排应用 — 技术说明

模块 `orchestrator`（Go 1.22+，唯一第三方依赖 `modernc.org/sqlite`，纯 Go 实现，
Windows 上可离线交叉编译：`GOOS=windows go build ./...`，无需容器）。

## 目录结构

- `model.go` — 工程/对白/策略/真值数据模型，内容哈希，清单读写
- `errors.go` — 结构化错误码：E_REF / E_RHY / E_PARA / E_CUT / E_QUEUE / E_STATE / E_DIFF
- `generator.go` — 固定种子（`DefaultSeed=20240607`）确定性样本与真值生成器
- `validate.go` — 工程加载与结构校验（需求 1）
- `schedule.go` — 节奏权重驱动的字符级呈现调度（需求 2）
- `interrupt.go` — 打断收束与续讲恢复（需求 3）
- `queue.go` — 跨对白呈现队列与打断优先级（需求 4）
- `state.go` — 逐对白编排状态机（需求 5）
- `orchestrator.go` — 编排管线、增量/全量重算、并发资源边界（需求 5/6/8）
- `checkpoint.go` — SQLite 检查点、版本识别、三路径运行器（需求 7）
- `verify.go` — 独立结果验证工具（需求 8）
- `cmd/orchestrator/main.go` — CLI：`gen` / `run` / `verify`

## 核心设计

**节奏调度**：节奏权重按 `1<<20` 定点化，呈现窗口（手动区间优先，否则语音片段
起止）按权重累积整数取整切分：`end_i = start + cum_i * duration / total`。
字符区间天然首尾相接、无重叠无空洞。权重和为零或定点换算溢出返回 `E_RHY`
并携带对白标识与溢出位置。

**打断收束**：打断点 `t` 处已完整呈现的字符数保存为进度；尾句（含当前字符的
句子剩余部分）必须在收束限定采样点内播完，`cutoffEnd = t + 尾句时长`，超限返回
`E_CUT`（Expected=所需时长，Actual=允许限定）。续讲位置为尾句之后第一个字符，
`ResumeStart == cutoffEnd`，续讲视图不重复推进任何已呈现字符，总呈现终点与
未打断调度一致。

**队列**：队列顺序为策略声明的对白 ID 排列，重复/悬空/缺失返回 `E_QUEUE` 并
指出冲突位置。打断只作用于当前对白，续讲发生在原队列位置而非队尾。

**状态机**：`pending → orchestrating → submitted / interrupted / failed`，
`interrupted → resume_pending → orchestrating`，`failed → orchestrating`（重试）。
非法迁移返回 `E_STATE` 且状态与既有结果不变；已提交对白锁定，拒绝编辑。

**增量重算**：编辑未提交对白后，仅失效该对白及其队列位置之后的未提交对白
（含续讲待办链），其余复用持久化结果并输出含失效原因与复用状态的清单；
与空缓存全量重算逐字符比对，不一致返回 `E_DIFF` 定位首个对白与字段。

**检查点**：每条对白提交后在单事务内原子写入 `results` 与 `commit_log`，
互斥锁串行化、提交顺序稳定；`meta` 记录内容哈希、策略版本与算法版本，
重启时任一不匹配即整体作废快照（禁止部分混用）并全量重算。实时编排
（`RunLive`）、断点恢复（`RunResumable`+`ContinueFromCheckpoint`）与一次性
全量（`RunBatch`）三路径产出逐字符一致的结果与提交序列。

**独立验证**：仅从持久化结果与工程清单出发，重校验字符区间拼接、收束限定、
续讲可达性与队列顺序，并与主链路重算结果逐项比对，输出首个违规对白与违规项。

**并发边界**：工作池上限 `MaxWorkers`，每线程文本/采样缓冲上限
（`MaxCharsPerDialogue` / `MaxSamplesPerDialogue`，超限 `E_PARA`）；计算并行、
提交严格按队列顺序串行，并行与串行产出相同结果与提交序列（`-race` 通过）。

## 本地验证

```sh
go test -race -count=1 ./...                 # 24 项测试全部通过
go run ./cmd/orchestrator gen -dir samples -n 6
go run ./cmd/orchestrator run -dir samples -db tmp/och.db -workers 4
go run ./cmd/orchestrator verify -dir samples -db tmp/och.db   # VERIFY OK
```

实测结果（2026-11-24，Go 1.22.12）：24 项测试在 `-race` 下全部 PASS，
覆盖真值容差比对、E_REF/E_RHY/E_PARA/E_CUT/E_QUEUE/E_STATE/E_DIFF 注入、
三路径逐字符一致、快照版本失效、篡改检测、并行/串行等价与单条对白异常隔离重试。
