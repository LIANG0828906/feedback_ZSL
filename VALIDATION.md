# 实际验证记录

验证日期：2026-11-22。验证环境为本地 Linux/WSL，未使用 Docker。

## 环境

| 组件 | 版本 |
| --- | --- |
| Python | 3.12.16 |
| numpy | 2.3.5 |
| scipy | 1.16.3 |
| PyQt5 | 5.15.11 |
| Qt | 5.15.14 |
| soundfile | 0.13.1 |
| pytest | 9.0.1 |

虚拟环境创建与安装：

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements.txt
uv pip install --python .venv/bin/python -e . --no-deps
```

## 正式样本诊断

命令：

```bash
./scripts/run_demo.sh /home/liang/feedback/uid190c93df-default-73/validation/final
```

样本参数：种子 `42`，时长 `1.0 s`，48000 Hz，16-bit PCM，36 对源-拾取声道，6 个频段，单声道与 5.1 两套布局。

结果：

```text
status=ok
alignment_pairs=36
coupling_estimates=216
calibration_settings=216
calibration_feasible=true
calibration_publishable=true
short_budget_entries=0
enabled_cancellation_settings=46
cancellation_gain_range_db=0.0..3.8 (cap=12.0)
main_channel_unchanged=true
phase_findings: boundary_phase_wrap=3, polarity_reversed=1
```

与注入真值比较：

```text
max_arrival_error_samples=2
max_coupling_gain_error_db=0.10797690800400161
max_coupling_phase_error_deg=0.8039018714389954
```

状态与快照：

```text
state_hash=4ed420042fb12ffff99da4f72cdd1430d14e91e20a013c90356f7d7eb90e6dc8
snapshot_id=4ed420042fb12ffff99da4f7
route_matrix_sha256=da6491aca0e9a9f605f07c6c08c450784e06e594dceca097daf1671634cbdd7f
calibration_sha256=b9440509e31b9ad078b07cc950ba08b08df1a8e3402284bd8e594e2505d6a40a
trace_sha256=c8a1fc06307ab96cd463aafc13e2c8fa056970445d9bcd328b935ed2191bc60b
```

`restore` 输出中的 `snapshot_id`、`state_hash`、`route_matrix_sha256` 和 `calibration_sha256` 与上表完全一致，路由矩阵与校准设置文件逐字节相同。

## 确定性复跑

使用相同种子和相同操作序列输出到另一个快照根：

```bash
./scripts/run_demo.sh /home/liang/feedback/uid190c93df-default-73/validation/final_repeat
sha256sum validation/final/trace.jsonl validation/final_repeat/trace.jsonl
sha256sum validation/final/diagnosis.json validation/final_repeat/diagnosis.json
```

结果：

```text
trace c8a1fc06307ab96cd463aafc13e2c8fa056970445d9bcd328b935ed2191bc60b
trace c8a1fc06307ab96cd463aafc13e2c8fa056970445d9bcd328b935ed2191bc60b
diagnosis 432532f8d7ef3260999e5995c88a63bde176a54e453123d55fb483bc00c9b10d
diagnosis 432532f8d7ef3260999e5995c88a63bde176a54e453123d55fb483bc00c9b10d
TRACE_BYTE_IDENTICAL
DIAGNOSIS_BYTE_IDENTICAL
```

两次运行的快照 ID、状态哈希、路由哈希和校准哈希均相同。

## 5.1 布局

将同一配置的 `measurement_layout` 改为 `5.1` 后重新诊断：

```text
status=ok
state_hash=151eee1cae8ad085b8b58c118af66a01a991e692abb5910901bb77007a9e17b9
coupling_count=216
mono_vs_51_max_strength_or_wrapped_phase_delta=5.115907697472721e-13
```

单声道逐对 WAV 与六声道拾取文件中对应声道的样本逐值一致。

## 增量复算

目标抑制量从 `12` 改为 `13 dB`：

```text
changed_inputs=["target_suppression_db"]
alignment:    cache_hits=36, recomputed=0
coupling:     cache_hits=216, recomputed=0
route_matrix: cache_hits=6, recomputed=0
budget:       cache_hits=0, recomputed=6
phase:        cache_hits=36, recomputed=0
calibration:  cache_hits=0, recomputed=6
```

将 B2 上边界和 B3 下边界同时从 `2000 Hz` 改为 `1800 Hz`：

```text
changed_inputs=["band_boundaries"]
coupling:     cache_hits=144, recomputed=72
route_matrix: cache_hits=4, recomputed=2
budget:       cache_hits=4, recomputed=2
phase:        cache_hits=0, recomputed=36
calibration:  cache_hits=0, recomputed=6
affected_coupling_bands=[B2, B3]
cached_coupling_bands=[B0, B1, B4, B5]
```

相位节点因相邻频段连续性依赖而保守全量重算，状态中记录了受影响频段集合。

## 业务码验证

自动化测试实际触发并验证：

- `E_PAIR_MISSING`：删除 `FR|BL` 的 `distance_m`，错误定位到源 `FR`、拾取 `BL`。
- `E_PAIR_MISSING`：全零分量在整段无有效信号，错误定位到对应源/拾取对。
- `E_BAND_UNRESOLVED`：`min_band_samples=100000` 时在 `B0` 中止，并报告有效样本数。
- `E_BUDGET_SHORT`：目标 `40 dB`、深度上限 `2 dB` 时逐频段标注短缺，目标值未被改写。
- `E_CALIB_INFEASIBLE`：同一短缺场景下冲突频段和目标不可达原因被列出，`publishable=false`。
- `E_PHASE_AMBIGUOUS` 判定逻辑：卷绕场景仅产生 `boundary_phase_wrap`；180 度跳变保留真实反接/边界跳变/能量泄漏候选；低频群延迟跳变可触发歧义。
- `E_BUSY`：一个线程复算期间重复提交立即返回 `E_BUSY`。
- `E_CANCELLED`：取消后查询仍返回取消前的已提交状态。

## 快照与进程终止

提交协议验证：

- `SIGKILL` 在 `COMMITTED` 标记写入前终止诊断子进程。
- 子进程返回负退出码。
- 随后 `SnapshotStore.load_current()` 仍返回中止前最后一次已提交快照。
- 未完成临时目录不进入 `committed_ids()`。

协作文档与实现均不把进程内取消当作崩溃恢复验证。

## PyQt5 工作台

在 `QT_QPA_PLATFORM=offscreen` 下实际执行：

- 启动窗口并从快照恢复 `AnalysisState`。
- 六个频段切换时刷新路由热力、预算、相位和校准表。
- 后台 `QThread` 完成复算并通过信号更新主线程。
- 关闭后重新打开工作台，自动恢复刚提交的快照且状态哈希一致。

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest -q
```

实际结果：

```text
...................                                                      [100%]
19 passed
```

测试覆盖样本逐字节确定性、单/5.1 解码、独立主拾取 ID 映射、真值误差、业务码、相位分类、增量缓存、快照字节恢复、SIGKILL 恢复、并发取消和无显示 GUI。
