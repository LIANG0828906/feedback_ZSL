# 实际验证记录

## 环境

- 执行环境：Linux 工作台，Python 3.14.4（代码要求 Python >= 3.11）
- NumPy：2.5.5
- 未使用 Docker/容器、GPU、真实设备或网络目标
- 当前环境未安装 Tkinter，因此 GUI 完成导入与语法检查，未做窗口交互截图验证

环境中没有 `python3.11` 和系统 pip；验证时使用已有的 NumPy 站点包。
交付的 `requirements.txt` 在目标 Windows Python 3.11 环境执行
`python -m pip install -r requirements.txt` 即可得到 pip 解析的 NumPy。

## 单元测试

命令：

```bash
python3 -m unittest discover -v
```

结果：**22/22 通过**。

覆盖：

- BVH 解析、帧/关节错误定位、父引用越界、确定性 FK；
- 求解实例复算一致、固定约束编号归约、冲突报告定位；
- 权重改动只重解受影响区间、其他帧复用哈希；
- 串行/并行逐帧内容哈希完全一致；
- 预算降级阶段、预算拒绝隔离、降级前后哈希一致；
- 重复启动 `E_BUSY`；
- 全局内存配额等待队列不提高并行度；
- 临时文件失败 `E_COMMIT_FAILED` 后旧区间保持；
- 哈希链恢复、断点定位；
- 第二区间快照版本错误时定位到首个断点；
- 快照依赖参数版本不匹配时 `E_RECOVERY_INVALID`；
- 取消检查点保留最后一次提交，恢复后与未中断结果一致；
- 冲突截断后续帧且其他段完整提交；
- CLI 生成/求解/校验闭环。

## 样本生成

命令：

```bash
python3 -m motionreview generate --out /tmp/motionreview-samples-va6jmu
```

实际输出摘要：

```json
{
  "aggregate_hash": "09259ba2a69626a36cf276e993dc39c95ee4384ea51dd2a8247928e5849a1e16",
  "short_baseline": "e1712915a01d012e184256a5ff62b5ac420467350a43b6fd522e0e629989a9e4",
  "medium_baseline": "e48f39a9223b4c3b442d4b936640231e45b3046919b1095f550496e3d99f1745",
  "conflict_baseline": "2286e558febd5091f88d563186ff67fa818a5165f17a106399b84f7fbbe26b1b",
  "long_baseline": "8bb56cd1d3742c2ec80678f218e24dcf9e9a868c19fad22112e4d6d14f531302",
  "conflict_frames": [21]
}
```

默认四段：96/7、240/12、64/8（冲突）、1024/16。

## 端到端复算

命令：

```bash
python3 -m motionreview verify \
  --sample /tmp/motionreview-samples-va6jmu \
  --config /tmp/motionreview-samples-va6jmu/run_config.json
```

退出码 `0`，核心结果：

| 段 | 验证帧数 | 并行结果 | 串行结果 | 冲突帧 |
|---|---:|---|---|---:|
| short | 96 | COMPLETED | COMPLETED | — |
| medium | 240 | COMPLETED | COMPLETED | — |
| conflict | 21 | CONFLICT_STOPPED | CONFLICT_STOPPED | 21 |
| long | 1024 | COMPLETED | COMPLETED | — |

`ok = true`，所有逐帧姿态哈希、内容哈希、逐约束残差和链哈希一致。
完整输出见 `artifacts/verification_parallel_serial.json`。

这验证了：

- 跨段并行与并行度无关的逐帧可复算；
- 冲突段在第 21 帧前提交、后续不提交，其他段不受影响；
- 恢复/续解结果与未中断结果一致（单元测试覆盖取消与续解）；
- 预算降级阶段、预算拒绝、原子写失败与哈希链断点的隔离行为。
