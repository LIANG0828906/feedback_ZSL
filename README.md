# FPStore：可恢复的本地音乐指纹样本库

本项目是一个不依赖网络服务、不使用容器的本地工作台。它把 WAV 特征提取、v1/v2/v3
指纹记录解析、v1/v2→v3 迁移、分段原子入库、强制终止恢复、整体替换发布、版本化检索
和迁移一致性报告串成一条可脚本化验证的链路。

所有样本由本地生成器确定性合成；所有库状态只依赖磁盘上的清单快照、分段文件和发布
指针。核心实现使用 Python + NumPy，桌面工作台使用 PySide6；没有显示环境时可运行等价
的命令行演示。

## 1. 环境与安装

已验证环境：

- Python 3.14.4
- NumPy 2.5.5
- PySide6 6.12.0
- Linux 本地文件系统，无 Docker/容器

标准安装方式：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
```

本工作区也兼容 `uv`：

```bash
uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python -r requirements.txt
```

以下命令均从仓库根目录运行。

## 2. 一键验收演示

```bash
.venv/bin/python -m fpstore.cli demo \
  --work artifacts/demo \
  --seed 20261121 \
  --crash-after 3
```

该命令会真实启动子进程入库，在提交第 3 个分段后执行 `os._exit(137)`，随后用新进程从
磁盘快照恢复。演示同时覆盖：

- 3 首本地合成样本，采样率 8000/16000/22050 Hz，单声道/双声道，16-bit PCM WAV；
- v3 分段入库、强制终止、恢复跳过已提交分段、不重复写入；
- 第一次整体发布、第二轮 v2 入库、第二次整体发布；
- 发布前打开旧快照，发布后旧快照仍完整可读，新快照包含全部新版本记录；
- v1 库迁移到 v3，迁移前后对同一查询序列逐条比对；
- 量化位、频带划分、声道合并策略变化后的重算差异与最大时间偏移；
- 全部结果写入确定性 JSONL 轨迹。

输出文件：

- `artifacts/demo/trace.jsonl`：逐事件轨迹，含样本、版本、分段状态、分段哈希、链哈希、
  匹配偏移、置信度与库版本哈希。
- `artifacts/demo/demo_summary.json`：强杀/恢复/发布/查询摘要。
- `artifacts/demo/migration_report.json`：字段映射表、丢失字段与逐记录迁移结果。
- `artifacts/demo/migration_consistency.json`：迁移前后逐查询、逐样本的行级差异。
- `artifacts/demo/recompute_report.json`：量化、频带、声道策略变化差异摘要与最大偏移。

生成两次并逐字节校验：

```bash
.venv/bin/python -m fpstore.cli demo \
  --work artifacts/determinism-check \
  --seed 20261121 \
  --crash-after 3 \
  --verify-determinism
```

校验结果写入 `artifacts/determinism-check/determinism_verification.json`。

## 3. 手动强制终止与恢复步骤

先建立独立演示目录：

```bash
mkdir -p artifacts/manual/samples
.venv/bin/python -m fpstore.cli generate \
  --out artifacts/manual/samples \
  --seed 20261121
```

第一次入库，提交 3 个新分段后强制终止进程。命令返回码应为 `137`：

```bash
.venv/bin/python -m fpstore.cli import \
  --library artifacts/manual/library \
  --samples-dir artifacts/manual/samples \
  --track-id-from-stem \
  --version 3 --quant-bits 0 \
  --trace artifacts/manual/trace.jsonl \
  --crash-after 3
echo "exit=$?"
```

记录已提交分段和库清单：

```bash
.venv/bin/python -m fpstore.cli status \
  --library artifacts/manual/library
```

也可以直接查看 `artifacts/manual/library/staging/current.json` 指向的清单快照。此时：

- `published/current.json` 不存在或仍指向旧版本；
- `staging/current.json` 指向最后已提交清单；
- 已被段文件写入但尚未更新 staging 指针的孤儿分段不属于已提交状态；
- 清单内每个分段带 `prev_chain_hash` / `chain_hash`。

重启并恢复：

```bash
.venv/bin/python -m fpstore.cli import \
  --library artifacts/manual/library \
  --resume \
  --version 3 --quant-bits 0 \
  --trace artifacts/manual/trace.jsonl
```

恢复会先校验清单哈希、哈希链和已提交分段文件内容哈希，再从最近有效提交继续；已提交
分段只记录 `segment_skip`，不会再次写入。

整体发布后才可检索：

```bash
.venv/bin/python -m fpstore.cli publish \
  --library artifacts/manual/library \
  --trace artifacts/manual/trace.jsonl

.venv/bin/python -m fpstore.cli query \
  --library artifacts/manual/library \
  --wav artifacts/manual/samples/track_beta.wav \
  --version 3 --quant-bits 0 \
  --top-k 3
```

协作式取消可把 `--crash-after` 换成 `--cancel-after N`。取消发生在分段检查点，保留已提交
分段和最后一次已发布版本；之后仍可 `--resume`。

## 4. GUI 工作台

有显示环境：

```bash
.venv/bin/python -m fpstore.gui --work artifacts/gui
```

无显示环境烟雾测试：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m fpstore.gui \
  --work artifacts/gui-smoke \
  --offscreen-smoke
```

工作台提供“生成样本、开始入库、取消入库、恢复入库、整体发布、查询首样本、迁移到 v3、
状态”按钮。入库运行在 `QThread` 中，取消按钮设置协作式取消事件，每个分段检查点响应。

## 5. 记录格式与统一下沉表示

记录外壳：

```json
{
  "schema": "fp-record/1",
  "version": 1,
  "record_id": "track:v1:seg000000",
  "checksum": "sha256:...",
  "payload": {}
}
```

校验和覆盖 `schema + version + record_id + payload` 的规范 JSON。所有必填字段都显式检查，
没有 `.get(..., default)` 式静默补值。

### v1

必填 payload 字段：

- `track_id`
- `segment_index`
- `frame_number`
- `sample_rate`
- `hop_length`
- `peak_pairs[]`：`anchor_frame`、`t1_frame`、`t2_frame`、`f1_bin`、`f2_bin`

v1 不保存时间量化位。内部时间基准为 `frame`，原始帧号完整保留。检索哈希只从已保存的
峰值对和帧差确定性派生，不写回任何虚构的 v2/v3 字段。

### v2

在 v1 基础上增加：

- `segment_hash`：峰值对与分段哈希的规范内容哈希；
- `segment_hashes[]`：带 `offset_ticks`、`offset_ms` 的哈希项；
- `time_quant_bits`：毫秒时间量化位；
- `band_layout`：严格递增的频带边界（Hz）；
- `hash_algo`。

v2 的时间基准为 `ms:<bits>`，量化粒度为 `2^bits` 毫秒。

### v3

在 v2 基础上增加：

- `channel_merge`：`mono` / `mean` / `left` / `right` / `sum_clip` / `not_recorded`；
- `compression`：`codec`、`bit_depth`、`lossless`；
- `time_quant_unit`；
- 可选迁移溯源：`origin_version`、`origin_checksum`、`origin_time_quantization`、
  `migration`、`migration_context`。

字段缺失、类型错误、未知版本、段内容哈希不匹配、记录校验和不匹配均返回
`E_FP_INVALID`，并携带 `record_id` 与具体字段路径，例如
`peak_pairs[0].f2_bin`。

## 6. 确定性特征提取

实现位于 `fpstore/fingerprint.py`：

1. 使用标准库 `wave` 读取 16-bit PCM、单/双声道、8000–48000 Hz WAV；
2. 双声道按显式策略合并，不做隐式默认；
3. 使用周期 Hann 窗和 NumPy RFFT 计算 STFT；
4. 在每帧取确定性的局部对数幅度峰值，按幅度、频率、帧号稳定排序；
5. 在 1..N 帧范围内生成峰值对，按 `(频率 bin1, 频率 bin2, 时间差, 频带 id)` 哈希；
6. 分段哈希对排序后的峰值对和哈希项做规范 SHA-256。

时间语义：

- v1/frame 基准：`frame_index` 是时间 tick；帧时长由 `hop_length / sample_rate` 决定；
- v2/v3 ms 基准：`offset_ticks` 的单位是 `2^bits` 毫秒；
- v2/v3 哈希中的时间差使用量化后的 tick，因此量化位变化会触发新哈希和新偏移。

`compare_extractions()` 对同一 WAV 在两个配置下重算，输出共享/未匹配峰值数、偏移变化
数、哈希变化数、平均绝对偏移和最大绝对偏移，并保留逐峰明细。

## 7. 迁移与时间信息保留

`fpstore/migration.py` 支持 v1/v2 → v3：

- v1 峰值对、帧号、原始 checksum 保留；
- v1 的原始帧量化基准写入 `origin_time_quantization`；
- v2 的 `segment_hashes`、`segment_hash`、`time_quant_bits`、`band_layout` 原样保留；
- v1 的检索哈希从峰值对确定性派生后写入 v3，不改变哈希语义；
- 每条记录和顶层报告都输出字段映射表；
- v1/v2 未知字段没有 v3 映射时返回 `E_MIGRATE_LOSS`，列出受影响记录、字段和值哈希，
  不静默丢弃；
- 时间量化信息不允许丢失，迁移严格模式失败时不发布新库版本。

`artifacts/demo/migration_report.json` 是演示生成的字段映射与损失报告实例。

## 8. 原子入库、恢复与发布隔离

库目录布局：

```text
library/
  locks/operation.lock
  manifests/<sha256>.json
  segments/<前两位>/<sha256>.json
  staging/current.json
  staging/sessions/<version_id>.json
  published/current.json
```

单个分段提交顺序：

1. 将规范记录字节写入同目录临时文件，`flush + fsync`，再 `os.replace` 为内容寻址段文件；
2. 将含新哈希链的完整 staging 清单写临时文件并用原子重命名发布；
3. 原子替换 `staging/current.json` 指针。

因此强杀可能留下未被指针引用的段文件或清单文件，但不会产生“已声明提交而段文件不完整”
的状态。恢复时：

- 读取 `staging/current.json`；
- 校验清单自身哈希、`entry_count`、每个 entry 的必填字段；
- 从 `parent_manifest_hash` 或 `GENESIS` 重算整条 `prev_chain_hash/chain_hash`；
- 校验每个已提交分段文件的内容哈希；
- 任何断链、缺字段或内容不匹配返回 `E_RECOVERY_INVALID`，`details.first_broken_segment`
  定位首个异常分段；
- 已提交分段按 key + 分段文件哈希跳过，不重复写入。

发布时先写完整 manifest 快照，再原子替换 `published/current.json`。查询只读取当前指针
指向的完整快照，因此并发发布时只能看到完整旧版本或完整新版本。查询返回
`library_version_hash`，结束时关闭 `LibrarySnapshot` 并清空其记录引用。

版本哈希与清单不一致、分段内容被篡改、调用方期望哈希不符时返回 `E_LIB_MISMATCH`。

同一库的入库、迁移和发布共用一个 `flock` 非阻塞操作锁；第二次并发提交返回 `E_BUSY`。

## 9. 检索与时间基准对齐

`fpstore/retrieval.py` 在已发布快照上建立 `hash -> segments` 桶：

- 每个哈希命中按 `数据库偏移 - 查询偏移` 投票；
- 同一曲目的最佳偏移按票数、绝对偏移稳定排序；
- 置信度为最佳偏移票数 / 查询与候选较小哈希数；
- 返回候选样本、指纹版本、起止时间、偏移、置信度、命中的库版本哈希和命中哈希样本。

时间基准规则：

- 两侧均为 `ms` 基准时，对齐到较粗的 `max(bits)` 量化粒度；
- 两侧均为 `frame` 基准时，要求 `hop_length/sample_rate` 相同；
- frame 与 ms 基准不能直接比较；未迁移的直接跨基准候选返回
  `E_ALIGN_MISMATCH`、标记 `excluded: true`，不进入投票。

## 10. 迁移一致性报告

`compare_query_sequences()` 对迁移前后相同查询序列逐条比对，不输出只有总数的报告：

- 每个查询、样本一行；
- 行内包含迁移前/后库版本哈希、起止时间、偏移、置信度、命中哈希样本；
- 差异类型为 `missing_match`、`hash_mismatch` 或 `time_drift`；
- 超过声明的 `tolerance_ms` 时给出 `offset_delta_ms`；
- 哈希不一致时给出交集为空及两侧版本信息。

演示使用 20 ms 容差，迁移前后 17 条分段、3 个查询全部一致，差异数为 0。

## 11. 命令摘要

```bash
# 生成样本
.venv/bin/python -m fpstore.cli generate --out artifacts/samples --seed 20261121

# 入库（可 --resume / --crash-after / --cancel-after）
.venv/bin/python -m fpstore.cli import \
  --library artifacts/library \
  --samples-dir artifacts/samples --track-id-from-stem \
  --version 3 --quant-bits 0

# 整体发布
.venv/bin/python -m fpstore.cli publish --library artifacts/library

# 查询
.venv/bin/python -m fpstore.cli query \
  --library artifacts/library --wav artifacts/samples/track_alpha.wav \
  --version 3 --quant-bits 0 --top-k 3

# 迁移并发布
.venv/bin/python -m fpstore.cli migrate --library artifacts/library

# 状态
.venv/bin/python -m fpstore.cli status --library artifacts/library

# 同一 WAV 两个配置重算差异
.venv/bin/python -m fpstore.cli recompute-diff \
  --wav artifacts/samples/track_beta.wav \
  --version 3 --old-quant-bits 0 --new-quant-bits 5 \
  --old-band-edges 0 500 2000 4000 8000 \
  --new-band-edges 0 1000 3000 6000 \
  --old-channel-merge mean --new-channel-merge left
```

`import`、`query`、`publish`、`migrate` 均支持 `--trace <jsonl>`。

## 12. 业务码

| 业务码 | 含义 |
| --- | --- |
| `E_FP_INVALID` | 记录版本/字段/类型/校验和/段哈希无效，携带记录与字段定位 |
| `E_MIGRATE_LOSS` | 迁移存在无法映射字段，携带受影响记录与字段清单 |
| `E_ALIGN_MISMATCH` | 候选时间量化基准不可直接对齐，候选被排除 |
| `E_RECOVERY_INVALID` | 恢复清单、哈希链、分段文件或会话无效，定位首个断链分段 |
| `E_LIB_MISMATCH` | 发布指针、清单哈希、分段内容或期望版本哈希不一致 |
| `E_BUSY` | 同一库已有入库或迁移任务 |
| `E_CANCELLED` | 操作在检查点被取消 |
| `E_AUDIO_INVALID` | WAV 不是受支持的本地 16-bit PCM 单/双声道 8000–48000 Hz |
| `E_STATE_INVALID` | 状态不允许当前操作，例如对未到达 ready 的 staging 发布 |

## 13. 测试与本次验证结果

运行：

```bash
.venv/bin/python -m unittest discover -s tests -v
```

本次结果：**24 项测试全部通过**（约 3.5 秒），覆盖严格解析、字段定位、迁移损失、
确定性提取、量化/频带/声道变化、强杀恢复、取消、`E_BUSY`、发布隔离、并发查询、
`E_LIB_MISMATCH`、断链定位、`E_ALIGN_MISMATCH` 排除和迁移前后行级一致性。

GUI 离屏烟雾测试退出码：`0`。

完整演示实际结果：

- 强制终止返回码：`137`
- 强杀时已提交分段：`3`
- 恢复时跳过：`3`，新增提交：`14`，最终分段：`17`
- 第一次 v3 发布哈希：
  `sha256:f1b8b05a4139024324fff6f162756d6f337f7c217607a37803469886ba507406`
- 第二次发布（v3 + v2）哈希：
  `sha256:bfc61ff9f5fdb8d856a29db39182759cc987341516208d6a2a92a4ad5eafc968`
- 发布隔离：旧快照 17 条记录仍可读；新快照 34 条记录，哈希不同
- v1 迁移前发布哈希：
  `sha256:f74203a663fd344e23521b26bf448b8c033e3cfc04f31a5211c2134558cc61ab`
- 迁移后发布哈希：
  `sha256:45ee632b90fcbbd2b020ce345a6ea3a8044820cb1af83ccfd8e22f2d33528b4d`
- 迁移前后查询差异：`0`
- 量化/频带/声道重算最大绝对偏移：`31 ms`
- 演示轨迹 SHA-256：
  `0ae22d9ba9e3db3874e71e6e48ba6dd91323517793fd69ab2d5acd1e7330563e`
- 双工作区同种子同操作序列：`trace_byte_identical=true`，
  `summary_byte_identical=true`

## 14. 主要代码索引

- `fpstore/records.py`：v1/v2/v3 解析、校验和、字段级错误定位。
- `fpstore/fingerprint.py`：WAV→STFT→峰值对→哈希、配置重算差异。
- `fpstore/migration.py`：迁移映射、损失检测、时间基准保留。
- `fpstore/library.py`：操作锁、原子分段、清单哈希链、恢复、发布、快照。
- `fpstore/retrieval.py`：桶投票、时间基准对齐、一致性报告。
- `fpstore/demo.py`：强杀/恢复/发布/迁移可重复演示。
- `fpstore/cli.py`：完整脚本化入口。
- `fpstore/gui.py`：可选 PySide6 工作台。
