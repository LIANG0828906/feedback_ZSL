# 实际验证记录

验证日期：2026-11-23  
工作目录：`/home/liang/feedback/uid42a8691e-default-118`

## 环境

- 运行环境：WSL2 Linux，Windows 字体可从 `/mnt/c/Windows/Fonts/msyh.ttc` 加载。
- Python：3.11.18。
- 虚拟环境：`.venv`。
- NumPy 1.26.4。
- SciPy 1.11.4。
- PyQt5 5.15.10。
- pyqtgraph 0.13.3。
- pytest 8.0.2。
- 未使用 Docker、容器、外部音频或网络数据。

## 样本生成

命令：

```bash
python -m acoustic_workbench.samplegen
```

结果：

- 3 组，共 31 段四通道 IR。
- `short_single`：1 段，约 0.45 s。
- `medium_batch`：6 段，约 1.05–1.55 s。
- `long_many`：24 段，约 2.10–2.90 s。
- `samples/` 总大小约 54 MB。
- 清单包含采样率、四通道布局、逐通道直达延迟、早期反射能量、DRR 与反射明细。
- 生成器重复运行得到相同清单 JSON 与相同 `.npy` 字节，测试已覆盖。

## 自动化测试

命令：

```bash
python -m pytest -q
```

结果：

```text
..................                                                       [100%]
20 passed
```

静态检查：

```bash
ruff check acoustic_workbench tests scripts
```

结果：`All checks passed!`

## 一键验收

命令：

```bash
python -m acoustic_workbench.cli verify --parallel 2 --buffer-mb 16
```

实际摘要：

```json
{
  "ok": true,
  "sample_id": "short_single_000",
  "three_path": {
    "partial_committed_frames": 14,
    "output_sha256": "f8a05fb2aeecfd130df492a615b5d64b28fb0a2ab6d34856c3a2609060a82c51"
  },
  "injections": {
    "inject_missing_channel": "E_CHAN",
    "inject_order_inversion": "E_ORDER",
    "inject_gain_overflow": "E_CLIP",
    "inject_bad_frame": "E_FRAME"
  }
}
```

完整报告：`artifacts/verification_report.json`。

### 三路一致性

- 实时完整处理与“中断 14 帧后恢复完成”逐样本一致。
- 实时完整处理与“读取已持久化参数后全量复算”逐样本一致。
- 两次比对最大绝对差均为 `0.0`。
- 输出 SHA-256 相同。
- 逐帧直达延迟、直达/早期/混响能量、DRR 和方向统计完全相同。
- 2.11 秒 `long_many_000` 另做 50 帧、并行度 3 的三路验证，中断 17 帧后恢复，输出 SHA-256 为 `27c8227cb94d7f5136f89dbc9ce80ce775ffbb0a7b4c7e4b3b491adf3623fb34`，同样逐样本一致。

### 四类异常

| 注入用例 | 预期 | 实际 |
|---|---|---|
| 通道缺失 | `E_CHAN` | `E_CHAN` |
| 时序倒置 | `E_ORDER` | `E_ORDER` |
| 增益溢出 | `E_CLIP` | `E_CLIP` |
| 坏帧 | `E_FRAME` | `E_FRAME` |

坏帧用例在第 2 帧失败，失败时已完成 2 帧提交；移除注入后重试该帧并完成剩余帧，其他帧未受污染。

### 可变规模取舍

| 组 | 样本数 | 自动模式 | 实际估计窗 | 重叠率 | 首帧直达延迟最大误差 |
|---|---:|---|---:|---:|---:|
| `short_single` | 1 | calibration | 4096 | 0.875 | 0 samples |
| `medium_batch` | 6 | preview | 1024 | 0.500 | 0 samples |
| `long_many` | 24 | preview | 1024 | 0.500 | 0 samples |

预览与校准逐帧比对全部通过；默认容差为延迟 1.0 样本、能量 8%、DRR 0.75 dB。测试另外验证了人为扰动会触发 `E_TOL` 并定位首帧。

三组代表样本首帧的四通道早期反射能量与解析真值最大相对误差分别为 `2.11e-8`、`4.72e-8`、`3.99e-8`，直达延迟误差均为 0 样本。

## 检查点验证

- SQLite 单连接加锁，逐帧事务提交。
- 中断检查点恢复后，只加载连续已提交前缀。
- 人为篡改算法版本后，全部帧记录作废并记录原因；重启从 0 帧开始。
- 载荷摘要、输入哈希、算法版本和序列连续性均在加载时校验。
- 取消请求只保留已提交前缀，返回 `cancelled=True`，不生成完整报告。

## UI 冒烟

命令：

```bash
QT_QPA_PLATFORM=offscreen python scripts/check_ui.py
```

实际输出：

```text
UI_OK title=多通道声学测量与校准工作台 samples=31 size=1440x987 screenshot=.../artifacts/ui_smoke.png
```

窗口完成创建、显示、事件循环处理、样本载入、处理结果展示和截图。截图见 `artifacts/ui_smoke.png`。Windows 启动命令为：

```powershell
python -m acoustic_workbench.app
```

代码不含平台专用路径；Windows 会直接使用系统微软雅黑字体。当前自动化会话无物理桌面，因此以 offscreen 窗口渲染作为可启动性验证；试听在有音频设备的 Windows 桌面上使用 QtMultimedia，无音频设备时可导出 WAV。

## 结论

本地确定性样本下，三路输出逐样本一致；短/长规模取舍记录完整；预览与校准在声明容差内；四类异常注入均命中对应错误码；检查点恢复、非法状态保护和取消边界均通过验证。
