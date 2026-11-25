# 关卡声学领域组件技术说明

## 1. 组成

- `pkg/fixedpoint`：`1e6` 标度的定点二维向量、整数平方根距离、有理交点取整。
- `pkg/acoustics`：工程 DTO、`E_SCHEMA/E_GEOM/E_STATE/E_UNREACH/E_DIFF`、校验、门洞图、房间链、镜像反射、SPL、状态机、有界工作池、依赖图、失效规划和报告比对。
- `pkg/generator`：固定种子 `250250` 的确定性样本与独立浮点解析真值。
- `pkg/checkpoint`：`modernc.org/sqlite` 检查点库。
- `pkg/engine`：稳定房间序、状态迁移、并行听者/声源对求解、失败隔离、重试和提交。
- `pkg/pipeline`：样本生成、真值校验、增量预览、中断恢复、一次性全量重算的三路径验收。
- `pkg/server` 与 `pkg/server/web/index.html`：仅依赖本地 JSON API 和原生 SVG/JS 的诊断页面。
- `cmd/acoustics`：`generate`、`verify`、`serve` 命令。

## 2. 几何与声学口径

坐标对外使用米制 JSON，进入核心后转换为标度 `1_000_000` 的 `int64` 定点值。样本坐标范围很小，距离平方保持在 `int64` 安全范围内；校验器把输入坐标限制在 `±1000 m`。

房间链采用显式、可复现的口径：

1. 以房间为节点、开启的门洞为边；
2. 先取最少房间跳转数；
3. 对同等跳转数的房间链，用定点整数计算 `声源 → 门洞中心序列 → 听者` 的折线长度；
4. 按长度、房间 ID、门洞 ID 做确定性排序；
5. 利用矩形凸性校验相邻门洞/点之间的线段始终位于对应房间内，因此不会穿越墙体。

跨房间反射以进入听者房间的最后一个门洞中心作为等效输入点；链前缀长度为声源到该门洞的定点长度。总反射路径为：

```text
prefix(source -> last_door) + |input -> reflection_point| + |reflection_point -> listener|
```

镜像交点使用 `math/big.Rat` 求解，再舍入为定点坐标；只有交点参数位于反射线段有效区间内才计为有效路径。路径长度和向量点积/模长使用整数运算，入射角量化为微弧度并在报告中同时输出角度值。

单条反射声压级：

```text
L_i = PowerDB - 20 log10(max(path_m, 1e-6)) + 10 log10(1 - absorption)
```

同一听者的多条反射按非相干能量求和：

```text
L_total = 10 log10(sum_i 10^(L_i/10))
```

## 3. 状态与提交边界

房间状态：`pending -> connecting -> reflecting -> committed`；任一活动阶段可进入 `failed`，失败房间经 `failed -> pending` 重试。已提交房间只能通过显式失效进入 `pending`，旧快照不会被原地修改。

正常窗口按房间 ID 字典序推进。每个房间在一个 SQLite 事务中写入：

- 反射面快照；
- 门洞快照；
- 连通链摘要；
- 听者求解结果；
- 房间内容哈希；
- 参数版本与算法版本。

`Store` 使用互斥锁和 `SetMaxOpenConns(1)` 保证写事务不交错；`commit_log` 保存实际提交顺序。

## 4. 精确失效

`PlanInvalidation` 比较两个修订：

- 反射面 canonical hash 变化：只标记使用该反射路径的 pair/listener；
- 门洞增删或门洞快照变化：标记所属墙两侧房间；
- 所有声源—听者链重新计算签名，比较房间链、门洞链和定点长度；
- 链签名变化时标记旧/新链上的房间、链对象和下游 listener；
- 未进入失效集合的房间由引擎从旧报告/SQLite 载荷恢复，并在新窗口中复用。

`DependencyGraph` 显式输出 `surface -> listener`、`door -> chain`、`chain -> listener` 和 `room -> surface` 边。

## 5. 恢复语义

打开同一 SQLite 文件时，只有窗口全局内容哈希、参数版本、算法版本和房间顺序全部匹配，才恢复 `status='committed'` 的房间。任一签名不匹配会在同一初始化事务中清空旧窗口检查点和提交日志，随后按房间序全量求解，避免新旧结论混用。

模拟中断只在房间事务成功后发生，因此已提交行一定完整；事务进行中的强制进程终止由 SQLite 原子事务回滚，但本组件不承诺在任意强杀后直接生成完整报告，重新运行即可恢复。

## 6. 并发边界

`acoustics.Pool` 启动固定数量 worker，并为每个 worker 设置容量为 `bufferPerWorker` 的 channel。所有缓冲已满时，提交方阻塞；同时记录 `buffer_full` 队列事件和任务 ID，不丢弃任务。单个 pair 任务的 panic 会被 worker 捕获并转为错误，房间据此进入 `failed`，其他房间继续执行。

本地服务使用 `max_workers=2, buffer_per_worker=1`，因此连续编辑时可在诊断面板看到真实背压记录；验收流水线使用更大的缓冲以减少计时敏感性。
