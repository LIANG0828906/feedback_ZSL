# 空间声学脉冲响应分析工作台 — 技术说明

## 1. 模块划分

| 包 | 职责 |
| --- | --- |
| `internal/generator` | 固定种子的确定性多通道 IR 生成、清单与解析真值、私有 `.sir` 二进制信号格式 |
| `internal/loader` | 本地样本加载、元数据校验（`E_META`）、参数越界校验（`E_PARA`）、通道缺失（`E_CHAN`） |
| `internal/analyzer` | 分帧、直达检峰、早期反射能量积分、指数尾音 log 线性拟合、帧级异常注入 |
| `internal/soundfield` | 通道间时差（ITD）、直达/混响能量比（DR）、可配置声道布局映射 |
| `internal/renderer` | 按帧拥有区间重建卷积事件与指数尾音、插值阶数残差、逐帧能量统计 |
| `internal/errorsx` | 四项逐帧误差、结构性上界推导与累积、`E_BOUND` 判定 |
| `internal/statemachine` | pending/estimated/committed/failed 四态合法迁移与提交冻结 |
| `internal/checkpoint` | SQLite 单连接串行事务检查点、哈希/版本失效与整体作废 |
| `internal/pipeline` | 并发资源池、三路径编排、暂停/重试、提交序列化 |
| `internal/compare` | 三路径逐样本比对，定位首个不一致采样/帧/字段（`E_DIVERGE`） |
| `internal/panel` | 不依赖 WebView 的 `net/http` 可视化面板（嵌入页面） |
| `pkg/workbench` | 对外稳定 API，CLI、面板、Wails 壳共用 |
| `desktop` | Wails v2 桌面工程（独立 module，`replace` 到核心模块） |

## 2. 确定性样本与真值

生成器使用 `math/rand.New(rand.NewSource(seed))`，同种子逐字节复现。每个样本的清单包含：
采样率、声道布局、长度、信号/干声文件引用、逐通道解析真值（直达样本点、反射到达点与能量、
尾音 onset/amplitude/rate）。真值只由解析构造写出，分析器从不回写真值。

信号为直达冲激 + 严格递增的离散反射 + 指数衰减尾音叠加，干声片段仅含直达冲激，
作为渲染阶段的卷积核。

## 3. 分帧分析与渲染边界

- 仅保留完整落在信号内的帧（`start+frameLength <= length`），不补零、不越界。
- 每帧检峰采用相对阈值 + 局部峰判定，首个峰为直达，其余为早期反射。
- 早期能量同时计算全帧口径与估计窗口径，二者之差用于截断误差量化。
- 渲染只写帧拥有区间 `[start,end)`；卷积跨帧尾巴在拥有区间外丢弃，
  合并阶段按固定帧序浮点相加，因此与并发执行顺序无关。
- 输出长度与原信号严格一致（测试断言）。

## 4. 误差传播报告

四项逐帧误差：

1. `peak_quantization`：检峰到达点与解析到达点的样本距离（仅直达所在帧定义）。
2. `energy_truncation`：落在估计窗外、帧内的解析反射能量。
3. `itd_estimation`：解析 ITD 与重建 ITD 之差（仅两声道直达同帧时定义）。
4. `render_interpolation`：插值阶数控件的核近似残差；整数延迟下结构性为 0。

结构性上界只由观测结构推导，不读真值、不读渲染结果：

| 指标 | 逐帧上界 | 累积算子 |
| --- | --- | --- |
| 峰值量化 | `0.5` 样本 | L∞ |
| 能量截断 | `frameLength`（满量程单点能量的结构和） | L1 |
| ITD | `1` 样本（两个量化到达点之差） | L∞ |
| 插值 | 阶数 0/1/3 → `0.5 / 0.25 / 0.0625` | L∞ |

`bound_scale` 是参数化收紧系数（诊断用），使上界低于实测误差时确定性触发 `E_BOUND`，
错误中带帧号与指标名。该机制由参数触发，不修改源码。

## 5. 状态机与检查点

合法迁移：

```
pending   -> estimated | failed
estimated -> committed | failed | pending
failed    -> pending
committed -> （终态，冻结）
```

SQLite 使用单连接（`SetMaxOpenConns(1)`）+ 每帧独立事务，写入天然串行、帧序稳定。
表结构为 `snapshot_meta`（输入哈希、算法版本、参数、总帧数）与 `frames`（帧号、状态、JSON 负载）。
重启时哈希或 `AlgorithmVersion` 不匹配即 `DELETE` 全表，全量重算，禁止部分混用旧帧。
输入哈希覆盖声学内容与元数据，但不含故障注入声明（故障是运行参数，解除注入重试不应作废快照）。

## 6. 异常注入与帧级隔离

全部异常由清单 `faults` 字段或运行参数驱动：

| 异常 | 错误码 | 触发方式 | 粒度 |
| --- | --- | --- | --- |
| 通道缺失 | `E_CHAN` | 信号文件不产出/布局映射目标缺失 | 样本/声道 |
| 反射倒置 | `E_ORDER` | `order_frame + order_channel` | 帧+声道 |
| 增益溢出 | `E_CLIP` | `clip_frame + clip_channel + clip_gain` 越过 ±1 | 帧+声道 |
| 坏帧 | `E_FRAME` | `bad_frames` 序号集合 | 帧 |
| 上界失效 | `E_BOUND` | `bound_scale < 1` 收紧结构上界 | 帧+指标 |

帧内异常只把该帧置 `failed`，其他帧继续估计与提交；失败帧可经 `RetryFailed` 回到
`pending` 后重新估计，已提交帧永不被改写。

## 7. 三条执行路径的一致性

- `realtime`：线程池分帧估计 → 帧序串行提交写 SQLite。
- `resume`：新引擎/新连接装载已提交冻结帧，仅处理剩余帧，提交序列仍按帧序。
- `replay`：只从 SQLite 读持久化参数与帧负载，合并出完整波形。

所有数值计算只依赖输入与参数，不依赖时间、goroutine 调度或内存地址；
`compare.Compare` 逐字段、逐采样比对，首个差异返回帧号、声道、样本下标与字段名，
由调用方包装为 `E_DIVERGE`。

## 8. 并发资源边界

`pipeline.Pool` 以带缓冲信号量限制最大并行度，以采样点预算限制在途缓冲；
预算耗尽时在条件变量上阻塞排队并记录 `buffer_exhausted_blocked` 事件，
不丢弃任何任务。SQLite 写入在提交阶段单线程串行执行。

## 9. 可视化

- `sir serve` 启动内置 HTTP 面板，展示逐帧参数表、通道延迟、能量曲线、
  累积误差上界曲线、原始/重建波形对比，并支持运行/暂停/重试/导出。
- `desktop/` 是 Wails v2 工程，绑定方法与 HTTP API 同构；
  Linux 构建桌面二进制需要 webkit2gtk 开发头文件（本环境未安装，故交付工程骨架与内置面板）。
