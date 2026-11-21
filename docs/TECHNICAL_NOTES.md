# SyncBench 多通道漂移同步工作台 — 技术说明

## 总览

`syncbench` 实现从多通道 WAV 读取到同步拼接的全链路：

```
WAV 读取 → 标记检测 → 分段时延估计 → 漂移拟合 → 诊断 → 重采样补偿 → 拼接落盘
```

包结构（`syncbench/`）：

- `wavio.py` — 逐通道 16-bit PCM WAV 读取（校验 48 kHz / PCM_16 / 单声道），多通道原子写。
- `generator.py` — 确定性样本生成器；真值写入 `truth.json`（`truth_source=generator_truth`）。
- `markers.py` — 归一化匹配滤波标记检测，输出 `{index, sample, confidence}` 与不可用列表。
- `delay.py` — 窗式互相关时延估计（逐标记窗口 + 分段中位数聚合）。
- `fit.py` — 一阶漂移模型最小二乘拟合（offset + slope），含 2σ 置信区间与拟合差分。
- `resample.py` — 分数延迟重采样：`linear` 与 `sinc`（Hann 窗 sinc，32 抽头）两种确定性模式。
- `diagnose.py` — 漂移来源诊断：CLOCK_SKEW / EVENT_JUMP / E_DRIFT_UNKNOWN。
- `stitch.py` — 分段补偿缓存（内容哈希键）与全通道对齐拼接（失败整体不落盘）。
- `pipeline.py` — 编排：策略选择、增量重算、双模式对比、报告与轨迹输出。
- `report.py` — JSON 报告：临时文件 + 原子重命名；版本与哈希链校验（E_REPORT_MISMATCH）。
- `trace.py` — 确定性轨迹日志：序号递增、无时间戳，代价用操作计数（估计 FLOPs）表示。
- `gui.py` — PySide6 诊断工作台（后台 QThread，E_BUSY，可取消）。
- `cli.py` — `generate` / `run` / `gui` 三个子命令。

## 关键算法决策

### 标记脉冲
使用固定种子的宽带噪声突发（128 样本，Hann 窗，单位能量）。早期版本使用 4 kHz
单音脉冲，其自相关以 12 样本为周期振荡，导致互相关周期模糊（整段延迟错到
1807 样本）；宽带突发的图钉形自相关消除了该模糊。

### 分段时延估计
对参考通道每个标记开 1024 样本窗做互相关（搜索 ±max_lag），抛物线插值到亚样本；
分段延迟取窗内各标记延迟的中位数，并赋到**标记位置均值**（而非分段几何中心——
后者会引入与漂移成正比的拟合偏差）。分段置信度取窗内最小值；低于
`min_seg_conf`（默认 5.0）或窗内无标记的分段标记为 `valid=False`，不插值。
实测：有效分段置信度 ≥ 15，缺失/低 SNR 段 ≤ 4.5，分离充分。

### 漂移诊断
1. 线性拟合残差最大值 ≤ 阈值（默认 2 样本）→ `CLOCK_SKEW`，输出 ppm 与置信区间。
2. 否则做**单跳变搜索**：遍历切分点并允许排除一个过渡分段（跳变可能落在分段
   内部，使该分段混入跳变两侧标记）；若最佳分段线性拟合 RMS ≤ 阈值且跳变量
   显著（> max(阈值, 6·MADσ)）→ `EVENT_JUMP`，输出跳变分段、跳变量与证据分段；
   平手时取较晚切分点（过渡段归跳变前侧）。
3. 以上均不成立 → `E_DRIFT_UNKNOWN`，输出候选假设
   （`multiple_event_jumps` / `nonlinear_clock_or_noise` / `unmodelled_drift`）
   与残差统计，绝不默认归为频率偏差。

### 重采样补偿
按拟合模型 `p(n) = offset + (1 + slope)·n` 做分数延迟读取。`sinc` 模式为 Hann 窗
sinc（每侧 16 抽头），`linear` 为线性插值；两种模式均确定性（同输入同输出，
测试含逐样本相等断言）。补偿后以参考通道标记为基准计算对齐误差，
声明容差 `align_tolerance`（默认 1.0 样本）。

### 数据规模自适应
`duration > strategy_threshold_s`（默认 10 s）→ `segmented_fit`；否则
`whole_xcorr`（整段单次互相关、常量延迟补偿）。策略与分段数、确定性代价估计
（相关长度 × log 样本数）写入轨迹；两种策略始终在同一素材上各算一次残差用于
对比，不做任何截断或检测精度降级。实测 skew4：分段策略残差 RMS 0.026 样本，
整段策略 1012 样本（漂移使整段相关峰涂抹失效——正是切换分段的理由）。

### 增量重算
补偿按输出分段缓存，键 = (通道, 分段号, 输入内容哈希, 参数哈希(offset/slope/
mode/seg_len))。通道数、分段长度、参考通道或插值模式变化只使对应键失效；
未受影响分段命中缓存（测试断言命中数与重算数）。拼接取各通道公共长度截齐，
任一通道为空或含非有限值则抛 `E_STITCH_MISMATCH`，全部通道不落盘。

### 持久化与确定性
报告记录逐条哈希链（`hash = sha256(prev_hash + canonical_json)`），载入校验版本
与整条链，任一不匹配抛 `E_REPORT_MISMATCH`。保存经 `*.tmp` + `os.replace`。
轨迹日志无时间戳，同种子同操作序列下报告与日志逐字节一致
（`test_deterministic_report_and_trace`）。

## 错误码

- `E_BUSY` — 同一素材已有同步任务在跑（GUI 与 `SyncSession` 双重防护）。
- `E_DRIFT_UNKNOWN` — 漂移模式无法归类（附候选假设与残差统计）。
- `E_REPORT_MISMATCH` — 报告版本或哈希链校验失败。
- `E_STITCH_MISMATCH` — 拼接对齐失败，全部通道不落盘。
- `E_MARKER_LOW_SNR` — 标记信噪比不足（检测输出中标记为 unavailable）。
- `E_CONFIG` — 配置非法（采样率/位深/参考通道越界等）。
- `E_CANCELLED` — 用户取消；会话锁释放，保留最后一次已提交结果。

## 运行方式

```bash
python3 -m syncbench.cli generate --data-dir data
python3 -m syncbench.cli run --data-dir data --dataset skew4 \
    --out-dir reports/skew4 --logs-dir logs --seg-len 2.0 --mode linear
python3 -m syncbench.cli gui --data-dir data --dataset skew4   # 需显示环境
python3 -m unittest discover -s tests -t .                      # 33 项测试
```

## 实测验证结果（48 kHz × 24 s × 4 通道套件）

| 数据集 | 注入 | 诊断结论 | 补偿后标记对齐误差 max |
|---|---|---|---|
| skew4 | ch1-3: +40/-25/+60 ppm | CLOCK_SKEW ×3（ppm 误差 < 2） | 0.071 (linear) / 0.086 (sinc) 样本 |
| jump4 | ch2: marker 12 处 +40 样本阶跃 | EVENT_JUMP，jump_segment=6，jump_size≈40 | 18.9（线性模型不可补偿，如实报告） |
| degraded4 | ch1 缺标记 5-7，ch2 标记 10-14 低 SNR | 不可用标记/分段如实标记，CLOCK_SKEW | 0.082 / 0.104 样本 |
| unknown4 | ch1 五次交替阶跃 | E_DRIFT_UNKNOWN（multiple_event_jumps） | 14.1（不默认归为频率偏差） |

- 拟合精度：ppm 误差 < 2，offset 误差 < 1 样本，分段拟合残差 RMS ≤ 0.24。
- 分段长度 2 s → 4 s 重估计：参数差分自动输出（fit_diff），长分段因段内漂移
  涂抹置信度下降，无效分段被排除而非插值。
- 增量重算：同配置二次运行全部命中缓存；改参考通道后仅受影响分段重算。
- GUI（离屏验证）：后台线程完成同步，运行中重复提交返回 E_BUSY，取消后
  会话锁释放并保留最后结果。
- 确定性：同种子重生成样本逐字节一致；同操作序列报告与轨迹日志逐字节一致。

轨迹日志样例见 `logs/*.trace.log`，基线报告见 `reports/*/report.json`。
