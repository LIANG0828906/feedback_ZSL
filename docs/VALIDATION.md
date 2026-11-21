# 实际验证记录

## 环境

```text
Python 3.14.4
NumPy 2.5.5
PyQt5 5.15.11
pytest 9.2.0
Linux 工作站，非 Docker/非容器
```

项目没有网络运行依赖；样本生成器和测试不访问外部资源。

## 自动化测试

执行：

```bash
python -m pytest -q
```

结果：

```text
............................. [100%]
29 passed
```

覆盖内容：

- 场景半径退化、奇异旋转矩阵、目标图尺寸不符的 `E_SCENE_INVALID` 字段定位；
- `world_to_camera` 外参换算；
- 同一分块布局下并行度 `1/2/4/7` 的梯度哈希完全一致；
- `2/3/4/6/16` 分块尺寸间梯度在 `atol=1e-12, rtol=1e-10` 内一致；
- 首个分歧分块编号与坐标定位；
- 解析梯度与有限差分对照；
- 依赖图受影响集、分块梯度复用、两类陈旧缓存否证与逐字节全量结果；
- 资源池稳定提交序、内存等待、分块降级和预算拒绝；
- 原子检查点续跑、哈希链断裂、内容篡改；
- `E_BUSY`、场景租约和 `E_STALE_HANDLE`；
- 样本生成与矩阵的确定性。

## 综合验收命令

```bash
python -m inverse_imaging.cli demo \
  --workdir /tmp/ii_accept/demo \
  --seed 7 \
  --report /tmp/ii_accept/acceptance_report.json
```

结果：

```json
{
  "ok": true,
  "manifest_hash": "b6d810869e055f68d45ccf5ed7506dc047513398da9c261dbc810336fc2b645d",
  "reduction_cases": 9,
  "max_abs_difference": 5.421010862427522e-19,
  "incremental_equivalent": true,
  "stale_reason": "stale_dependency:shading:chunk=0:upstream_version_or_content_changed",
  "cache_hash_reason": "cache_hash_mismatch:camera_rays:chunk=0",
  "trajectory_identical": true,
  "checkpoints_identical": true,
  "latest_hash": "6a38a9e094e102ac03588b91c857af8d251c81d578c414ae0a078a5042e78b7a",
  "budget": {
    "completed": ["first", "second", "third"],
    "rejected_budget_case": true,
    "reserved_memory_mb": 0.0,
    "stable_sequence": [0, 1, 2],
    "statuses": ["completed", "completed", "completed"]
  }
}
```

## 真实进程强制终止

执行序列：

1. 不中断执行 4 步到 `full/`；
2. 新进程执行 4 步，在提交第 1 步后调用 `os._exit(17)`；
3. 第三个进程使用同一 `part/` 检查点目录恢复并完成剩余步。

结果：

```text
full_exit=0
killed_exit=17
resume_exit=0
trajectory-identical
checkpoints-identical
full history_steps [0, 1, 2, 3]
part history_steps [0, 1, 2, 3]
```

恢复后 `trajectory.jsonl` 和 `checkpoints/` 下全部文件与不中断运行逐字节相同。

## 界面验证

当前环境没有物理桌面会话，因此使用 Qt 离屏平台完成构造级验证：

```bash
QT_QPA_PLATFORM=offscreen python ...
```

加载本地场景、创建 `QApplication`、构造主窗口和退出事件循环均成功，返回码 `0`。可见窗口需在 Windows 桌面会话中运行：

```powershell
python -m inverse_imaging.cli gui --scene path\to\scene.json
```

## 日志确定性

- 轨迹事件不包含时间戳、文件大小或线程完成顺序。
- 事件的真值来源是步号、参数版本、参数内容哈希、梯度内容哈希、分块归约哈希和固定归约顺序。
- 调度日志为每个事件附加规范 JSON 的内容 SHA-256。
- 相同种子、参数、操作序列、分块和并行度下，损失、梯度和轨迹逐字节一致。

