# HDR 帧序列色彩转换工作台 — 技术说明

## 1. 环境与运行

- Python 3.12.16（要求 3.11/3.12），本地虚拟环境 `.venv`
- 依赖：NumPy 2.5.5、PySide6 6.12.0（安装时官方稳定版）
- 不使用 Docker 或任何容器；所有落盘走“临时文件 + fsync + 原子重命名”
- 样本全部由本地确定性生成器合成，无网络、无真实素材依赖

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install numpy PySide6
.venv/bin/python run_experiment.py demo --work-dir ./workspace
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -v
```

Windows 上等价执行 `python run_experiment.py demo --work-dir workspace`，
原子提交使用标准库 `os.replace`，在 NTFS 上同样保证同卷原子替换。

## 2. 模块结构

| 模块 | 职责 |
| --- | --- |
| `errors.py` | 五个稳定错误码与带定位信息的异常类型 |
| `hdrformat.py` | 32 字节自定义二进制头、帧布局、长度/码值校验、原子读写 |
| `color.py` | Rec.709/Rec.2020/Display-P3 基色矩阵、Bradford 白点适应、sRGB/PQ/HLG、三种色调映射 |
| `fixedpoint.py` | 可配置 QF 定点量化、就近/确定性随机舍入与逐元素舍入方向 |
| `pipeline.py` | 六步链路、浮点/定点双路径、每步内容哈希与最大码值偏差 |
| `errorbound.py` | 逐像素区间传播误差上界与最早越界环节归因 |
| `cache.py` | 影响因子指纹、逐帧缓存键、依赖闭包与保守重算原因 |
| `versions.py` | 不可变版本、两阶段发布、查询隔离与陈旧校验 |
| `snapshot.py` | 按帧分块原子快照、哈希链、崩溃恢复 |
| `generator.py` | 确定性样本生成（PCG64 种子序列，暗部纹理+高光条带） |
| `experiment.py` | 脚本化编排与 JSONL 轨迹日志 |
| `gui.py` | 可选 PySide6 薄界面，计算仍走确定性核心 |

## 3. 二进制格式

小端固定头 32 字节：魔数 `HDRSEQ01`、宽/高/帧数（uint32）、位深（uint16）、
色彩空间码（uint8）、传递函数码（uint8）、峰值亮度（uint32）、4 字节保留零。
帧数据为紧凑交织 RGB；8-bit 用 uint8，10/12/16-bit 统一 uint16 容器，
10/12-bit 样本超过 `2^bit_depth-1` 即判定非法。

错误定位约定：头部级问题 `bad_frame=-1`；第 N 帧截断/超范围时 `bad_frame=N`
（0 基），尾随字节定位到声明帧数。

## 4. 转换链与双路径

链路固定为 `linearize -> primaries -> adapt -> tonemap -> encode -> quantize`。

- 浮点路径全程 float64；定点路径对线性化值、矩阵系数与每次矩阵乘、
  色调映射、编码结果按 `q=2^-F` 重新量化，并记录 -1/0/+1 舍入方向。
- 每一步中间量以规范化字节的 SHA-256 标识；定点模式同时输出每步
  “换算到目标码值尺度”的最大绝对偏差。
- 模式非法、小数位越界、舍入模式未知或曲线非法时抛 `E_MODE_INVALID`，
  在任何帧产出之前失败。
- 双路径逐帧逐像素偏差清单与超容差帧号由 `FrameResult.pixel_diff`、
  `over_tolerance` 给出；目标空间/传递函数变化后 `diff_summary`
  输出新旧编码值的逐帧与整体差异摘要。

## 5. 误差上界传播

每个像素通道维护“真实计算中心 + 区间半宽”，沿链路传播：

1. 线性化：源码值 ±0.5 码值反演区间，定点再加 0.5q
2. 基色矩阵：区间矩阵乘（负/正系数分别取端点）+ 系数舍入 L1 项 + 0.5q
3. 白点适应：同型区间矩阵乘与系数舍入项
4. 色调映射：区间包络与“上游半宽 × 局部斜率”取最大值，包含截断
5. 编码：包络与“上游半宽 × 编码局部斜率”取最大值，定点再加 0.5q
6. 输出量化：信号半宽折算码值 + 半码值舍入；定点双路径独立舍入再补半码值

最终上界单位为目标码值。阈值越界时按链路顺序记录**最早越界环节**；
传播中出现 NaN/Inf 抛 `E_ERROR_UNBOUNDED` 并携带候选因子列表与帧号。
在 3 种传递函数 × 3 源空间 × 3 目标空间 × 3 位深 × 3 档小数位共
1944 组配置上验证：真实双路径偏差始终不超过声明上界（最大差值 0.0）。

## 6. 增量重算与版本隔离

- 影响因子：`target_colorspace`、`target_peak_nits`、`tone_curve`、
  `frac_bits`；其余配置（传递函数、模式、舍入、种子、位深、容差）
  同样进入逐帧缓存键，任何变化都不会错误复用。
- 缓存键 = SHA-256(帧源内容哈希 + 规范因子集)。命中即复用并记录
  `frame_reused`；无法证明未受影响（无缓存、缺源哈希、因子变更）时
  保守重算并在 `conservative_reasons` 标注原因。
- 重算在 `VersionBuilder` 独立结构中完成，`VersionStore.publish`
  一次性切换当前指针；查询只返回完整旧版本或完整新版本并回带版本哈希。
- 版本哈希由清单与逐帧哈希链重算得到；任何帧内容被篡改都会在查询时
  触发 `E_VERSION_STALE`，且不返回任何帧。

## 7. 快照与崩溃恢复

每帧一个分块：`chunk_0000NN`，含帧号、内容哈希、前块哈希、链式哈希、
码值、误差摘要、源哈希、阶段哈希与因子指纹。提交顺序为
临时文件 → fsync → `os.replace`；全部分块完成后才发布 manifest。

恢复时按序校验字段完整性与哈希链，断链/缺字段抛
`E_RECOVERY_INVALID` 并定位首个断链帧；参数集指纹不匹配禁止复用。
已提交帧只返回既有分块、不重复写入，未提交帧补算后统一发布。
实验通过 `crash_after` 注入强制终止（不依赖真实 kill），
manifest 在崩溃时保证不存在。

## 8. 确定性与轨迹日志

- 生成器使用显式 `SeedSequence` 的 PCG64，无时间戳、无文件大小参与真值
- 轨迹日志为排序键 JSONL、固定 `\n` 换行；同种子同操作序列两次实验
  日志与样本文件逐字节一致（已在验收脚本中比对 SHA-256）
- 事件类型：`sequence_loaded`、`recompute_plan`、`frame_recomputed`、
  `frame_reused`、`version_diff`、`version_published`、`snapshot_resume`、
  `crash_injected`、`recovery_scan`、`snapshot_published`

## 9. 需求与错误码对照

| 需求 | 实现入口 | 测试 |
| --- | --- | --- |
| 1 加载/校验/定位异常帧 | `hdrformat.load_sequence` | `tests/test_format.py` |
| 2 完整链路与差异摘要 | `pipeline.convert_*`、`diff_summary` | `tests/test_color_pipeline.py` |
| 3 双路径与 E_MODE_INVALID | `pipeline`、`fixedpoint` | `tests/test_color_pipeline.py` |
| 4 误差上界/归因/E_ERROR_UNBOUNDED | `errorbound` | `tests/test_errorbound.py` |
| 5 增量重算与依赖闭包 | `cache`、`experiment` | `tests/test_cache_versions.py`、`test_experiment.py` |
| 6 版本发布/查询隔离/E_VERSION_STALE | `versions` | `tests/test_cache_versions.py` |
| 7 快照恢复/E_RECOVERY_INVALID | `snapshot` | `tests/test_snapshot.py` |
| 8 确定性生成器与实验入口 | `generator`、`run_experiment.py` | `tests/test_experiment.py` |

## 10. 实际验证结果

- `python -m unittest discover -s tests`：36 个测试全部通过
- `pyflakes`：无告警；全部模块 `py_compile` 通过
- `run_experiment.py demo`：6 个版本发布，包含一次全帧复用、
  一次崩溃后仅补算缺失帧；两次运行轨迹与样本 SHA-256 完全一致
- GUI 在 `QT_QPA_PLATFORM=offscreen` 下实例化并完成一次双路径转换

## 11. 已知建模边界

- 三种内置空间白点均为 D65，默认白点适应为恒等；Bradford 链路保留，
  接入非 D65 素材时无需改架构。
- HLG 按场景线性 OETF 建模，未额外施加 BT.2446 显示映射；峰值亮度
  作用于色调映射曝光归一环节，可按需扩展 OOTF。
- 上界模型刻意保守（区间 + L1 + 安全舍入项），适合做可信性门禁，
  不代表统计意义上的典型误差。

