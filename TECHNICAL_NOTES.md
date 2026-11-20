# 脉冲响应深度噪声抑制与增强处理器 — 技术说明

## 环境

- Python 3.11.18（uv 管理的 CPython，venv 位于 `.venv/`）
- NumPy 2.4.6、PySide6 6.12.0（`requirements.txt`；GUI 不可用时使用 CLI 入口）
- 不使用 Docker/容器；样本、轨迹、结果均落盘于本地 `work/`

## 模块结构（`ir_enhancer/`）

| 模块 | 职责 |
| --- | --- |
| `errors.py` | 错误码：E_NOISE_EST_INVALID / E_ONSET_PROTECTED / E_ONSET_SHIFT / E_ONSET_GAIN / E_BUSY / E_RECOVERY_INVALID / E_CANCELLED / E_WAV_FORMAT |
| `config.py` | 不可变配置：谱参数（帧长、过减因子、增益下限、wiener/spectral_sub）、门限、保护窗口、频带表、声道/频带级覆盖；内容哈希 |
| `wavio.py` | 16-bit PCM WAV 确定性读写（48000 Hz，单声道/立体声/5.1），round-to-nearest-even 量化 |
| `noise.py` | 预触发区间噪声底/RMS/输入 SNR 估计与到达检测（阈值 = 6×噪声 RMS）；区间不足或全零 → E_NOISE_EST_INVALID（含声道号与区间） |
| `spectral.py` | 分帧加窗（周期 Hann，hop=N/2，COLA）→ 确定性 FFT → 按频带增益（Wiener/谱减，过减因子与增益下限可配）→ 重叠相加重建；输出每帧残余噪声能量与输入噪声底之比 |
| `gating.py` | 时域门限（置零或按比例衰减），保护窗口内样本不被修改；修改量超声明容差 → E_ONSET_PROTECTED（定位声道与样本） |
| `onset_check.py` | 直达声保护性恢复 + 不变性校验：重检测到达样本不一致 → E_ONSET_SHIFT；窗口峰值偏差超容差 → E_ONSET_GAIN；任一失败即中止不输出 |
| `metrics.py` | 逐声道逐频带 SNR 改善量与残余噪声能量（噪声/信号各自区间内带通，避免砖墙滤波 sinc 振铃泄漏）；劣化频带标注原因 |
| `engine.py` | 增量重算（声道级内容哈希缓存）、原子发布（版本目录 + current 符号链接整体替换）、原子导出（临时文件 + rename） |
| `snapshot.py` | 每声道完成后写快照，SHA-256 哈希链；恢复时校验链完整性，断链/字段缺失 → E_RECOVERY_INVALID，仅重算未完成声道 |
| `control.py` | TaskManager（同一测量仅一个任务，重复启动 E_BUSY）与 CancelToken（声道/频带边界协作式取消） |
| `samplegen.py` | 确定性样本生成器：已知噪声底、直达声位置（默认 4800）与混响尾的 5.1/单声道样本 + 真值 JSON |
| `cli.py` / `gui.py` | 无界面入口（gen-sample / run / verify / gui）与 PySide6 图形界面 |

## 关键设计

1. **直达声不变性**：谱域处理与门限可能改变保护窗口内样本，因此管线在门限之后执行
   保护性恢复（窗口内样本精确还原为输入基准），再对最终输出做到达位置与峰值幅度校验；
   重检测使用与输入相同的噪声 RMS 基准，保证检测器口径一致。
2. **增量重算**：缓存键 = SHA-256（输入声道哈希 + 该声道全部有效参数哈希 + 管线版本）。
   参数修改后仅受影响声道的键失效，未受影响声道复用缓存；重算完成后一次性原子发布，
   查询接口在替换期间始终读到完整旧版或完整新版。轨迹日志逐条记录
   `{channel, bands, action: reuse|recompute, cache_key}`。
3. **确定性**：全程 float64、固定帧格、固定种子（PCG64）、轨迹日志无时间戳（逻辑序号），
   相同种子与操作序列下输出 WAV 与轨迹日志逐字节一致。
4. **恢复**：快照哈希链 `hash = SHA256(seq, input_hash, config_hash, completed, prev_hash)`，
   首块 prev 为 GENESIS；恢复时逐块校验，任何断链/缺字段/哈希不匹配均以
   E_RECOVERY_INVALID 中止。

## 使用

```bash
# 环境（离线 wheel 可用时）
uv venv --python 3.11 .venv && uv pip install --python .venv/bin/python -r requirements.txt

# 生成确定性样本（5.1，含真值）
.venv/bin/python -m ir_enhancer gen-sample --seed 1 --out work/samples

# 增强并原子导出（--resume 从最近有效快照恢复；--crash-after-channel 用于故障演练）
.venv/bin/python -m ir_enhancer run --in work/samples/sample_seed1.wav --workdir work/run --out work/export

# 验收校验：直达声位置不变 + 噪声能量下降
.venv/bin/python -m ir_enhancer verify --in work/samples/sample_seed1.wav --enhanced work/export/enhanced.wav

# 测试
.venv/bin/python -m unittest discover -s tests
```

## 实际验证结果（本机，2026-11-21）

- `unittest`：18 个测试全部通过（噪声估计、门限保护、不变性、端到端、增量、恢复、并发、确定性、导出）。
- 端到端（seed=1，5.1，1 s）：直达声到达样本 6 声道均为 4800（输入=增强）；
  预触发区噪声能量下降 14.6–15.8 dB；报告 `degraded_bands` 为空。
- 增量重算：修改声道 2（谱+门限）与声道 5（保护窗口）后，轨迹显示仅 ch2/ch5 为
  `recompute`，其余 `reuse`；增量结果与全新离线整段处理 `assert_array_equal` 逐样本一致。
- 恢复：子进程在声道 2 后 `os._exit(1)` 模拟强杀，`--resume` 后结果与无崩溃运行逐样本一致；
  删除链中间快照或删除字段均以 E_RECOVERY_INVALID 拒绝。
- 确定性：相同种子两次运行，样本 WAV、增强 WAV 与轨迹日志哈希逐字节一致。
