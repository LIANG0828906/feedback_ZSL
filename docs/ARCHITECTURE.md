# HDR Delivery Studio 技术说明

## 1. 二进制格式

序列使用固定 46 字节小端头，后接连续帧数据。所有多字节整数为 little-endian。

| 偏移 | 长度 | 字段 |
|---:|---:|---|
| 0 | 8 | magic `HDRSEQ01` |
| 8 | 2 | 格式版本，当前为 1 |
| 10 | 4 | 头长度，必须为 46 |
| 14 | 4 | 宽度 |
| 18 | 4 | 高度 |
| 22 | 4 | 帧数 |
| 26 | 2 | 位深，允许 8/10/12/16 |
| 28 | 1 | 色彩空间码：1=Rec.709，2=Rec.2020，3=Display-P3 |
| 29 | 1 | 传递函数码：1=SDR gamma，2=PQ，3=HLG |
| 30 | 4 | 峰值亮度，float32，单位 nit |
| 34 | 8 | 每帧字节数 |
| 42 | 4 | 前 42 字节的 CRC32 |

帧数据为 `height × width × 3` 个通道，顺序 R/G/B。8-bit 使用 `uint8`；10/12/16-bit
使用每样本 16-bit little-endian 容器，未使用高位必须为零。读取器在构造时校验头、CRC、
总文件长度、每帧长度和声明位深；任何错误都会在返回帧前抛出 `E_HDR_INVALID`。

## 2. 色彩转换链

每帧按以下固定顺序处理：

1. 整数码值归一化为 `[0,1]`。
2. 源传递函数线性化到绝对 nit。
3. Rec.709/Rec.2020/Display-P3 的原生 RGB 通过基色矩阵进入 XYZ。
4. Bradford 矩阵做源白点到目标白点适应。
5. XYZ 进入目标基色 RGB。
6. 目标峰值映射：`none`、扩展 Reinhard、归一化 Hable。
7. 按帧应用曝光 `2^EV`。
8. 目标传递函数编码，量化到源位深。

所有中间值为 NumPy `float64`，仅在最终输出阶段四舍五入到整数码值。参数中的
`peak_luminance` 对 SDR/HLG 是显示峰值，对 PQ 仍按 ST 2084 的 10000 nit 绝对归一化。
HLG 采用 BT.2100 inverse OETF/OETF 的信号域转换，峰值元数据负责场景到显示峰值归一化，
不在色彩变换链中重复叠加显示 OOTF，以避免与目标 tone mapping 重复。

每帧摘要包含：

- `raw_min_code`、`raw_max_code`
- `max_code_deviation`：编码浮点码值与最终整数码值最大绝对偏差
- `max_clip_deviation`：低于 0 或高于满码值的最大越界量
- `clipped_samples`
- `out_of_range` / `overrange`

版本摘要另含 float64 精度声明、基色矩阵、矩阵哈希、白点适应和传递函数说明。

## 3. 版本与哈希链

每个 `VersionRecord` 记录：

- 单调递增 `version`、`parent_version`、`parent_record_hash`
- 完整参数集
- 受影响帧区间、影响帧数、重算帧数、复用帧数
- 全部逐帧 SHA-256、实际输出字节的 SHA-256
- 中间链摘要、逐帧码值偏差摘要
- `operation`（`convert` / `rollback`）与回退目标

`record_hash` 是记录除自身 `record_hash` 外规范化 JSON 的 SHA-256。下一版本的
`parent_record_hash` 必须等于上一版本 `record_hash`。回退读取历史参数与历史帧，
但以当前版本为父版本创建新的单调版本号。

参数依赖规则：

- 色彩空间、传递函数、峰值亮度或 tone map 改变：全部帧受影响。
- 仅曝光段改变：比较逐帧 EV，只重算 EV 不同的帧。
- 未受影响帧优先硬链接上一版本输出，并复用逐帧哈希；日志区分重算与复用数量。

## 4. 发布事务

一次提交按以下顺序执行：

1. 在内存中规范化并验证参数。
2. 在内存中转换所有受影响帧并检查曝光溢出；非法提交不会写任何输出帧。
3. 写入 `versions/.staging-vNNNNNN-PID`，复用帧硬链接，逐帧哈希与整体内容哈希复验。
4. 将 staging 目录原子重命名为 `versions/vNNNNNN`。
5. 将快照写入临时文件、`fsync`，原子替换 `snapshots/current.json`。
6. 在会话锁内一次性替换内存版本链和预览状态。
7. 追加确定性轨迹日志。

查询接口在会话锁内读取完整版本记录或帧内容；发布计算期间查询仍看到旧版本，快照提交
与内存切换完成后才看到新版本，不存在混合帧集。

发布前再次读取 staging 的全部帧哈希与内容哈希。测试钩子可在提交前制造字节错位，
此时返回 `E_VERSION_MISMATCH`，staging 被清理，旧版本保持不变。

## 5. 快照与恢复

`snapshots/current.json` 包含：

- schema 版本和源数据集描述（相对路径、文件/头哈希、格式字段）
- 完整版本链及哈希链
- 最新版本号
- 预览版本、帧号、播放状态
- 整个快照的规范化 JSON SHA-256

写入时先生成不可变 `snapshot-vNNNNNN.json`，再原子替换 `current.json`。恢复规则：

1. 快照必须是完整 JSON 且字段齐全，`snapshot_hash` 必须匹配。
2. 源文件哈希与头哈希必须匹配快照。
3. 版本号连续，父版本和父记录哈希必须连贯，每条记录哈希必须匹配。
4. 每个版本 manifest 必须与快照记录一致。
5. 已存在但哈希错误的发布帧视为损坏，返回 `E_RECOVERY_INVALID`。
6. 缺失帧视为未完成区间，只对该区间从源重新转换；结果必须匹配快照中的逐帧哈希，
   否则中止。
7. 大于快照最新版本号的孤儿版本目录会被丢弃；它们从未发布。

强制终止边界：

- staging 中终止：快照仍是旧版本，staging 在下次打开时清理。
- 版本目录已提升但快照未替换时终止：恢复旧快照并丢弃孤儿版本。
- 快照已替换后终止：恢复读到新版本，因为快照是提交点。
- 可处理取消返回 `E_CANCELLED`，不写快照、不改变最后发布版本。

## 6. Qt 工作台

`WorkbenchWindow` 使用 `QThread` + `QObject` worker 执行提交或回退，主线程只更新 UI。
同一会话只有一个任务，重复提交由引擎返回 `E_BUSY`。取消设置线程安全的
`threading.Event`，转换循环在帧边界检查并释放所有临时状态；帧读取始终使用上下文管理器，
不保留文件句柄。界面时间轴绘制当前版本受影响区间，版本列表支持差异对比和历史回退。

## 7. 确定性

样本由 `numpy.random.default_rng(seed)` 和纯本地解析图案生成。轨迹日志为规范化 JSONL，
不含时间戳、文件大小或绝对路径；内容真值全部来自 SHA-256。相同种子和操作序列下，
样本、各版本帧、manifest、快照、基线文件与轨迹日志逐字节一致。

## 8. 实际验证记录

验证环境：

- Python 3.11.18
- NumPy 2.4.6
- PySide6 6.12.0
- pytest 9.2.0
- Linux，Qt offscreen 与本地桌面会话创建均验证

执行结果：

```text
$ .venv/bin/python -m pytest -q
..........................                                               [100%]

$ .venv/bin/ruff check hdr_studio tests scripts
All checks passed!

$ .venv/bin/python -m hdr_studio.cli demo --workspace <A> --force
$ .venv/bin/python -m hdr_studio.cli demo --workspace <B> --force
$ diff -r <A> <B>
<无输出>

sample_set_hash: 8deb0f4063a5736dc742948cfa8b5adb2c99b35e8de41efa73edab21f16b2497
trace_hash:      960847c45b6afbe5524bb05166830ee3a226b150c29c289af9627f16225c5004
baseline_hash:   c6d3a35991312c549ce3878add5f143f0edd182b0270b24e2757330b0cd189e1
latest_output:   76c3610ce210d2f6236e6ade0f545e6f26d16e32bcb49b53066a19ff49fcba5c
```

强制终止测试在版本目录已原子提升、快照尚未替换时执行 `os._exit(97)`；重新打开后恢复到
版本 1，孤儿版本 2 被丢弃。删除已发布帧后恢复仅重算该缺失帧，并验证哈希与原快照一致。

