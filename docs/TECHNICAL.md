# 音频指纹工作台技术说明

## 模块结构

| 模块 | 职责 |
| --- | --- |
| `fpworkbench/errors.py` | 业务码：`E_FMT_INVALID` / `E_MIGRATE_LOSS` / `E_BASELINE_MISMATCH` / `E_BUSY` / `E_LIB_MISMATCH`，`FpError` 携带文件与字段路径 |
| `fpworkbench/model.py` | 版本无关的统一内部表示 `Fingerprint` / `PeakPair`；帧长、频带、时间量化基准（32ms）等规范参数 |
| `fpworkbench/formats/v1.py` | v1：原始峰值对 `[anchor_frame, f1, f2, dt]` + 帧号 |
| `fpworkbench/formats/v2.py` | v2：分段哈希 `{hash, t_quant, segment}`、量化位 `quant_bits`、频带 `bands_hz` |
| `fpworkbench/formats/v3.py` | v3：声道合并标记 `channel_merge`、压缩元数据 `compression`、提取真值头 `provenance`、历史保留块 `legacy` |
| `fpworkbench/wavio.py` | 标准库 `wave` 读写 16-bit PCM，单/双声道，8000–48000 Hz |
| `fpworkbench/extract.py` | STFT → 分带峰值 → 峰值对 → `(hash, t_ms)`；参数与真值来源写入 `provenance` |
| `fpworkbench/migrate.py` | v1/v2 → v3 迁移，输出字段映射表与 `E_MIGRATE_LOSS` 丢失清单 |
| `fpworkbench/retrieve.py` | 哈希桶 + 时间偏移投票检索；量化基线对齐，不兼容时 `E_BASELINE_MISMATCH` |
| `fpworkbench/consistency.py` | 迁移前后同一查询序列逐条比对，按声明容差判定并定位样本/偏移/版本 |
| `fpworkbench/library.py` | JSON 清单持久化（临时文件 + 原子重命名）、版本与内容哈希校验、迁移互斥锁 |
| `fpworkbench/samplegen.py` | 固定种子的多采样率/单双声道合成样本与三版本基线生成器 |
| `fpworkbench/gui.py` | PySide6 工作台：版本分布、字段映射、迁移进度、检索时间轴与投票分布可视化 |
| `scripts/demo.py` | 脚本化演示：语料生成 → 建库 → 迁移前后检索 → 一致性报告 → 轨迹日志 |

## 指纹文件格式

三版本均为 `{format_version, payload, checksum}` 信封，`checksum` 为
`sha256(canonical_json(payload))`（键排序、紧凑分隔符）。解析顺序：
版本号 → 校验和 → 逐字段类型校验；任一失败抛 `FpError(E_FMT_INVALID)`
并附 `file` 与 `field`（如 `payload.peak_pairs[3]`），绝不返回空指纹。

## 特征提取管线

- 帧长按采样率换算为最接近 46.4ms 的 2 的幂（8000Hz→512，44100Hz→2048），hop 为帧长 1/4；
- 频带边界以 Hz 声明（300/500/800/1200/2000/3500），按各采样率折算为 FFT bin，低采样率自动坍缩高频带；
- 双声道先均值合并为单声道，`channel_merge=true` 写入头部；
- 每带每帧取局部极大且超过带内均值 1.5 倍的前 2 个峰值，锚点与后续 24 帧目标区内最多 4 个峰值配对；
- 哈希 = `(f1<<22)|(f2<<12)|dt`，时间按统一基准 32ms 量化；
- 提取器标识、帧长/hop/量化/频带等参数全部写入 v3 `provenance` 头。

## 迁移规则

- v1→v3：峰值对原样保留（`preserved-raw`），哈希时间重定标到 32ms 统一网格；
  v1 无 `channel_merge` 字段，由声道数推导并记一条 `E_MIGRATE_LOSS`（原始合并状态不可恢复）；
- v2→v3：`quant_bits` 与 `segment` 下标无 v3 对应字段，原值保留在 `legacy` 块并逐项记
  `E_MIGRATE_LOSS`，不静默丢弃；
- 重定标产生离网格残差时，按数量与最大残差记 `E_MIGRATE_LOSS`，原始时间序列保留在
  `legacy.extras.original_hash_times_ms`；
- 每样本输出 `migration_report.json`：`field_mappings`（source/target/action）+ `lost_fields`。

## 检索与一致性

- 库侧建 `hash -> [(entry, t_ms)]` 桶；查询哈希命中后按 `t_lib - t_query` 在统一量化基准上取整投票；
- 基线一致或整数倍（如 32↔64）自动对齐；非整数倍（如 32↔48）抛 `E_BASELINE_MISMATCH`；
- 候选含 `offset_ms / start_ms / end_ms / votes / confidence / vote_histogram / lib_version`；
- 一致性验证（`consistency.py`）对迁移前后结果按 `(query, sample)` 逐条比较，
  超差输出 `OFFSET_EXCEEDS_TOLERANCE`、丢失输出 `LOST_AFTER_MIGRATION`，均定位到样本、偏移与版本。

## 持久化与原子性

- `library.json` 含 `lib_version`、样本清单（路径/版本/sha256/迁移标记）、迁移进度、`content_hash`；
- 写入 = 同目录 `.tmp` 临时文件 + `fsync` + `os.replace` 原子重命名；每个样本迁移后立即检查点；
- 载入校验 `lib_version` 与 `content_hash`，不符抛 `E_LIB_MISMATCH`；
- 迁移产物写入新文件 `<sample_id>.v3.json`，原始版本文件保留（`original_path` 记录），不覆盖；
- `migrate_all` 用非阻塞锁保证同一库同时只有一个迁移任务，重复提交抛 `E_BUSY`。

## 确定性

- 样本生成使用 `numpy.random.RandomState(seed)`，查询片段与噪声均由该序列派生；
- 轨迹日志（`artifacts/demo/trace.log`）每行一个排序键 JSON 事件，不含任何时间戳；
- 相同种子与操作序列下日志逐字节一致（已用两次运行 `diff` 验证）。

## 实际验证结果

- `python -m unittest discover -s tests`：18 个测试全部通过（三版本解析/校验、往返序列化、
  迁移映射与丢失清单、检索命中真值偏移、基线不匹配、库版本/内容哈希校验、E_BUSY、确定性）；
- `python scripts/demo.py`：9/9 一致性比较通过（容差 64ms），三条查询的真值偏移误差
  分别为 16/28/8ms；六个异常码用例全部按预期触发；
- GUI 冒烟测试（`QT_QPA_PLATFORM=offscreen`）：库加载、后台检索（3 候选）、
  后台迁移、时间轴绘制均正常，主线程未阻塞。

## 运行方式

```bash
uv venv .venv && uv pip install --python .venv/bin/python numpy PySide6
.venv/bin/python -m unittest discover -s tests   # 测试
.venv/bin/python scripts/demo.py                 # 脚本化演示（产物在 artifacts/demo/）
.venv/bin/python -m fpworkbench.gui artifacts/demo  # 交互工作台
```
