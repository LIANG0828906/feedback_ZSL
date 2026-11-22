# 实际验证记录

执行日期：2026-11-23

执行环境：

- WSL2 Linux
- CPython 3.14.4
- NumPy 2.5.5
- PySide6 6.12.0
- 本地回环 `127.0.0.1` 绑定成功

目标兼容环境为 Windows + Python 3.11+。代码不使用 Unix 专属信号，原子写目录 `fsync` 在 Windows 不可用时自动降级为已 `fsync` 临时文件加 `os.replace`。

## 自动化测试

命令：

```bash
python -m unittest discover -s tests -v
```

结果：

```text
Ran 30 tests in 2.182s
OK
```

覆盖范围：

- 三版本逐 tick 解码与权威摘要一致。
- 协议错误路径、源版本、冲突对象定位。
- v1/v2 `E_LOSSY` 警告与降级映射。
- 跨线程重复编码字节确定性。
- 固定种子重复推进摘要一致。
- 预测回滚逐字段一致，序列空洞/倒序不破坏确认状态。
- 检查点原子性、提交顺序、身份失配整体作废、强制终止和取消边界。
- 回环服务三类异常下三版本收敛和回显。
- 有界并发、队列/内存上限和跨运行字节稳定。
- 单命令演示报告生成。

## 演示运行

命令：

```bash
python demo.py --ticks 60
```

结果：

```text
demo: PASS (10 passed, 0 failed)
final_hash: d16897e62030771afaa912121ed5cd314c53f56cc29252d43ea0f753e4af3dfa
```

报告文件：

- `artifacts/demo/report.json`
- `artifacts/demo/report.md`

连续两次执行 `python demo.py` 后，`samples/frames_v1.jsonl`、
`frames_v2.jsonl`、`frames_v3.jsonl` 与 `artifacts/demo/report.json`
的 SHA-256 均保持不变。报告 SHA-256：

```text
0a28acbdd0644f308b0db6012caf1ade5063fab3d923572b7e0b83b100d6025b
```

## 样本

- 对局种子：`42869123`
- 对局 ID：`local-collab-demo`
- 玩家：`p1`, `p2`
- 单位：`u1`, `u2`, `u3`, `u4`
- tick 数：60（含 tick 0，共 61 帧）
- 输入流指纹：`2944bc3cc862be8879a37ac70c998bfe54e9489e684f142d50845f3272d3e85a`
- 三版本帧文件：`samples/frames_v1.jsonl`, `frames_v2.jsonl`, `frames_v3.jsonl`

## 互操作与确定性

| 版本 | 解码帧 | 再编码字节差异 | 重复编码差异 | 最终摘要 |
|---|---:|---:|---:|---|
| v1 | 61 | 0 | 0 | 与权威一致 |
| v2 | 61 | 0 | 0 | 与权威一致 |
| v3 | 61 | 0 | 0 | 与权威一致 |

v1、v2 每帧各产生 3 条 `E_LOSSY` 警告；v3 为 0。首个分歧字段为 `null`。

## 预测与回滚

- 预测使用仅含 `p1` 输入的本地意向流，权威包含 `p1` 和 `p2`，因此产生真实偏差。
- tick 0–12 每次权威帧到达后，确认状态与权威逐字段一致。
- 观测修正记录：71 条。
- 最大单字段绝对修正量：5605 个定点单位。

## 回环异常

每个版本注入：

- 丢帧：sequence 7
- 重复帧：sequence 13
- 乱序：sequence 21/22

服务统计：

```text
accepted_clients = 3
frames_sent = 345
reports_received = 24
echoes_sent = 24
resync_requests = 6
dropped = 3
duplicated = 3
reordered = 3
```

v1、v2、v3 客户端最终摘要全部等于：

```text
d16897e62030771afaa912121ed5cd314c53f56cc29252d43ea0f753e4af3dfa
```

首个分歧为 `null`。

## 错误码验证

| 场景 | 结果 |
|---|---|
| 未知版本 99 | `E_VER`, `$.protocol_version` |
| v1 缺少 `server_tick` | `E_VER`, `$.server_tick`, source_version=1 |
| 损坏 JSON | `E_DECODE`, `$` |
| v1 引用不存在单位 | `E_SEMANTIC`, object_id 可见 |
| v3 逻辑时钟倒退 | `E_SEMANTIC`, `$.logical_clock` |
| v3 差量与完整快照冲突 | `E_SEMANTIC`, 冲突单位 `u1` |
| 客户端序列空洞/倒序 | `E_SEQ`, 确认摘要不变 |

## 检查点与终止边界

- tick 0/10/20 串行写入成功，加载恢复 tick 20。
- 倒序提交返回 `E_SEQ`。
- 对局种子、输入指纹或协议版本任一失配时整份检查点无效并要求重算。
- 强制终止模拟停在 tick 25，最后持久窗口为 tick 20；原进程 `complete=false`。
- 重启从 tick 20 恢复，tick 21–60 明确重算，最终摘要与完整权威一致。
- 取消场景 `cancelled=true`、`complete=false`。

## 并发边界

```text
max_workers = 4
max_queue = 8
max_buffer_bytes = 4194304
max_observed_pending = 3
max_observed_buffer_bytes = 1572864
```

三个协议版本在 worker 内完成推进、编码与解码；跨两次并发运行，每 tick 的编码字节哈希完全一致。

## 界面

PySide6 offscreen 冒烟通过：

- 单位表 4 行。
- 可切换版本 1/2/3。
- 61 个预测修正曲线点。
- 无显示环境自动降级，不影响报告生成。

