# 架构与技术说明

## 1. 模块总览

| 模块 | 职责 |
| --- | --- |
| `scoreworkbench/constants.py` | 内部模型版本、转换规则版本、PPQ、资源上限等常量 |
| `scoreworkbench/errors.py` | 八个稳定错误码与结构化 `details` |
| `scoreworkbench/model.py` | 统一领域模型 `Score/Note/ExpressionEvent/...`、索引、悬空校验、时间换算 |
| `scoreworkbench/versioning/` | 三个 schema 版本的加载器、导出器、降级警告、往返比对 |
| `scoreworkbench/session.py` | 编辑会话、工作副本、原子批次提交与回滚 |
| `scoreworkbench/persistence.py` | 版本门控快照、临时文件 + `fsync` + `os.replace` 原子提交 |
| `scoreworkbench/playback.py` | 统一拍位回放引擎、NumPy 曲线采样、可注入时钟 |
| `scoreworkbench/gui/` | PySide6 主窗口与三泳道时间轴控件 |
| `scoreworkbench/resources.py` | 打开文件信号量、事件缓冲上限、进程内串行写锁 |
| `scripts/generate_samples.py` | 固定种子离线样本生成 |
| `scripts/acceptance_demo.py` | 端到端验收驱动，产出 JSON 报告 |

## 2. 三个 schema 版本的差异与统一策略

| 维度 | v1.0 | v2.0 | v3.0 | 内部模型 v4.0 |
| --- | --- | --- | --- | --- |
| 命名 | snake_case | camelCase | snake_case | snake_case |
| 音符时间 | 整数 tick（480 PPQ，小节内） | 浮点拍（小节内） | 微秒（`start_us` 绝对） | 浮点拍（小节内） |
| 事件时间 | 整数 tick | 毫秒 `timeMs`（绝对）+ 小节/拍锚点 | 微秒 `anchor_us`（绝对）+ 小节索引 | 小节 + 小节内锚点拍 |
| 力度 | MIDI 0..127，缺省 96 | 浮点 0..1，缺省 0.75 | 浮点 0..1，必填 | 浮点 0..1 |
| 表情词汇 | legato/staccato/tenuto；none/cresc/dim | 增加 accent、rit、aTempo | 再增加 fermata | 全量枚举 |
| 踏板 | 布尔 | 布尔 | 布尔 + 浮点 `depth` | 数值 + 可选 depth |
| 独有能力 | — | — | `pitch_bend`、`interactive_layer` | 全量保留 |

统一时间单位是**拍（quarter beat）**，绝对拍 = 小节起点拍 + 小节内拍位；秒由
`seconds = beat * 60 / bpm` 推导。所有单位换算集中在 `versioning/common.py`，
tick 与微秒使用整数乘除保证可逆，因此切换源版本后同一事件的绝对时间不漂移。

## 3. 升级、降级与转换痕迹

- 升级时每个被补齐的可选字段写入一条 `TraceRecord(action="default_fill")`，
  每个单位换算写入 `action="normalise"`，包含 `object_path/field_path/detail`。
- 降级前调用 `preview_lossy` 得到 `E_LOSSY` 警告清单，每条含 `field_path`、
  `object_id`（音符或事件编号）与原因；未显式 `confirm_lossy=True` 时抛
  `LossyError`，绝不静默丢弃。
- 降级适配是“克隆后裁剪”：移除 `pitch_bend`、置空 `interactive_layer`、
  折叠 fractional pedal、收窄枚举，原模型不被修改。

## 4. 一致性与悬空防护

- `Score.rebuild_index` 重建音符/事件索引并检测重复标识。
- `validate_references` 对每个事件校验 `note_id` 存在，否则 `E_DANGLE`
  （携带 `event_id` 与 `target_note_id`）。
- 移动/替换音符时，绑定事件按 `(measure, anchor_beat)` 增量迁移；
  删除音符时级联删除其事件，保证无悬空残留。

## 5. 编辑会话与提交边界

- `EditSession` 持有 `_committed`（已提交）与 `_working`（深拷贝工作副本）。
- 编辑仅改工作副本；`commit` 先在候选克隆上完整校验，通过后整体替换并记录
  `BatchRecord`（含受影响对象标识集合与操作序列），失败则已提交模型保持原样。
- `rollback` 丢弃工作副本与待处理队列；导出与回放始终读取已提交模型。

## 6. 持久化与原子快照

快照信封同时携带 `snapshot_format_version`、`domain_model_version`、
`conversion_rules_version`。读取时任一不匹配即**整体作废**并要求从源文件重建，
不存在新旧记录混用。写入流程：同目录临时文件 → `flush` → `fsync` →
`os.replace` 原子替换 → 目录 `fsync`，配合进程内 RLock 与 `fcntl` 文件锁，
强制终止后只能看到完整旧快照或完整新快照（Windows 无 `fcntl` 时退化为进程锁）。

## 7. 回放与可视化

`PlaybackEngine` 用可注入的 `Clock`（测试用 `FakeClock`）驱动，支持任意拍位
起播、暂停、停止、seek；力度/踏板曲线由 NumPy 重采样，连奏以区间表示。
越界拍位或力度抛 `E_PARA` 并携带 `source_event_id` 与合法范围。GUI 时间轴
渲染三条泳道（Velocity / Pedal / Legato）与播放头，点击时间轴可 seek。

## 8. 错误码对照表

| 错误码 | 触发场景 | 关键诊断字段 |
| --- | --- | --- |
| `E_SCHEMA` | 未知版本、缺必填字段、类型错误 | `file_path/version/field_path` |
| `E_DECODE` | 文件不可读、JSON 损坏 | `file_path/line/column` |
| `E_SEMANTIC` | 可解析但语义冲突（时间锚点不一致、重复 ID） | `object_path` |
| `E_DANGLE` | 事件绑定的音符不存在 | `event_id/target_note_id` |
| `E_LOSSY` | 降级会丢失字段且未确认 | `warnings[]` |
| `E_ROUNDTRIP` | 往返文档不等价 | `first_mismatch.path/expected/actual` |
| `E_PARA` | 拍位/力度越界 | `source_event_id/valid_range` |
| `E_SNAPSHOT` | 快照缺失、撕裂或版本不匹配 | `expected/actual` |

## 9. 资源边界

- `max_open_files=8`：并行加载通过有界信号量占用文件句柄。
- `max_event_buffer=50000`：载入事件数超限抛 `E_SEMANTIC`。
- 批量加载/转换可线程并行，结果顺序与输入一致；所有快照写入经同一把串行锁，
  提交顺序稳定。

## 10. 强制终止语义

强制终止只保证快照层面的原子性：重启后要么读到完整旧快照、要么完整新快照。
正在生成但尚未原子替换的导出文件不承诺完整，重启后应从持久化快照恢复，
再重新执行导出。
