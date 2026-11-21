# 验证记录

验证日期：2026-11-22

## 环境

```text
Python 3.14.4
NumPy 2.5.5
PySide6 6.12.0
Qt offscreen platform for GUI smoke test
```

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen python -m unittest discover -s tests -v
```

实际结果：

```text
Ran 24 tests in 10.353s

OK
```

覆盖点：

- 单声道和 5.1 WAVE_FORMAT_EXTENSIBLE 往返读写。
- 5.1 确定性样本直达声到达样本与生成器真值完全一致。
- 参考声道/门限变化后的到达位置、偏移变化摘要。
- 倍频程/三分之一倍频程频带聚合、分段数、相干增益和误差上界。
- 线性相位 FIR 对称性、4095 点系数、群延迟 `(N-1)/2`。
- 非法阶数、非法目标曲线的 `E_FILTER_INVALID`，以及失败提交保留上一滤波器。
- 误差传播节点、阈值跨阈归因和证据频带。
- 版本单调递增、内容哈希、任意版本差异、只追加回退、哈希链校验。
- 仅改误差阈值时复用频谱、平滑、FIR 与节点哈希。
- 目标曲线仅改一个频带时只重算该频带滤波器/误差节点。
- `E_VERSION_STALE` 父哈希保护。
- 已发布版本内容被篡改时返回 `E_VERSION_STALE`。
- `E_BUSY`、检查点取消、最后发布版本不变。
- 并发查询期间只观察到完整 6 声道/24 频带版本。
- 取消后新进程式引擎从磁盘恢复，复用已提交声道，只重算其余声道。
- 首次运行尚无发布版本时取消，也能从快照恢复并发布 v1。
- 子进程在首声道检查点后真实 `SIGKILL`，新引擎仅凭磁盘 manifest/段恢复。
- 恢复结果与独立工作区全量重算的到达、频谱、平滑、FIR 系数和误差逐值一致。
- 快照段内容篡改返回 `E_RECOVERY_INVALID` 并定位第一个段区间。
- 快照 manifest 缺字段返回 `E_RECOVERY_INVALID`。
- 恢复幂等：已发布 run 再次恢复不再追加版本。
- Qt 离屏工作台启动、后台发布并填充表格与绘图控件。

## 端到端 CLI 验收

命令：

```bash
python -m acoustic_calibration.cli verify --workspace artifacts/verification
```

实际关键输出：

```json
{
  "arrivals_match_ground_truth": true,
  "version_chain_valid": true,
  "rollback_appended": true,
  "forced_cancel_preserved_version": true,
  "recovery_reused_channels": [0],
  "recovery_recomputed_channels": [1, 2, 3, 4, 5],
  "recovered_artifacts_equal_clean_run": true
}
```

版本演进统计：

| 步骤 | 版本 | 重算节点数 | 说明 |
| --- | ---: | ---: | --- |
| baseline | 1 | 468 | 全量 |
| reference + threshold | 2 | 468 | 参考/门限变化全量重对齐 |
| smoothing 1/3 → 1/6 oct | 3 | 318 | 不重算频谱节点 |
| local target point | 4 | 12 | 仅一个目标频带的 6 个 FIR + 6 组误差节点 |
| error threshold | 5 | 144 | 仅误差节点；滤波器系数逐值不变 |
| rollback to v1 | 6 | 0 | 追加新版本，产物复制自 v1 |
| crash recovery | 7 | 394 | 复用声道 0，重算声道 1–5 |

恢复后的 v7 与独立工作区全量重算结果：

```text
arrival / spectral / smoothing / FIR / error / consistency 逐值一致
```

## 图形工作台冒烟

自动化离屏实例化验证：

- 自动生成确定性 5.1 样本。
- 后台线程完成首版校准。
- 主线程保持响应，轮询发布事件。
- 声道表 6 行，版本表至少 1 行。
- 能量曲线、误差上界包络和 FIR 响应画布正常绘制。

## 约束确认

- 未使用 Docker、Podman 或其他容器。
- 未访问外部音频源或未授权目标。
- 样本由固定 seed 的 NumPy 随机源本地生成。
- 版本、快照和轨迹全部落在工作区磁盘。
