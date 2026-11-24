# 语音事件指纹标注工作台 — 技术设计说明

面向游戏音效资产管理的本地桌面工作台，核心是**逐帧事件标注**而非跨格式检索：
分帧 → 频谱特征 → 帧指纹量化 → 与类别特征音模板匹配 → 连续帧聚合为事件区间 →
基于清单真值做质量校验。全部能力在本机离线完成，无任何外部网络依赖。

## 1. 模块结构

| 包 | 职责 |
| --- | --- |
| `internal/model` | 领域类型、参数默认值、稳定异常码（`E_*`）、算法版本常量 |
| `internal/sample` | 固定种子确定性素材生成（PCM + 描述 JSON + 清单 + 类别音调参数） |
| `internal/dsp` | Hann 窗、radix-2 FFT、分帧指纹量化、类别模板合成、静音占比、参数校验 |
| `internal/ingest` | 素材/PCM 加载、结构与区间校验、内容哈希 |
| `internal/match` | 模板余弦打分、间隙容忍聚合、置信度、一对一质量校验 |
| `internal/window` | 帧窗口划分与四态状态机 |
| `internal/checkpoint` | 纯 Go SQLite（modernc.org/sqlite）原子串行检查点与快照作废 |
| `internal/pipeline` | 受限线程池引擎、实时/恢复/全量三条路径、崩溃续跑 |
| `internal/compare` | 逐帧/逐事件/逐统计自动比对，定位首个不一致 |
| `internal/server` | 本地 HTTP API 与嵌入式专业界面（go:embed，原生 JS） |
| `cmd/audiobench` | `generate` / `demo` / `serve` 三个命令 |

## 2. 确定性指纹算法

1. 分帧：`frame_length`（须为 2 的幂）、`frame_shift`（≤ 帧长），Hann 加窗。
2. FFT：自实现迭代 radix-2 Cooley-Tukey，纯 `float64` 标量、固定运算顺序，
   不读取 goroutine 本地状态或随机源，因此**与线程标识、分帧完成顺序无关**。
3. 频带能量：线性划分 `bands` 个频带，取带内平均谱能量。
4. 量化：除以总能量得相对能量分布，乘以 1000 四舍五入为整数码本，
   同时保留 `total_energy`、帧号与采样点偏移。
5. 类别模板：用与素材合成完全相同的确定性渲染器生成特征音，再走同一套
   分帧/FFT/量化路径取平均相对能量，保证模板与素材“同源”。

## 3. 事件聚合与置信度

- 每帧对全部类别模板算余弦相似度，取最高分；超过 `match_threshold` 记为命中。
- 同类别相邻命中，间距不超过 `gap_tolerance_frames` 即合并；命中数低于
  `min_hit_frames` 的片段丢弃。按类别独立聚合，天然保证跨类别区间不重叠。
- 置信度 = 平均相似度 ×（0.5 + 0.5 × 覆盖率），覆盖率为命中帧占运行帧跨度比例。
- 无任何命中时返回**空事件列表而非错误**；声能量占比低于 `quiet_ratio_threshold`
  返回 `E_QUIET` 并在 details 给出实测 `voice_energy_ratio`。

## 4. 异常码映射

| 码 | 触发条件 | 关键 details |
| --- | --- | --- |
| `E_SCHEMA` | 字段缺失、非法 JSON、类别不在枚举、采样率不支持 | `field`、`event_index`、`label` |
| `E_RANGE` | 事件越界、事件重叠、PCM 字节数不符 | `event_index`、`actual_start/end`、`material_bound` |
| `E_PARA` | 帧长/帧移/频带/间隙/阈值越界，帧移>帧长 | `field`、`actual` |
| `E_QUIET` | 声能量占比低于阈值 | `voice_energy_ratio`、`threshold` |
| `E_QUAL` | 类别错配或起止偏差超容差 | `mismatch_count` 及报告内逐项错配 |
| `E_STATE` | 窗口非法迁移、冻结窗口被参数改写、非失败窗口重试 | `window_index`、`from`、`to` |
| `E_DIV` | 三路径首个不一致 | `kind`、`index`、`field`、两侧取值 |
| `E_IO` | 本地文件 / SQLite 读写失败 | 底层错误 |

## 5. 窗口状态机与检查点

- 四态：`pending → extracting → committed`，任意活动态可 `→ failed`，
  `failed → extracting` 重试；`committed` 为终态，任何迁移返回 `E_STATE` 且原状态不变。
- 每提交一个窗口，在**单连接、单事务**内写入窗口指纹、参数快照、事件片段，
  写入严格串行且按窗口序号排序，绝不交错。
- `meta` 表保存素材 ID、内容哈希（PCM 的 SHA-256）、参数版本、算法版本、
  窗口/总帧数。重启时任一不匹配 → 单事务清空全部 `windows` 与 `meta`，
  **整体作废后全量重算，禁止部分混用旧窗口**。
- 进程被强制终止后，只恢复已落盘窗口，未提交窗口重新计算；
  系统不承诺强制终止后仍能产出完整报告（见 `pipeline` 崩溃测试）。

## 6. 三条执行路径与一致性

- `realtime`：窗口并行提取、串行提交，跑到完成。
- `resume`：校验快照身份，匹配则从首个未提交窗口续跑，不匹配则整体作废重算。
- `full`：不依赖检查点，顺序一次性全量计算。

三者共用同一套纯函数指纹与聚合逻辑；`internal/compare` 逐帧比对
（帧号、偏移、总能量、量化码本、相对能量）、逐事件比对八个字段、再比对质量
统计，首个差异即返回 `E_DIV` 并给出帧号/事件编号、字段名与两侧取值。

## 7. 并发资源边界

- 工作线程池受 `MaxWorkers` 限制；工作槽占满时派发侧**阻塞排队**并记录
  `BlockEvent`（窗口号 + 原因），不静默丢窗。
- 检查点存储 `SetMaxOpenConns(1)`，配合单一提交协程，保证写入串行与窗口顺序。
- 窗口计划的所有读写都在引擎互斥锁保护下，`go test -race` 全绿。

## 8. 本地界面

`audiobench serve` 启动 `127.0.0.1` HTTP 服务，界面通过 `go:embed` 打包进单一
二进制，原生 HTML/CSS/JS，无 CDN/外部字体。界面提供：波形与事件区间叠加、
逐窗状态矩阵、帧指纹码本、清单/结果对照、质量统计、三路径比对、参数调整、
增量标注、逐窗重试、暂停/取消与 JSON 导出。
