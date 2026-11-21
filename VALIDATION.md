# 实际验证记录

验证环境：Python 3.12.16、NumPy 2.5.5、PySide6 6.12.0。当前执行环境为本地 Linux/WSL；实现仅使用 Python 标准库、NumPy 和 PySide6 的跨平台 API，不依赖 Docker。

## 自动化测试

命令：

```bash
python -m unittest discover -v
```

结果：19/19 通过。

- 统一时基：16k、44.1k、48k 及 24k/32k/48k 输出样本边界均为整数。
- 跨档并行：准备函数用会合屏障确认至少两个不同档位同时进入 worker，否则测试失败。
- 样本生成器：相同 seed 两次生成的全部样本文件与 `baseline.json` 逐字节一致。
- 非法 WAV/帧输入：均返回 `E_TIMEBASE_INVALID`。
- 多档切片：3 档 × 12 片，`verify` 无问题。
- 调度轨迹：标准运行出现 `memory_wait`，且预算降级事件包含各阶段估算。
- 预算样本：`degrade_auto` 完成 6 片，记录到 `minimal_buffers` 降级；两个预算不足档位返回 `E_BUDGET_EXCEEDED`，未产生后续切片。
- 提交失败：故障档位停在第 2 片且无第 3 片目录，其他档位完成 10/10。
- 切换：样例产生 5 次合法切换，所有切换边界音视频起始 tick 与边界 tick 相等，缓冲字节未超限。
- 缺口：人为截断索引后模拟返回 `E_SWITCH_INVALID`，轨迹 `valid=false`。
- 时间戳篡改：边界 0 的音视频起始 tick 被改后返回 `E_SWITCH_INVALID`，冲突字段包含三个起始时间戳字段。
- 配置变更：修改档位增益后加载返回 `E_SNAPSHOT_MISMATCH`。
- 边界表变更：修改 `boundaries.json` 后加载返回 `E_SNAPSHOT_MISMATCH`。
- 快照链篡改：修改快照中切片 `chain_hash` 后加载返回 `E_SNAPSHOT_MISMATCH`。
- `E_BUSY`：首个档位仍在准备时重复启动返回 `E_BUSY`。
- 协作取消：准备中取消后 `status=cancelled`、无失败档位、无未提交切片，最近快照可重新加载。
- 边界暂停：暂停后最多保留已完成的当前边界切片；再次运行从已提交索引继续并完成。
- 强制终止：第 4 次原子提交后 `os._exit(137)`；无 `exit_report.json`；恢复后全部切片/索引与干净运行逐字节一致。
- 确定性：两次相同 44.1k 运行共 85 个输出文件（含切片、索引、切换轨迹、日志）哈希完全一致。

## 手工验收命令与结果

### 标准 48 kHz 双声道 / 3 档 / 12 片

```bash
python -m livetranscode run \
  --workspace /tmp/lt_run_2 \
  --sample /tmp/lt_samples/esports_48k_stereo \
  --clean
python -m livetranscode verify --workspace /tmp/lt_run_2
```

结果：

- `status=complete`
- 3 档均 `12/12`
- 36 个切片任务发布
- 切换模拟 `valid=true`，`switch_count=5`
- `verify: ok=true, problems=[]`

### 预算降级与失败隔离

```bash
python -m livetranscode run \
  --workspace /tmp/lt_budget \
  --sample /tmp/lt_samples/budget_degrade \
  --clean
```

结果：

- 退出码 7（存在 `E_BUDGET_EXCEEDED` 档位）。
- `degrade_auto` 6/6，完整落盘。
- `degrade_min` 与 `reject_tiny` 在 slice 0 被拒绝。
- 轨迹包含每档每阶段的 `estimated_bytes`、`limit_bytes`、`components` 和 `fits`。
- 未提交档位没有数字切片目录。
- 合法档位仍可完成切换模拟，`valid=true`。

### 强制终止与恢复

```bash
python -m livetranscode run \
  --workspace /tmp/lt_crash2 \
  --sample /tmp/lt_samples/esports_48k_stereo \
  --clean \
  --crash-after-commits 5

python -m livetranscode resume --workspace /tmp/lt_crash2
python -m livetranscode verify --workspace /tmp/lt_crash2
```

结果：

- 强制终止退出码 137。
- `exit_report.json` 不存在。
- 终止时各档只有磁盘索引中已提交的前缀，例如 `high=1, low=2, medium=2`。
- 恢复后所有档位 12/12，`verify.ok=true`。
- 与未中断运行比较，除快照/日志/退出报告外，所有切片文件和索引哈希差异为 0。

### GUI 烟雾测试

```bash
QT_QPA_PLATFORM=offscreen python - <<'PY'
from PySide6.QtWidgets import QApplication
from livetranscode.ui import MainWindow
app = QApplication([])
window = MainWindow("/tmp/lt_ui_smoke", "/tmp/lt_samples/voice_16k_mono", None)
window.show()
print(window.table.rowCount(), len(window.engine.config.tracks))
PY
```

结果：表格显示 2 个档位，窗口可构造/关闭；同一切片引擎支持开始、暂停、取消与快照恢复按钮。

