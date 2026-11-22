# 实际验证记录

验证日期：2026-11-23（Asia/Shanghai）

## 环境

- Python 3.11.18
- PySide6 6.6.3.1
- NumPy 1.26.4
- WSL2 Ubuntu 26.04，内核 `6.18.33.1-microsoft-standard-WSL2`
- 宿主 Windows 图形会话在 WSL 中映射为 `DISPLAY=:0`；界面另用 PySide6 `QT_QPA_PLATFORM=offscreen` 做可重复截图
- 所有 wheel 来自本机离线 wheelhouse，未访问网络服务

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -v
```

结果：

```text
Ran 42 tests in 11.713s
OK
```

覆盖项：

- `E_SEQ` 帧跳号、`E_CHAN` 通道越界、`E_PARA` 强度/间隔/版本非法；
- 合并、禁止打断、优先级打断和全事件决策；
- 串行/有界并行调度与编码逐字节、逐决策一致；
- 增量优先级/能力/增删事件失效范围及未受影响包复用；
- HTP1 重放一致、CRC、`E_PACK` 包上限/时序窗口；
- 非零窗口下界时事件偏移从下界起算并保持落在窗口内；
- `E_STATE` 非法迁移保持状态，失败包可重试；
- 回环丢包、重发、超时、乱序分类；
- `E_LOCAL` 本地包序倒置；
- 单包重试不污染其他包记录与统计；
- 检查点哈希链、版本 mismatch 整体作废、全量重放；
- `E_DIVERGE` 首个包、通道和字段定位；
- 中途退出后 3 个持久化窗口续跑，与完整实时路径逐包一致；
- 增量编辑后直接恢复会拒用旧事件流快照并全量重放；
- 非连续窗口写入被拒绝且不落盘；
- 检查点冗余元数据列与哈希载荷不一致时整库作废；
- PySide6 工作台四区渲染烟雾测试。

完整日志：`artifacts/test-run.log`

## 三路径验收

命令：

```bash
.venv/bin/python -m haptics verify \
  --workdir artifacts/acceptance \
  --parallel --max-workers 3 \
  --max-events-per-task 7 --max-packets-per-task 3 \
  --json-out artifacts/verification.json
```

摘要：

```json
{
  "ok": true,
  "sample_hash": "b50610c139af58dafb477b1780dedf8243602575493d972d47b3c7b4227f9878",
  "packet_count": 11,
  "decision_count": 26,
  "service_codes": ["E_DUP", "E_MISS", "E_REORDER", "E_TIMEOUT"],
  "alignment_stats": {
    "OK": 6,
    "E_MISS": 1,
    "E_DUP": 1,
    "E_TIMEOUT": 1,
    "E_REORDER": 2,
    "E_LOCAL": 0,
    "TOTAL": 11,
    "RETRYABLE": 5
  }
}
```

三条路径比对全部一致：

```text
REALTIME          ⇄ RESUME             equal=true
REALTIME          ⇄ PERSISTED_REPLAY   equal=true
RESUME            ⇄ PERSISTED_REPLAY   equal=true
```

对 `verification.json` 连续执行两次三路径验收，文件 SHA-256 均为：

```text
3fae312c57f59253f705cbcb9fe4df39556234bfd9bab7676ee47c82cd9f44e1
```

检查点：

- 11 个窗口，序号 `0..10` 连续；
- 恢复路径复用 11 个持久化包；
- 持久化重放读取 11 个包和 26 条决策；
- 最后哈希前缀 `b01cdd5349b35fce`。

样本与完整输出：

- `artifacts/acceptance/events.json`
- `artifacts/acceptance/capabilities.json`
- `artifacts/acceptance/fault_plan.json`
- `artifacts/acceptance/checkpoints.sqlite3`
- `artifacts/verification.json`
- `artifacts/verify-summary.txt`

## 界面

用真实 PySide6 窗口离屏运行三路径验证并截图：

```text
screenshot 1680x1020, three-path ok=true, loopback port assigned by OS
```

截图：`artifacts/workbench.png`

界面中可见：

- 事件流和 26 条逐事件决策；
- 11 个通道包及 `CONFIRMED`/`FAILED` 状态；
- `E_MISS`、`E_DUP`、`E_TIMEOUT`、`E_REORDER` 明细；
- 三路径比对全部“一致”；
- 回环端口和检查点工作目录。

另以当前图形会话直接启动窗口 4 秒，进程保持运行直至超时退出（`exit=124`），说明本地 Qt 窗口能够创建；日志仅有 WSL 软件渲染的 EGL 警告，不影响界面。

## 端口

本次验收均由 `--port 0` 启动，实际端口由 OS 分配，界面状态栏显示实际值。单元测试另验证了固定端口冲突时返回 `E_IO`、`field=port`。生产/本地固定端口示例：`--port 17890`。

## 说明

当前执行宿主是 WSL2 而非 Windows 原生进程，因此额外使用 `offscreen` 生成可重复截图；代码使用 Qt、TCP 回环和文件路径 API，未依赖 WSL 专有服务，Windows Python 3.11 + PySide6 可直接运行。模拟设备处理时刻采用确定性逻辑时钟，避免真实墙钟导致三路径回显比对不稳定；墙钟纳秒值仍随回显返回，仅供人工排障。

