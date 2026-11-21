# 实际验证记录

验证日期：2026-11-22（Asia/Shanghai）

## 环境

```text
Python 3.11.18
pip 26.3
PySide6 6.12.0
NumPy 2.4.6
pytest 9.2.0
```

虚拟环境位于 `.venv`，包以 editable 模式安装。验证过程不访问网络服务、不启动容器。

## 自动化测试

命令：

```bash
.venv/bin/pytest
```

结果：

```text
22 passed in 18.38s
```

覆盖内容：

- 场景缺失字段、未闭合路径、非有限坐标、分区重叠及错误定位。
- 三层笔触生成、排线避让轮廓、阴影密度调制涂抹透明度。
- 相同参数笔触字节一致性、统一网格批次路由。
- 轮廓/阴影/涂抹/几何因子的增量失效和保守闭包原因。
- 批次写失败、旧版本保留、临时文件清理。
- 子进程 `os._exit(91)` 强杀、staging 完成批次恢复、缺失批次续算。
- 分块篡改、`CURRENT` 版本不符的 `E_RECOVERY_INVALID` 定位。
- `E_BUSY`、批次边界取消、取消后旧预览可读。
- 版本原子快照与 `E_STALE_HANDLE`。
- 样本目录逐字节确定性。
- PySide6 离屏绘制并输出 PNG。

## 完整回归

命令：

```bash
.venv/bin/npr-pipeline regression \
  --out artifacts/regression \
  --seed 20261122
```

三类样本初始版本哈希：

| 样本 | 分区数 | 批次文件数 | 初始版本哈希 |
|---|---:|---:|---|
| sparse | 4 | 60 | `93be3c83ed55fa12aa506ba913777061a9763f4d5ce3754b6376a9dcd65d2135` |
| balanced | 6 | 66 | `50c7e797fba115d9ecb5d932e290c2d3144bb700a5b2d44e118ab330a50b59ac` |
| dense | 9 | 116 | `9a639461b840052ff2460f9fbcd6c51352ede716aaab44494da27f39a63bbb1d` |

`balanced` 增量与故障注入结果：

| 步骤 | 版本 | 结果 |
|---|---:|---|
| 阴影间距变化 | 2 | 轮廓批次全部复用，阴影与涂抹重算 |
| 涂抹宽度变化 | 3 | 轮廓、阴影全部复用，仅涂抹重算 |
| 注入首个分块写失败 | 3 | `E_COMMIT_FAILED`，旧版本仍为 3 |
| 子进程写入 3 个批次后强杀 | 3 | 退出码 91，旧版本仍为 3 |
| 磁盘恢复 | 3 | 识别 3 个已完成批次和未完成批次 |
| staging 续算 | 4 | 复用 3 个已完成批次，恢复版本哈希 `625a0d5e4e6c8ff08e1b630c823b0121644a643cb06d7d62912e4de99b19045f` |

最终预览快照、`CURRENT.json` 与轨迹日志的版本哈希一致：

```text
preview.content_hash = 625a0d5e4e6c8ff08e1b630c823b0121644a643cb06d7d62912e4de99b19045f
trace.version.content_hash = 625a0d5e4e6c8ff08e1b630c823b0121644a643cb06d7d62912e4de99b19045f
```

回归报告中的轨迹 SHA-256：

```text
d2aa129b10856ca4fea2efa6abb2d1594dbe1696601f42e1eb605da1daefd608
```

## 确定性复跑

相同种子和操作序列在独立目录再次执行完整回归，并比较关键产物：

```bash
diff -q artifacts/regression/traces/balanced.jsonl \
        /tmp/npr-regression-final-replay2/traces/balanced.jsonl
diff -q artifacts/regression/report.json \
        /tmp/npr-regression-final-replay2/report.json
diff -q artifacts/regression/samples/manifest.json \
        /tmp/npr-regression-final-replay2/samples/manifest.json
```

结果：全部无差异，输出 `DETERMINISTIC_REPLAY_OK`。正式保留的回归证据位于 `artifacts/regression/`。

## 强杀恢复观测

强杀后 `CURRENT.json` 仍指向旧版本；staging 中记录了原子完成的批次。恢复工具输出类似：

```json
{
  "version": 3,
  "resume_available": true,
  "completed_batches": [
    "outline:zone_00:r0c0",
    "outline:zone_00:r0c1",
    "outline:zone_00:r1c0"
  ],
  "missing_batches": ["..."]
}
```

再次运行相同场景与参数后提交版本 4，轨迹中 `resumed_batches` 为 3；未完成批次继续写盘，已提交版本在恢复期间始终可预览。

