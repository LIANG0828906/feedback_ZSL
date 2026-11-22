# ScoreForge 技术说明

## 1. 时间单位与绝对时间换算

- 全工程只使用一种声明时间单位：tick，`TICKS_PER_QUARTER = 480`。
- 拍号 `[num, den]` 的小节长度：`num * (480 * 4 / den)` tick。
- 绝对物理时间由 `TempoMap`（分段常速，首段必在 tick 0）逐段积分换算：
  `ms = Σ segment_ticks * 60000 / (bpm * 480)`。
- 模型内所有事件始终保留 tick 值（可回溯），毫秒仅在观测/回放边界换算。

## 2. 样本与真值

`scoreforge/generator.py` 以固定种子（默认 42）产出 3 组工程，每组包含：

- 结构描述：`measure_structure`（拍号序列）+ `parts[].measures[].notes[]`
  （`note_id` / `pitch` / `duration_ticks`，时值为正整数 tick）；
- 表达事件：`dynamics` / `legato` / `pedal`，以
  `(anchor_note_id, beat_offset_ticks)` 绑定到具体音符；
- 真值清单：指定小节期望总时值、力度曲线采样点期望值（含容差）、
  事件绝对时间戳（含容差）。

真值由生成器内的参考实现独立计算（`ref_*` 函数），是观测与验证的唯一依据；
模型实现（`model.py`）与其交叉验证（`tests/test_model_timeline.py`）。
同一绝对拍位出现多个力度事件时，规范语义为"后者优先"
（`collapse_dynamics`，两侧一致）。

## 3. 错误码

| 代码 | 含义 | 触发位置 |
| --- | --- | --- |
| E_SCHEMA | 字段缺失/类型非法 | `validation.py` |
| E_REF | 声部/小节/锚点悬空、拍位越过小节 | `validation.py` / `session.py` |
| E_DUR | 时值非正整数、累计时值溢出小节 | `validation.py` / `session.py` |
| E_DANGLING | 删除/替换音符后残留悬空事件（含事件 id） | `session.prepare_commit` |
| E_BATCH | 编辑冻结小节、空批次、重复提交 | `session.py` |
| E_CHECKPOINT | 检查点版本/哈希不匹配（整体作废） | `checkpoint.py` |
| E_DIVERGE | 三路径结果不一致（含路径/字段/两侧取值） | `recompute.compare_scores` |
| E_PREC | 真值精度超容差（含小节编号与采样点拍位） | `precision.py` |

## 4. 编辑会话与提交边界

- `EditSession.score` 为已提交内部模型，`EditSession.working` 为工作副本；
  所有编辑只作用于工作副本，导出与回放只读已提交模型。
- 提交分两阶段：`prepare_commit`（纯读：校验 + 片段构建 + 内容哈希，
  可并行）与 `apply_commit`（原子写回 + 索引刷新 + 检查点写入，主线程串行）。
- 提交失败不触碰已提交模型（批次整体回滚）；工作副本保留编辑以便修复重试。
- 已提交小节冻结：再次编辑或重复提交返回 E_BATCH。
- 修改音符时值后，下游音符 onset 由累计时值派生，锚定事件的绝对拍位自动迁移
  （`tests/test_model_timeline.py::test_anchor_migration_after_duration_edit`）。

## 5. 检查点

- 每次提交写入 `checkpoints/<project_id>/fragment_<seq>.json`
  （模型片段 + 事件快照 + 内容哈希 + 参数/算法版本），并更新 `manifest.json`。
- 写入：tmp 文件 + `os.replace` + `fsync` 原子落盘；单锁串行化，seq 严格递增，
  乱序写入直接拒绝（E_CHECKPOINT）。
- 加载：`manifest` 与每个片段的版本号、内容哈希全部校验；任一不匹配则
  删除全部片段与 manifest（整体作废，禁止部分混用），从源样本重新加载。

## 6. 三路径一致性

- 路径 A 实时编辑：`realtime_run`；路径 B 断点恢复：`resume_run`
  （提交 k 小节后丢弃会话模拟强杀，`restore_session` 续跑，
  已冻结小节的编辑操作由 `_skip_committed_ops` 过滤）；
  路径 C 全量重算：`full_recompute`。
- `compare_scores` 逐音符时值、逐事件绝对 tick/毫秒、120 tick 网格力度采样
  比对，首个不一致即抛 E_DIVERGE（路径 + 字段名 + 两侧取值）。

## 7. 精度校验

`verify_truth` 输出完整偏差报告：逐小节总时值偏差、逐采样点力度绝对偏差、
逐事件时间戳偏差；任一超容差由 `report.raise_if_failed()` 抛 E_PREC。
真值未覆盖的小节全部列入 `report.unverifiable`（显式不可校验，不静默通过）。

## 8. 并发与资源边界

- `ParallelCommitter`：`max_workers` 限制并行度；`buffer_size` 信号量限制
  在途任务，耗尽时 `acquire()` 阻塞排队（不静默丢弃）。
- 准备阶段在线程池并行，应用阶段按给定顺序串行 → 并行与串行产出相同
  提交序列与相同最终模型（`tests/test_concurrency.py`）。
- 单小节异常仅标记该小节 FAILED（错误对象随结果返回），修复后可单独重试，
  其他小节不受影响。

## 9. 回放

- `PlaybackEngine` 构造时一次性导出已提交时间轴（只读快照）；
  起播/暂停/跳转仅移动读指针，事件绝对时间不漂移；回放不修改任何模型状态。
- GUI（`scoreforge/gui/`）：时间轴控件绘制力度曲线、连奏、踏板泳道与播放头，
  支持任意拍位起播；所有领域错误以错误码弹窗 + 状态栏展示。

## 10. 验证记录（本机实测）

- 环境：Linux (WSL Ubuntu) / Python 3.14.4 / numpy 2.5.5 / PySide6 6.12.0 /
  pytest 9.2.0（任务书指定 Windows + Python 3.11，代码无平台依赖）。
- `python -m pytest tests/ -q` → 48 passed。
- `python -m scoreforge.acceptance` → 7 章节全部通过（失败项 0），
  报告：`artifacts/acceptance/acceptance_report.txt`。
- GUI 冒烟测试在 `QT_QPA_PLATFORM=offscreen` 下通过
  （加载→编辑→提交→精度校验→恢复→回放全流程）；
  手工窗口验证需本地显示会话，运行 `python main.py`。
