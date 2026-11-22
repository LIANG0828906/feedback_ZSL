# 音频指纹与相似片段检索管线 — 技术说明

## 1. 范围与约束

纯本地管线，不使用容器、不访问外部目标。依赖仅 `numpy` 与本机
`ffmpeg`/`ffprobe`（也支持 `imageio-ffmpeg` 提供的二进制）。所有样本由
固定种子 `20261123` 确定性生成。

## 2. 目录结构

```
afpipe/
  errors.py        稳定错误码与异常层级（E_META/E_DECODE/...）
  config.py        参数、校验(E_PARA)、参数版本指纹、格式/算法版本常量
  hashing.py       规范 JSON、SHA-256、原子写(temp+fsync+os.replace)
  ffmpeg.py        FFmpeg 发现、探测、s16le 解码与整型→浮点归一
  manifest.py      清单 schema 校验(E_SCHEMA)、清单指纹
  samples.py       确定性样本生成器（WAV PCM + MP3，一组额外 OGG）
  fingerprint.py   分帧 FFT、对数频带、delta 嵌套量化、汉明相似度
  pipeline.py      单文件全链路与清单元数据交叉校验
  checkpoint.py    串行原子分片检查点、整快照失效语义
  indexstore.py    v2 索引持久化、版本门禁、v1 旧格式写入与显式升级
  builder.py       有界线程池并行提取 + 清单顺序串行提交 + 断点恢复
  search.py        粗前缀倒排→汉明校验→对角投票→片段族归并
  verify.py        仅依赖持久化产物的结果核对，首处不一致定位
  compat.py        兼容矩阵、共存去重、量化单调性观测
  cli.py           generate/build/search/verify/compat/checkpoint-info
tests/             42 个 unittest 用例
run_acceptance.py  端到端验收驱动，报告写入 artifacts/
```

## 3. 解码与统一归一（需求 1）

* 探测：优先 `ffprobe -show_streams`，缺失时回退解析 `ffmpeg -i` 输出。
* 解码：统一请求 `-ar <目标采样率> -ac <1|保留> -f s16le`，由 FFmpeg
  完成重采样与下混；进程内执行 `int16 / 32768 → float32`，保证整型归一
  行为跨容器一致。
* 缓冲边界：按 `decode_buf_frames` 限定每次读取块与单文件累计上限，
  满足每线程解码缓冲上限要求。
* 校验链路：
  * 采样率/声道/时长与清单不符 → `E_META`，details 指出字段、声明值、
    实际值；
  * 文件缺失或 ffmpeg 非零退出/无样本 → `E_DECODE`；
  * 实际声道超过 `max_channels` → `E_CHAN`（先于索引写入，拒绝整个文件）；
  * 解码后时长超出 `duration_tolerance_s` → `E_META`。

## 4. 指纹算法与量化单调性（需求 2）

每帧：Hann 窗 → rFFT 幅度谱 → 32 个对数间隔频带的加权 RMS 能量 →
相邻频带 delta（31 维谱形状特征，对全局增益与有损编码的音量变化稳健）。

嵌套量化（同一 delta 的码在各层级前缀一致）：

| quant_bits | 每 delta 码 | 含义 |
|---|---|---|
| 1 | 1 bit，2 级 | 符号 |
| 2 | 2 bit，4 级 | 符号 × 弱/强（阈值 0.35×尺度） |
| 4 | 3 bit 存储，8 级 | 符号 × 三级嵌套阈值（0.35/1.15/2.4） |

嵌套结构使量化收紧只能消除碰撞、不能制造碰撞，因此同索引帧的精确哈希
匹配率随 bit 数单调不增；鲁棒度量（归一化汉明相似度）实测同样单调不增。
参数越界（频带数、帧移、量化位、线程数等）在 `Config.validate()` 统一
抛出 `E_PARA`。

## 5. 版本模型与禁止静默混用（需求 3、5）

三层版本标识同时写入 `registry.json`：

* `index_format_version`（当前 `2.0.0`，旧版 `1.0.0`）——磁盘布局；
* `parameter_version`——对采样率、声道模式、帧长/帧移、频带数、量化位等
  指纹相关参数的 SHA-256 短指纹；
* `algorithm_version`——算法本身版本；
* 另存 `manifest_fingerprint` 与每条目的源文件 `sha256`、lineage。

查询加载时任一字段不匹配即抛 `E_VER` 拒绝查询，不做静默降级。旧版
1.0.0 索引只能通过显式升级转换，升级产出 `upgrade_trail.json`，逐条记录
`legacy_entry_id → new_entry_id`、帧计数与内容哈希映射。升级后的条目
`lineage="upgraded-from-1.0.0"`，与原生条目可在同一 v2 库共存；检索按
`physical_key` 归并，同一物理片段只计一个片段族。

## 6. 检索与片段族（需求 4）

1. 查询长度门禁：短于 `min_query_ms` → `E_SHORT`；长于 `max_query_ms`
   或倒排扫描超过 `max_postings_scan` → `E_BUDGET`。
2. 倒排索引以帧码粗前缀为键（1/2/4 bit 对应 7/8/10 位前缀），候选条目
   再做完整汉明相似度校验（层级阈值 0.20/0.25/0.30）。
3. 对角投票：`query_idx - entry_idx` 一致的命中聚成对齐候选，输出源文件、
   起止时间、对齐帧数与置信度（命中帧数/查询帧数），按置信度降序。
4. 片段族归并：同 `physical_key` 的 PCM、MP3、OGG 版本归为一族，给出族内
   各版本 `version_scores`；跨格式查询（如 MP3 查询命中 WAV 条目）天然
   支持，因为归一化与量化只依赖解码后的谱形状。

## 7. 可暂停/可恢复构建（需求 6）

* 每个文件按 `shard_frames` 切片，分片 JSON 经临时文件 + fsync +
  `os.replace` 原子落盘；全局锁保证写盘严格串行、不交错。
* 每片携带内容哈希、参数版本、算法版本与分片哈希；`state.json` 是串行
  追加日志，文件完成后写 `_complete.json` 标记。
* 重启恢复：先核对参数/算法/清单身份，再逐片核对磁盘分片哈希。任一不
  匹配 → 整个快照删除重算（`invalidation` 记录原因），禁止混用旧分片；
  未完成文件不参与恢复，只依据完整持久化分片续建。
* SIGKILL 测试见 `tests/test_checkpoint_resume.py`：被杀后仅有完整文件
  可复用，重启后产出完整索引。

## 8. 验证工具与异常隔离（需求 7）

`verify` 只读取持久化索引、清单与查询请求重新检索：

* 真值模式：核对族身份、目标片段时间窗重叠、PCM/lossy 双格式覆盖、
  物理片段不重复计数、置信度顺序与取值范围；
* 基线模式：与已保存结果逐字段比对（时间字段带 1e-4 容差），发现不一致
  即输出首个 `query_id / match_source / field`；
* 清单 JSON 损坏或缺关键字段 → `E_SCHEMA`；
* 单文件提取异常被工作线程捕获并记为失败（可重试），不写入该条目，其他
  文件索引不受污染（见 `tests/test_failure_isolation.py`）。

## 9. 并发与资源边界（需求 8）

* 指纹提取在 `ThreadPoolExecutor(max_workers=cfg.max_workers)` 中并行，
  上限可配（1–16）；FFmpeg 子进程受每线程缓冲块大小约束。
* Future 按清单顺序逐个消费，检查点与索引提交全部串行，条目顺序等于
  清单顺序。并行（4 线程）与单线程构建的 registry 与 postings 经字节级
  比对完全一致（`tests/test_parallel_equivalence.py`）。
* 声道超上限文件在解码前拒绝，无任何部分写入。

## 10. 错误码对照

| 码 | 触发场景 |
|---|---|
| E_META | 清单 sr/声道/时长/内容哈希与实际不符 |
| E_DECODE | 文件缺失、ffmpeg 失败、无样本输出 |
| E_PARA | 配置参数越界或不支持的量化位 |
| E_VER | 索引格式/参数/算法/清单指纹不匹配 |
| E_SHORT | 查询短于最小可检索长度或帧数不足 |
| E_BUDGET | 查询超时长上限或扫描超预算 |
| E_SCHEMA | 清单非法 JSON 或 schema 缺字段 |
| E_CHAN | 声道数超过声明上限 |
| E_CORRUPT | 持久化索引/检查点损坏 |

## 11. Windows 运行步骤

```powershell
py -3.10 -m venv .venv
.venv\Scripts\activate
python -m pip install -r requirements.txt
# 任选其一：把 ffmpeg.exe 放入 PATH，或 set FFMPEG=C:\tools\ffmpeg.exe
# 也可：pip install imageio-ffmpeg
python run_acceptance.py
python -m unittest discover -s . -p "test_*.py"
```

路径全部使用 `os.path` 拼接、子进程用列表形式调用，无 shell 依赖。

## 12. 实测结果（本机 Linux / Python 3.14 / numpy 2.5 / ffmpeg libmp3lame）

* 42 个 unittest 全部通过；端到端验收 `artifacts/acc_00_summary.json`
  状态 PASS。
* 兼容矩阵：当前 v2 与升级后 v2 上，PCM 与 lossy 查询命中率均为 1.0；
  1.0.0 旧库与参数版本错配均被 `E_VER` 拒绝，无意外接受。
* 量化单调性（PCM vs MP3 归一化汉明相似度，三组素材）：
  1 bit ≈ 0.89–0.90 ≥ 2 bit ≈ 0.85–0.87 ≥ 4 bit ≈ 0.76–0.81，
  默认 2 bit 全部达到 `match_rate_floor=0.75`。
* 共存库去重：每个查询恰好 1 个片段族，`duplicate_physical_keys=0`。
* 并行/串行构建 registry 与 postings 字节一致；SIGKILL 后恢复完整。

### 已知编码特性

libvorbis 输出的 OGG 容器头在不同进程间不保证字节一致，但解码后的 PCM
字节完全一致；因此确定性校验对 OGG 比较解码内容而非容器哈希，WAV/MP3
仍比较文件 SHA-256。
