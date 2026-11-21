# TileForge

本地、确定性、可恢复的分块 HDR 转换工作台。实现自定义 `.tcseq` 二进制序列、Rec.709/Display-P3/Rec.2020 与 SDR gamma/PQ/HLG 转换、固定并行度与内存预算调度、增量重算与原子发布、在线预览/离线一致性校验，以及强制终止后的逐块哈希链恢复。

## 当前验证状态

- 单元与集成测试：`28/28` 通过。
- 本地验收矩阵：`11/11` 通过。
- 样本：8/10/12/16-bit，4 种图块网格，SDR/PQ/HLG 多方向转换。
- 预览与离线：4 个样本逐块、逐码值一致；容差 1 code。
- 增量：仅改 tile 3 曝光时只重算 1 块，复用 11 块。
- 预算：`full -> banded` 降级成功；彻底超限返回 `E_BUDGET_EXCEEDED`，旧版本不变。
- 恢复：子进程在第 5 块提交后 `os._exit(75)`，恢复只转换剩余 10 块，最终哈希与离线一致。
- 回退与过期版本：版本回退成功，旧 hash 查询返回 `E_VERSION_STALE`。
- 验收报告：[artifacts/acceptance/report.json](artifacts/acceptance/report.json)
- 轨迹日志：[artifacts/acceptance](artifacts/acceptance) 下共 21 个 `.jsonl`、739 条事件。

当前执行容器只预置 Python 3.14.4，没有 Python 3.11、NumPy 或 PySide6 的本地离线安装包。核心引擎因此只依赖 Python 标准库，已在现有环境完成全部验收；目标 3.11 环境可使用仓库内离线引导脚本安装本地 wheel。PySide6 工作台已实现，但本容器因缺少 Qt 绑定未做窗口级运行验证，已验证缺依赖时会给出明确提示并退出，不影响 CLI/核心验收。

## 快速运行

核心命令不依赖 NumPy/PySide6：

```bash
python3 -m tileforge generate --out artifacts/local-samples --seed 190
python3 -m tileforge inspect artifacts/local-samples/texture_8bit_rec709_sdr.tcseq
python3 -m tileforge convert \
  --input artifacts/local-samples/texture_8bit_rec709_sdr.tcseq \
  --config artifacts/local-samples/texture_8bit_rec709_sdr.config.json \
  --workspace artifacts/local-workbench
python3 -m tileforge preview --workspace artifacts/local-workbench --once
```

离线整段转换与预览比对：

```bash
python3 -m tileforge offline \
  --input artifacts/local-samples/texture_8bit_rec709_sdr.tcseq \
  --config artifacts/local-samples/texture_8bit_rec709_sdr.config.json \
  --output artifacts/offline.tcseq

python3 -m tileforge preview \
  --workspace artifacts/local-workbench \
  --offline-report artifacts/offline.tcseq.report.json
```

完整验收：

```bash
python3 -m unittest discover -s tests -v
python3 scripts/run_acceptance.py
```

## 离线 Python 3.11 + NumPy + PySide6

先把已下载的 NumPy、PySide6 wheel 放入同一目录，然后执行：

```bash
./scripts/bootstrap_offline.sh /path/to/wheelhouse /path/to/python3.11
.venv-offline/bin/python -m tileforge --help
.venv-offline/bin/python -m tileforge workbench \
  --input artifacts/local-samples/texture_8bit_rec709_sdr.tcseq \
  --config artifacts/local-samples/texture_8bit_rec709_sdr.config.json \
  --workspace artifacts/local-workbench
```

`bootstrap_offline.sh` 强制 Python 版本为 3.11，并只用：

```text
pip install --no-index --find-links WHEELHOUSE ...
```

不会访问网络。`requirements-offline.txt` 给出 NumPy/PySide6 的兼容范围。

## 目录

```text
tileforge/
  binary.py       二进制格式、严格校验、原子写入
  color.py        确定性解码/矩阵/白点适应/色调映射/编码
  budget.py       采样点、浮点缓冲、编码缓冲与峰值内存估算
  scheduler.py    固定并行度、累计预算、等待队列、稳定提交
  config.py       参数规范化、conversion id、缓存 fingerprint
  store.py        不可变版本、CURRENT 原子指针、回退、清单哈希
  recovery.py     逐块提交、参数版本、降级阶段、哈希链校验
  pipeline.py     增量规划、发布、离线转换、后台任务控制
  preview.py      固定窗口快照、游标推进、逐码值比对
  samplegen.py    固定种子样本与基线哈希
  gui.py          PySide6 工作台（可选依赖）
tests/            28 项标准库 unittest
scripts/          离线引导与端到端验收
artifacts/        实际验收样本、版本库、离线输出、轨迹与报告
```

## `.tcseq` 格式

固定 56 字节小端头部：

| 偏移 | 字段 | 类型 | 说明 |
|---:|---|---|---|
| 0 | `magic` | `8s` | `TCSEQ1\0\0` |
| 8 | `version` | `u16` | 格式版本 1 |
| 10 | `header_size` | `u16` | 固定 56 |
| 12 | `width` | `u32` | 图像宽 |
| 16 | `height` | `u32` | 图像高 |
| 20 | `tiles_x` | `u16` | 横向图块数 |
| 22 | `tiles_y` | `u16` | 纵向图块数 |
| 24 | `bit_depth` | `u8` | 8/10/12/16 |
| 25 | `channels` | `u8` | 1..4；色彩转换要求 3 |
| 26 | `source_transfer` | `u8` | 0=SDR gamma, 1=PQ, 2=HLG |
| 27 | `target_transfer` | `u8` | 同上 |
| 28 | `conversion_id` | `u32` | 参数指纹的 CRC32 |
| 32 | `block_count` | `u32` | 必须等于 `tiles_x * tiles_y` |
| 36 | `data_length` | `u64` | 所有块 payload 字节数之和 |
| 44 | `flags` | `u32` | 保留标志 |
| 48 | `reserved` | `u32` | 必须为 0 |
| 52 | `header_crc` | `u32` | 前 52 字节的 CRC32 |

每个图块：

```text
<tile_id:u32, x:u32, y:u32, width:u16, height:u16,
 exposure:f64, sample_count:u32, payload_length:u32>
payload[payload_length]
content_hash[32]
```

- 8-bit 每采样 1 字节；10/12/16-bit 每采样 2 字节小端。
- `content_hash = SHA-256(32-byte block header || payload)`。
- 图块坐标必须与声明的规则网格一致且 ID 唯一。
- `width % tiles_x != 0` 或 `height % tiles_y != 0` 返回 `E_GRID_INVALID`，`details.collision_fields` 指明冲突的尺寸与网格字段。
- 数据长度、采样数、码值上限、CRC、块哈希、尾部多余字节全部严格校验。
- 文件写入使用同目录临时文件、`flush + fsync + os.replace + 目录 fsync`；失败时清理临时文件，上一有效文件保持不变。

## 色彩转换链

固定顺序：

1. 源传递函数解码：BT.709 SDR OETF 逆变换、ST 2084 PQ EOTF、BT.2100 HLG 逆 OETF。
2. RGB 基色矩阵到 CIE XYZ。矩阵由 Rec.709 / Display-P3 / Rec.2020 色度坐标和 D65 白点确定性求解。
3. 白点适应。不同白点时使用 Bradford 变换；相同白点走严格恒等路径。
4. XYZ 到目标线性 RGB。
5. 逐块曝光：`effective_stops = block_override(或文件曝光) + global_exposure_stops`，线性增益为 `2^stops`。
6. 峰值映射：`none`、`reinhard`、`aces`，按目标峰值亮度压缩并保持色相比例。
7. 目标传递函数编码，量化到原序列位深。

Pipeline 不使用全局随机数、系统时间、任务完成顺序或线程局部状态。所有浮点临时量为 `float64`；内存降级只改变工作集和缓冲复用方式，不改变浮点精度、输出位深、转换代码或量化规则。每个块输出：

- 目标 payload 与块内容哈希；
- `max_code_delta`（量化前连续码值与整数码值的最大绝对偏差）；
- 生效曝光、色调映射曲线、降级阶段。

## 内存预算与调度

估算公式：

```text
sample_count = tile_width * tile_height * channels
float_bytes  = float_sample_capacity * 8
encoded_bytes = sample_count * (1 或 2)
total_bytes = float_bytes + encoded_bytes + overhead_bytes
```

声明的降级序列默认为：

| 阶段 | 浮点工作集 | 说明 |
|---|---|---|
| `full` | 6 个完整采样缓冲 | 全块并行流水 |
| `banded` | 8 行一组的 4 个缓冲 | 分带复用 |
| `streaming` | 2 个扫描行缓冲 | 单行原地复用 |

`tile_budget_bytes` 与 `run_budget_bytes` 在配置加载时固定，调度器没有任何提高预算的运行时代码。若当前累计预留不足，任务进入等待队列；只有最小声明阶段仍超单块预算时返回 `E_BUDGET_EXCEEDED`。错误详情包含所有阶段、估算用量、位深与 `precision_changed=false`。

并行度由 `memory.parallelism` 固定，内部 `ThreadPoolExecutor(max_workers=parallelism)`。不同块可并行完成，但提交回调按输入块顺序执行，恢复日志序号与哈希链因此稳定，不受线程完成顺序影响。

同一输入序列使用 `flock` 非阻塞运行锁；锁已被占用时立即返回 `E_BUSY`，不会等待或静默并发启动。

## 增量与发布

每块 cache fingerprint 包含：

- 源块内容哈希、tile id、几何、文件曝光；
- 生效曝光、逐块色调映射；
- 源/目标基色、传递函数、白点、峰值；
- 位深、通道数、观测窗口与同步标记。

一次参数提交先比较全部 fingerprint，只把变化块加入重算集合。未变化块直接复用当前不可变版本中的 payload、块哈希与误差指标。之后重新组装完整序列并一次发布。

发布顺序：

1. 将完整新序列写入 `versions/vNNNNNNNNNNNN.tcseq` 并通过原子重命名落盘。
2. 写不可变 manifest，内含版本、父版本、参数、影响/复用块、降级阶段、每块来源与哈希。
3. 原子替换 `CURRENT.json` 指针。

查询只读一次 `CURRENT.json`，随后打开不可变 manifest 与序列文件，因此发布期间只能看到完整旧版本或完整新版本。指定版本哈希不匹配时返回 `E_VERSION_STALE`。`rollback` 同样原子替换指针，历史文件永不修改。

## 恢复协议

运行目录：`runs/run-<key>/`

```text
state.json
commits/00000000.json
commits/00000001.json
...
```

`state.json` 记录输入哈希、参数哈希、参数版本、同步标记、影响块序列、基础版本和链种子。每个块提交包含：

- 序号、tile id、提交顺序；
- 参数版本、配置哈希、输入哈希、同步标记；
- payload、输出哈希、块内容哈希；
- 曝光、最大码值偏差、色调映射；
- 降级阶段与完整内存估算用量；
- 前一条链哈希与当前提交哈希。

`commit_hash = SHA-256(canonical(record_without_hash) || payload)`；下一条记录必须引用上一条哈希。所有块记录使用临时文件加原子重命名，并逐条 `fsync`。

恢复只扫描磁盘状态：校验 state、连续序号、文件名、字段完整性、参数版本、tile 顺序、payload 哈希、上一条链哈希和提交哈希。通过后跳过已提交前缀，只对缺失块调度转换；不会覆盖已提交块。断链、字段缺失或状态不一致返回 `E_RECOVERY_INVALID`。

预算彻底超限时删除本运行暂存目录且不发布，上一有效版本保持不变。

## 预览与离线一致性

- 工作台用 `QTimer(config.preview_refresh_ms)` 固定周期读取当前已发布快照。
- 离线入口使用同一 `JobConfig`、同一同步标记、同一观测窗口和同一色彩 Pipeline。
- `PreviewCursor` 每轮推进一个块，到末尾后用离线报告逐块比对。
- 先比较块哈希、曝光和同步字段；块哈希不同则按位深解码每个码值，按 `tolerance_code` 比较。
- 超差返回 `E_PREVIEW_MISMATCH`，详情定位首个 `tile_id`、`sample_index`、`pixel_index`、`channel`、`offset`、实际值、期望值与容差。

## 工作台

`python -m tileforge workbench --input ... --config ... --workspace ...`

界面包含：

- 图块网格热力预览；`F/B/S/R` 标注 `full/banded/streaming/reused`；
- 已发布版本、版本哈希、同步标记、影响/复用数量；
- 源/目标基色与传递函数、峰值、色调映射、同步标记、并行度、预算编辑；
- 选中图块的独立曝光编辑；
- 启动、暂停、恢复、协作式取消；
- 历史版本列表与回退；
- 固定刷新周期只读已发布版本，不读取未完成 journal。

## 轨迹

JSONL 事件不含时间戳，包含：

- `conversion_request`、`incremental_plan`、`recovery_scan`；
- `task_queued`、`task_submitted`、`task_completed`、`task_committed`；
- `downgrade_attempt`、`downgrade_accepted`、`budget_rejected`；
- `version_published`，含版本号、影响/复用/重算数量、降级阶段与全部内容哈希；
- `simulated_forced_termination`、恢复扫描、离线完成。

实际日志见 `artifacts/acceptance/**/traces/*.jsonl` 与 `artifacts/acceptance/offline/*.trace.jsonl`。

## 业务错误码

| 错误码 | 场景 |
|---|---|
| `E_HEADER_INVALID` | magic/version/header size/CRC/字段取值错误 |
| `E_GRID_INVALID` | 网格不能整除尺寸、块几何/ID 冲突 |
| `E_BIT_DEPTH_INVALID` | 非 8/10/12/16 位深 |
| `E_TRANSFER_CODE` | 未知传递函数码 |
| `E_DATA_LENGTH` | 采样数、payload、data_length、截断或尾随数据不一致 |
| `E_BLOCK_HASH` | 块内容哈希不匹配 |
| `E_BUSY` | 同一输入序列已有运行实例 |
| `E_BUDGET_EXCEEDED` | 所有声明降级阶段仍超预算 |
| `E_VERSION_STALE` | 查询/提交期望版本哈希不匹配 |
| `E_CONVERSION_MISMATCH` | 配置声明的转换标识与输入头部不一致 |
| `E_RECOVERY_INVALID` | 恢复状态缺失、断链、字段缺失或参数版本不一致 |
| `E_PREVIEW_MISMATCH` | 同步窗口或逐块码值超出声明容差 |
| `E_CONFIG_INVALID` | 参数非法、观测窗口越界、块覆盖越界 |
| `E_IO` | 原子写入或读取失败 |
| `E_CANCELLED` | 协作式取消 |
