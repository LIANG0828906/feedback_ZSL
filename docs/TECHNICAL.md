# 语音字幕时序对齐管线 — 技术说明

## 总览

面向游戏过场与互动媒体无障碍呈现的语音字幕时序对齐数据处理管线。
核心保证：**并行调度与资源预算下的时序可复现** —— 并发生成的先后
顺序不影响时序结果；阅读速度与折行耗时冲突时通过受控取舍收敛；
中断后能从已提交台词继续。

## 模块结构

| 模块 | 职责 |
| --- | --- |
| `caption_pipeline/loader.py` | 清单加载与结构校验（E_SCHEMA/E_REF/E_PARA） |
| `caption_pipeline/envelope.py` | 逐帧能量语音包络检测（帧长/帧移可配） |
| `caption_pipeline/windows.py` | 台词显示窗口推导与空隙延长、待取舍标记 |
| `caption_pipeline/layout.py` | 行宽约束最小代价折行（DP）与多屏切分 |
| `caption_pipeline/fit.py` | 排版耗时 ↔ 可读时长双向约束与取舍 |
| `caption_pipeline/states.py` | 台词级状态机（E_STATE） |
| `caption_pipeline/scheduler.py` | 工作线程池 + 阻塞式资源预算 |
| `caption_pipeline/checkpoint.py` | SQLite 原子串行检查点 |
| `caption_pipeline/engine.py` | 编排：检测 → 推导 → 并行排布 → 串行提交 |
| `samplegen.py` | 固定种子确定性样本生成器（含解析真值） |
| `verify.py` | 独立结果验证工具（E_SEG 链路） |
| `main.py` | 无窗口 CLI 入口（gen/summary/run/verify） |
| `gui.py` | PySide6 图形界面（暂停/继续/停止/续跑） |

## 关键设计

### 1. 加载校验（业务要求 1）
- 字段缺失/类型非法 → `E_SCHEMA`，引用悬空/跨段窗口 → `E_REF`，
  数值非法（每行最大字符数、行高、阅读速度上限等）→ `E_PARA`，
  均在 `PipelineError.field` 中指出字段名。
- 加载后 `Project.summary()` 输出工程摘要与台词总数
  （CLI：`main.py summary`）。

### 2. 包络检测与窗口推导（业务要求 2）
- 逐帧能量（帧长 `frame_len`、帧移 `hop` 可配），阈值为峰值能量的
  固定比例，近邻合并 + 短段丢弃，输出确定。
- 窗口 = 所属语音段（或显式窗口）；短于 `ceil(字数/语速上限*采样率)`
  时延长至下一句窗口起点之前的空隙；空隙不足记 `deficit`（待取舍），
  不静默截短。帧参数越界 → `E_PARA`（指出 `frame_len`/`hop`）。

### 3. 确定性折行与多屏切分（业务要求 3）
- 代价 `Σ(行宽上限-行宽)²×权重`，动态规划取最小；同代价取靠前断点，
  结果只依赖（文本, 参数），与线程/遍历顺序无关。
- 避头尾：闭口标点不出现在行首、开口标点不出现在行尾，标点不孤立
  成行；超单屏行数上限（`(屏高-2×留白)//行高`）切分为连续多屏。
- 行宽为零/权重为负 → `E_PARA`（指出字段名）。

### 4. 双向约束与受控取舍（业务要求 4）
- 折行结果反推最短可读时长：
  `Σ行(ceil(行字数/语速×sr) + 行开销) + (屏数-1)×切屏开销`。
- 冲突时按可解释优先级取舍并逐条记录（台词编号、手段、理由）：
  ① 压缩留白（margin→4，提升单屏行数，减少切屏开销）
  ② 增加屏幕切分（软上限 2 屏 → 硬上限 4 屏内放行）
  ③ 回收空隙（延长窗口覆盖残余差额）
- 仍不可收敛 → `E_FIT`，`details.lines` 列出冲突台词编号与实测差额
  （采样点）。

### 5. 状态机与提交边界（业务要求 5）
- `PENDING → LAYING_OUT → COMMITTED/FAILED`，`FAILED → PENDING`（重试）。
- 非法迁移 → `E_STATE` 且保持原状态；前置依赖（包络+窗口推导）未完成
  不得进入 `LAYING_OUT`。
- 进度 = 已提交台词数/总台词数；已提交台词锁定窗口与折行，参数调整
  因版本不匹配导致快照整体作废，只影响未提交台词的重算。

### 6. 并行调度与资源预算（业务要求 6）
- `ThreadPoolExecutor` 工作池，`max_workers` 限制最大并行度；
  文本/采样两类计数型预算（`_BlockingBudget`）耗尽时阻塞排队，
  不静默丢台词；单任务超预算 → `E_PARA`。
- 折行/取舍为纯函数，提交按时间轴顺序串行化 → 并行与串行逐句一致
  （测试 `test_parallel_equals_serial`、`test_budget_blocks_but_never_drops`）。

### 7. 检查点机制（业务要求 7）
- 每提交一句，SQLite 单事务写入（窗口、折行、取舍、内容哈希、参数
  版本、算法版本），单连接 + 单锁串行，写入不交错。
- 重启时内容哈希/参数版本/算法版本任一不匹配 → 整体作废快照并全量
  重算，禁止部分混用。
- 强制终止（kill -9）后只依据已持久化且版本匹配的台词恢复
  （测试 `test_kill9_recovery`）。

### 8. 独立验证工具（业务要求 8）
- `verify.py` 只读持久化结果与工程清单，独立重算期望值并逐项比对：
  窗口不重叠、不超阅读速度上限、折行行数/每行字符数与真值一致、
  整段占用时长与真值在一个帧长容差内。
- 首个违规输出：台词编号、违规项名称、实测值；坏段（引用/窗口越界）
  走 `E_SEG` 链路，可修复后重试，不污染其他台词。

## 运行方式

```bash
uv venv .venv && uv pip install --python .venv/bin/python -r requirements.txt
.venv/bin/python main.py gen --out samples --seed 20261123 --projects 3
.venv/bin/python main.py summary --project samples/proj_0/manifest.json
.venv/bin/python main.py run --project samples/proj_0/manifest.json --persist ckpt0 --workers 4
.venv/bin/python main.py verify --project samples/proj_0/manifest.json --persist ckpt0
.venv/bin/python gui.py samples        # 图形界面（需显示环境）
.venv/bin/python -m pytest tests -q    # 测试（GUI 用 offscreen 平台）
```

## 验证记录

见 `docs/VALIDATION.md`（实际运行输出）。
