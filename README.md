# 8-Channel Clock Drift Workbench

本仓库实现一套可复算的 8 路麦克风阵列时钟漂移分析与补偿工作台。它读取多声道
16-bit PCM WAV 和本地同步标记 JSON，完成标记检测、分段互相关、一阶漂移/阶跃
诊断、分数延迟重采样、多通道对齐拼接、残差热力输出，以及 PySide6 可视化诊断。

实现不依赖真实声卡、Docker 或外部服务。所有验收素材均由本地确定性合成器注入
已知的初始偏移、ppm 漂移、标记缺失和阶跃。

## 环境

依赖已固定在以下版本：

- CPython `3.11.18`（`.python-version`）
- NumPy `2.2.6`
- PySide6 `6.8.3`
- pytest `8.3.5`（仅测试）

`pyproject.toml`、`requirements.txt`、`requirements-dev.txt` 和 `uv.lock` 均记录
了上述约束。首次准备：

```bash
./scripts/bootstrap.sh
source .venv/bin/activate
```

脚本使用本地 `uv` 安装 CPython 3.11.18，创建 `.venv`，安装固定依赖和 editable
项目。当前工作区已经完成该步骤，可直接使用 `.venv/bin/python`。

## 快速运行

生成 20 秒 8 通道样本并执行一次同步：

```bash
track-sync demo --out artifacts/demo --duration 20 --channels 8
```

分别生成和运行：

```bash
track-sync synth --out artifacts/sample --duration 30 --channels 8 --seed 20261122
track-sync run \
  --wav artifacts/sample/drift_8ch.wav \
  --markers artifacts/sample/markers.json \
  --out artifacts/run \
  --segment-seconds 2 \
  --reference 0 \
  --interpolation linear
```

启动桌面工作台：

```bash
track-sync-gui \
  --wav artifacts/sample/drift_8ch.wav \
  --markers artifacts/sample/markers.json \
  --output artifacts/gui
```

无图形环境可用离屏方式做构造检查：

```bash
QT_QPA_PLATFORM=offscreen track-sync-gui --output artifacts/gui
```

### 策略与插值对比

`compare` 在同一 WAV、同一标记描述上运行：

1. `segmented + linear`
2. `segmented + windowed_sinc`
3. `full + linear`

并写出策略残差、两种插值残差、样本数/标记检测不变量到
`<out>/comparison.json`：

```bash
track-sync compare \
  --wav artifacts/sample/drift_8ch.wav \
  --markers artifacts/sample/markers.json \
  --out artifacts/compare \
  --segment-seconds 2
```

### 版本恢复

```bash
track-sync recover --out artifacts/run
```

恢复只读取 `reports/latest.json` 指向、且通过内容哈希校验的已提交报告。进程强制
终止留下的临时文件或孤立报告不会被当作成功结果。

## 标记描述

推荐使用周期字段：

```json
{
  "sample_rate": 48000,
  "channel_count": 8,
  "marker": {
    "first_marker_sample": 19200,
    "period_samples": 24000,
    "marker_count": 50,
    "pulse_width_samples": 192,
    "search_radius_samples": 900,
    "min_confidence": 0.3,
    "template": [-0.0, 0.01, 0.04, "..."]
  }
}
```

也可直接给出严格递增的 `marker_samples` / `expected_samples` 数组。`template`
是可选标记波形；提供时使用确定性的本地匹配滤波，未提供时回退到局部移动 RMS
能量包络。检测窗口按标记序号独立处理，因此缺失标记不会被后一个标记静默顶替。

任一标记未达到置信度或 SNR 阈值时，报告包含：

```json
{
  "code": "E_MARKER_MISSING",
  "details": {"channel": 6, "marker_index": 13, "stage": "source"}
}
```

该通道仍保留完整结果对象，不会从通道列表消失。

## 处理流程

1. **WAV 读取**：校验 RIFF/WAVE、16-bit PCM、采样率和通道数，以只读
   `numpy.memmap` 访问 PCM，避免先复制整段音频。
2. **标记检测**：对每个已知标记窗口做模板匹配或能量包络检测，输出到达样本、
   置信度、峰值相关度和缺失序号。
3. **策略选择**：`auto` 在录音时长或启用通道数达到阈值时选择分段估计；也允许
   强制 `full` 或 `segmented`。策略切换不改变素材长度，也不改变标记检测参数。
4. **时延估计**：FFT 互相关后在最大时延范围内搜索峰值，抛物线插值得到分数样本
   时延；由峰值曲率和相关质量给出不确定区间。
5. **漂移拟合**：加权最小二乘拟合
   `delay(n) = offset + rate_ppm * 1e-6 * n`。连续分段用线性模型；另以
   `linear + step` 候选模型和 BIC 增益检测阶跃。证据不足或残差不符合两类模型时
   输出 `E_DRIFT_UNKNOWN` 及候选解释。
6. **补偿**：按 `y[n] = x[n + delay(n)]` 做确定性分数延迟插值，支持：
   - `linear`：两点线性插值，速度快；
   - `windowed_sinc`：Lanczos 窗 sinc，默认 8 个半抽头。

   两种模式均逐段处理、保持输出样本数不变，并支持阶跃模型。
7. **标记复检**：对补偿后的 WAV 重新执行完全相同的标记检测，以实际标记残差验证
   容差，而不是只报告模型拟合误差。
8. **事务式拼接**：按时间顺序逐段写入临时 WAV；只有全部通道、全部分段都成功才
   `os.replace` 发布。任一分段失败会删除临时 WAV 和本次新写补偿缓存，不留下半
   对齐的正式文件。
9. **原子报告发布**：先写内容寻址报告，再原子替换 `latest.json`，最后在进程内
   切换 `Snapshot` 指针。查询只会看到完整旧版或完整新版。

## 缓存与增量重算

缓存位于 `<out>/cache`，全部条目使用内容键和原子替换：

- `markers/`：`源 WAV 哈希 + 通道 + 标记描述 + SNR 参数`
- `delays/`：`源哈希 + 通道 + 参考通道 + 策略 + 分段边界 + 互相关参数`
- `fits/`：`时延序列 + 拟合参数 + 采样率`
- `segments/`：`源哈希 + 通道 + 分段边界 + 漂移模型 + 插值模式/抽头数`

因此：

- 只改插值模式：复用标记、时延和漂移拟合，仅重算补偿分段。
- 只改漂移拟合参数：复用标记和时延，重算拟合及受影响补偿。
- 只增加通道：已有通道全部复用，仅检测/估计/补偿新增通道。
- 改分段长度或参考通道：受影响通道的时延、拟合和补偿重算。

每个补偿分段在报告中公开 SHA-256 内容哈希。缓存命中时会重新校验 NPZ 内保存的
哈希；不一致按 miss 处理。`cache.stats` 给出 hit/miss 和复用字节数。

## 输出

```text
<out>/
  cache/
    markers/ delays/ fits/ segments/
  reports/
    latest.json
    v000001-<hash>.json
  runs/<run-id>/
    aligned.wav
    residual_heatmap.npz
    summary.json
  comparison.json            # compare 命令
```

`residual_heatmap.npz` 包含：

- `channel_ids`
- `segment_starts`, `segment_ends`
- `before_rms`, `after_rms`
- `before_max`, `after_max`
- `marker_counts_before`, `marker_counts_after`

报告还包含策略耗时依据、预计/实测残差、逐通道漂移模型与置信区间、证据分段、
标记缺失、缓存统计、分段内容哈希和新旧时延序列差异摘要及最大偏差位置。

## 并发和取消

`SyncService.submit()` 立即返回后台任务状态。同一 WAV 文件（按设备/inode/大小/
mtime 识别）同时只允许一个任务，重复提交抛出 `E_BUSY`。取消请求会设置事件，
任务只在标记通道、时延分段、漂移通道、补偿分段或残差检查点之间停止；WAV 读取器
和写入器均通过上下文管理器关闭。取消或失败不会发布新版本。

UI 使用 180 ms 定时器查询任务状态，DSP 全在后台线程执行，因此主线程不阻塞。

## 验证

自动化测试：

```bash
.venv/bin/python -m pytest -q
```

覆盖标记检测与缺失、分段时延、线性/阶跃/未知漂移、线性与 sinc 补偿、事务失败、
两种策略对比、缓存作用域、RIFF/RF64 WAV、E_BUSY、取消、版本哈希及 UI 离屏
构造。当前 15 项测试全部通过。

完整验收：

```bash
./scripts/acceptance.sh --out artifacts/acceptance_run
```

本次实际运行报告保存在 `artifacts/acceptance_report.json`。素材为本地合成的
24 秒、8 通道、48 kHz、16-bit PCM WAV，注入 ppm 漂移，并在通道 6 的标记 13
注入缺失；另有 16 秒 4 通道阶跃素材。

实测结果：

- 验收状态：`PASS`
- 自动策略：`segmented`，12 个分段
- 线性插值补偿后最坏标记残差：`0.523` 样本（容差 `1.5`）
- Lanczos 窗 sinc 补偿后最坏标记残差：`0.523` 样本
- 强制整段互相关最坏残差：`79.705` 样本，证明长漂移下分段+模型拟合必要
- 第二次相同参数运行：标记 8/8、分段时延 96/96、拟合 8/8、补偿分段 96/96
  全部缓存命中，复用 `18,432,000` 字节；分段内容哈希完全一致
- 阶跃诊断：通道 1 识别为 `step_jump`，估计跳变 `12.019` 样本（注入 `12`）
- 三种运行的输入样本数、输出样本数和标记检测签名完全一致

## 规模与限制

- `auto` 策略对长录音或高通道数使用分段 FFT，以避免整段相关的时间与内存开销；
  强制 `full` 仍可用于同素材对照。补偿输出按分段流式写盘，不要求先拼接整段。
- 读取器支持 `RIFF/WAVE`、`RF64`、`WAVE_FORMAT_PCM` 与
  `WAVE_FORMAT_EXTENSIBLE` 的 16-bit PCM。补偿输出超过 RIFF 32-bit
  尺寸上限时自动写 RF64，因此数小时多通道结果无需先切成多个文件。
- 标记模板匹配比纯能量峰值更稳健；没有模板且背景含强窄带瞬态时，应缩小
  `search_radius_samples` 或提供模板。
