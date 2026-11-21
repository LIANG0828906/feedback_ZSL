# fpkit 技术说明

可复现的音乐指纹建库与检索组件。纯本地运行：Python 3.11 + NumPy（Tkinter 随标准发行版可用），无外部服务、无网络、无容器。

## 模块结构

| 模块 | 职责 |
| --- | --- |
| `fpkit/wavio.py` | 16-bit PCM WAV 解析（单/双声道，8k–48kHz），不支持布局返回 `E_AUDIO_UNSUPPORTED` 并定位字段（`sample_rate` / `bits_per_sample` / `channels` / `compression` / `riff`） |
| `fpkit/fingerprint.py` | 确定性 STFT（Hann 窗、定长帧/移）→ 分带局部峰值 → 峰值对哈希（含频带标识），记录携带锚点时间偏移；`diff_summary` 输出重采样/窗参数变化后的新旧指纹差异摘要 |
| `fpkit/budget.py` | 内存计量模型与声明式降级序列 |
| `fpkit/scheduler.py` | 并行建库调度器：样本间并行、按样本序号稳定提交、等待队列、`E_BUSY` |
| `fpkit/library.py` | 指纹库：临时文件 + 原子重命名提交、哈希链清单、恢复校验 |
| `fpkit/search.py` | 哈希桶 + 时间偏移投票检索，绑定已发布库版本 |
| `fpkit/consistency.py` | 跨配置/跨运行逐条一致性比对与容差判定 |
| `fpkit/samplegen.py` | 确定性合成样本生成器（种子驱动，含已知相似片段与基线真值） |
| `fpkit/experiment.py` / `fpkit/cli.py` / `run_experiment.py` | 脚本化实验入口 |

## 指纹算法

- 帧长 2048、帧移 512、Hann 窗（`np.hanning`，完全确定）；幅谱取 `rfft` 模值。
- 频带 `[200,800)…[3200,6400]` Hz，按采样率折算 bin 区间；每帧每带取前 2 个局部极大值（幅值并列时取低 bin，确定性强约束）。
- 峰值对：锚点与目标区内（`dt∈[1,48]` 帧）最多 3 个后续峰值配对；`sha256(f1|f2|dt|band1|band2)[:16]` 为哈希，记录为 `[hash, t_offset, band]`。
- 指纹内容哈希 = 全部记录规范 JSON 的 sha256。指纹是 `(PCM, 采样率, 配置)` 的纯函数：降级阶段只改变内部缓冲（帧批大小、哈希表分块），输出逐字节不变（有测试断言）。

## 资源预算与降级序列

单任务内存按三部分计量（`fpkit/budget.py`）：

```
mem = BASE(64KiB) + frame_batch*(n_fft/2+1)*8   # 频谱缓冲
      + n_peaks*32                              # 峰值点集
      + min(n_hashes, hash_chunk)*48            # 哈希表临时占用
```

- 全局并行度、单任务预算、资源池预算只来自配置文件，运行时不得静默提高。
- 启动前按上界估计；超预算不启动，按声明序列逐级降级：stage 1 帧批 256→64，stage 2 →16，stage 3 再叠加哈希表分块 1024。每级估计与实际用量写入轨迹（`TASK_DEGRADE` / `TASK_COMMIT`）。
- 全部阶段仍超限 → `E_BUDGET_EXCEEDED`，该样本不写入任何指纹记录，其余样本继续。
- 调度按提交序号选取可运行任务；累计内存超池预算的任务进入等待队列并写 `TASK_WAIT` 轨迹；同一批次重复启动返回 `E_BUSY`（锁文件判定）。

## 原子入库、哈希链与恢复

- 每条样本记录先写 `lib/tmp/*.tmp` 再 `os.replace` 原子重命名为 `records/<sample_id>.fp.json`；随后清单 `manifest.json` 同样原子提交。
- 清单条目：样本标识、指纹内容哈希、批次序号、配置版本；`chain_hash = sha256(prev|sample_id|fp_hash|batch_seq|config_version)`，链头即库版本 `version_hash`。
- 查询前 `verify()` 重放哈希链并核对记录文件内容哈希，不一致返回 `E_LIB_MISMATCH`。
- 恢复（`recover()`）只依据磁盘状态：校验哈希链，断链/字段缺失以 `E_RECOVERY_INVALID` 中止并定位首个断链样本；清理陈旧锁与临时文件、删除未入链的孤儿记录；返回已提交样本集合，调度器跳过它们（`TASK_SKIP_COMMITTED`），只补算未提交样本，绝不重复写入。
- 强制终止模拟：子进程在提交 K 条后 `os._exit(137)`（无清理），恢复运行后库与不间断运行逐字节一致。

## 检索与投票

- 查询指纹逐哈希查桶，对 `(样本, 量化偏移)` 投票；量化基准 `time_quant` 与库不一致时返回 `E_ALIGN_MISMATCH`，该候选不计入投票。
- 候选按 `(-votes, sample_id, start_frame)` 确定性排序，返回起止时间（帧/秒）与置信度（得票/查询哈希数），并回带绑定的库版本哈希。
- 检索是库状态的纯函数，任意并行度下结果一致（有实验检查）。

## 一致性与可复现性

- 轨迹日志为 JSONL，只用逻辑序号 `seq` 排序，不含任何墙钟时间戳；相同种子 + 配置 + 操作序列下，样本、指纹库、轨迹日志、检索结果逐字节一致（实验检查 `byte_reproducible`、`recovery_trace_reproducible`）。
- 一致性报告（`reports/consistency_report.txt`）逐查询、逐候选列出 MATCH/DIFF，定位样本、偏移与配置版本，容差为声明的 `time_tolerance_frames`，不只给汇总数字。

## 运行方式

```bash
python3.11 -m venv .venv && .venv/bin/pip install numpy
.venv/bin/python run_experiment.py workdir/experiment 20261122   # 完整实验
.venv/bin/python -m unittest discover -s tests                   # 单元测试
.venv/bin/python -m fpkit.cli generate --workdir W --seed 42     # 单步：生成
.venv/bin/python -m fpkit.cli build    --workdir W               # 单步：建库
.venv/bin/python -m fpkit.cli query    --workdir W               # 单步：检索
```
