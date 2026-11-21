# 广播播出中心多声道素材响度计量流水线

本地确定性实现,无网络服务、无音视频编解码库依赖,仅 Python + NumPy。

## 运行

```bash
pip install numpy pytest
python3 -m loudmeter workdir      # 脚本化演示入口,输出到 workdir/
python3 -m pytest tests/ -q       # 测试套件
```

环境说明:本机仅提供 Python 3.14(任务建议 3.11,接口完全兼容),NumPy 2.5.5。

## 模块与业务要求映射

| 要求 | 模块 | 要点 |
|---|---|---|
| 1 WAV 读取/下混 | `loudmeter/wavfile.py` | 手工解析 RIFF/fmt/data,支持 PCM 与 WAVE_FORMAT_EXTENSIBLE;校验采样率 48k、6 声道、16/24 bit、block_align/byte_rate/data 长度一致性、5.1 声道掩码(0x3F/0x60F);任一矛盾抛 `E_WAV_INVALID`;`downmix()` 按声道权重下混 |
| 2 观测块切片/块级计值 | `loudmeter/blocks.py` | 可配置 `block_samples` 切片;BS.1770 K 加权(高架+RLB 两级 biquad,48 kHz 系数)后按声道权重求能量;短期响度;4 倍 FFT 过采样真峰值;每块记录窗口起止、逻辑同步序号(非系统时间)、内容 SHA-256;`reslice_diff()` 输出新旧块数差异与最大窗口偏移 |
| 3 两阶段门限积分 | `loudmeter/gating.py` | 阶段一:全块集未门限积分 → 相对门限(默认 -10 LU);阶段二:绝对门限(-70 LUFS)+相对门限筛选 → 门控积分与真峰值最大值;输出被滤除块标识与两阶段中间值;块集变化必须整体重算(见 `recompute.py`) |
| 4 并行调度器 | `loudmeter/scheduler.py` | 并行度与单任务内存预算仅来自 `MeterConfig`(JSON 配置文件),运行时提高抛 `E_CONFIG`;块任务跨素材并行、同素材按序提交、按提交序号稳定派发;内存累计超预算进入等待队列(`wait_budget` 轨迹);重复启动同一素材抛 `E_BUSY`;调度为确定性离散事件模拟,逻辑时钟推进,轨迹逐字节可复现 |
| 5 增量重算/门限联动失效 | `loudmeter/recompute.py` | 参数变化后计算受影响块集合并重算候选门限;仅当筛选集合不变且结构未变时走 `incremental`(只重算受影响块或复用块值);筛选集合改变、块长/过采样变化或无法证明未受影响时 `full` 整段重算;结果经 `VersionStore.publish` 一次整体替换发布 |
| 6 版本与查询隔离 | `loudmeter/versions.py` | 清单发布为不可变版本(递归冻结),单指针原子交换,发布期间旧版本持续可读;查询结果携带实际命中的版本哈希;哈希与发布清单不符抛 `E_VERSION_STALE`,废止句柄抛 `E_STALE_HANDLE` |
| 7 原子快照/恢复 | `loudmeter/snapshot.py` | 同目录临时文件 + `os.replace` 原子重命名;快照含块计值、门限中间值、参数版本与哈希链(`prev_hash`+内容哈希);载入逐环校验,断链/缺字段抛 `E_REPORT_MISMATCH`;`unfinished_blocks()` 只续算未提交块,不重复写入;恢复仅返回计量状态,不承诺生成退出报告 |
| 8 样本生成/演示 | `loudmeter/samplegen.py`、`loudmeter/demo.py` | 固定种子生成 16/24 bit、多声道内容与已知真峰值参考(0.5 幅度、峰值落在采样点之间的加窗突发,参考 -6.02 dBTP);演示输出 `trace.jsonl`(调度轨迹)、`blocks.jsonl`(素材标识/块序号/短期响度/门限状态/并行度/预算用量/内容哈希)、`report.json`;同步标记为逻辑序号,无系统时间 |

## 关键设计取舍

- **确定性优先**:调度器用确定性离散事件模拟(逻辑时钟 + 提交序号稳定排序)而非真实线程竞态,保证"相同种子与操作序列下报告与日志逐字节一致";并行度、槽位、预算用量均体现在轨迹中。
- **门限联动**:相对门限 = 未门限积分 - 10 LU,依赖整段块集;任何块重算后都重新推导两阶段结论,旧门限下的部分结果一律不复用(`recompute.py` 中 `selection_changed` 判定)。
- **真峰值**:4 倍 FFT 过采样,块边界加窗样本的周期性假设影响已在测试容差(0.3 dB)内验证。

## 实际验证结果(本机运行)

- `python3 -m pytest tests/ -q`:**41 passed**(WAV 校验 8、块计值 5、门限 4、调度 5、版本 5、快照 6、重算 4、端到端 3)。
- `python3 -m loudmeter workdir`:4 个素材全部完成计量,`recovery_consistent: true`。
- 真峰值参考样本 `tp_ref`:测得 -6.021 dBTP,参考 -6.0206 dBTP。
- `gated24`:静音块(约 -75 dBFS)被绝对门限滤除(`gated_out_block_ids: [4]`)。
- 块长 19200→9600 重切:`block_count_delta=5`,`max_window_offset_shift=48000`,模式 `full`。
- 门限 -10→-8 LU(选择集未变):模式 `incremental`,块值复用。
- 崩溃恢复:提交 2/5 块后强制终止,从最近有效快照续算,恢复清单版本哈希与不间断全量运行一致。
- 两次完整演示的 `trace.jsonl`/`blocks.jsonl`/`report.json` 逐字节一致(测试 `test_demo_determinism_byte_identical`)。
