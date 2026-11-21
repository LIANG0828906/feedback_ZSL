# 实际验证记录

验证日期：2026-11-22（本机时区 Asia/Shanghai）

## 环境

```text
Python 3.14.4
numpy 2.5.5
PyQt5 5.15.11
matplotlib 3.11.2
```

依赖安装在项目本地 `.venv`，未使用容器、外部网络服务或真实音频设备。

## 测试

命令：

```bash
.venv/bin/python -m unittest discover -s tests -v
```

结果：`Ran 11 tests in 1.812s - OK`。

覆盖：

1. 参考声道变化触发对齐重算并输出到达样本差异；
2. 材质变化复用加载、对齐和早期反射节点；
3. 频带变化标注受影响反射事件；
4. 陈旧版本拒绝渲染且不写文件，重建后成功输出 6 声道 WAV；
5. 四类注入返回独立业务码、`input:` 定位，发布字段字节不变；
6. 监听点租约返回 `E_BUSY`，其他监听点不受影响；
7. 协作式取消返回 `E_CANCELLED` 且保留最后提交；
8. 单通道卷积失败返回 `E_SYNTHESIS_FAILED`，无半写 WAV；
9. 两个独立工作区轨迹、字段和渲染逐字节一致；
10. 声源位置变化从早期反射节点开始重算，并复用加载/对齐节点；
11. 对齐真值绑定原始 WAV 内容哈希和样本索引。

## 诊断

命令：

```bash
.venv/bin/python -m acoustic_workbench.cli diagnose \
  --workspace artifacts/verification_workspace
```

`ok=true`，12/12 项检查通过。实际耗时由机器负载决定，不作为真值。

| 检查 | 结果 |
|---|---|
| baseline_reconstruction | PASS |
| five_one_field | PASS |
| noop_all_cache_hit | PASS |
| material_incremental_reuse | PASS |
| window_incremental_with_event_impact | PASS |
| stale_field_rejected | PASS |
| source_incremental_reuse | PASS |
| render_after_rebuild | PASS |
| anomaly_injection_business_codes | PASS |
| listener_busy_guard | PASS |
| cooperative_cancel_preserves_commit | PASS |
| byte_deterministic_replay | PASS |

## 产物哈希

```text
8e817530cfeb4045acae3478370159b7c542ecbe4cbff43279ef9ae75b6c2c52  deterministic_field.json
11d6652779b276c6aaed75b4658d6a43cafda697dc79ab1513eb9e8493a7afa7  deterministic_trajectory.jsonl
b7b926e7a63f6a9ad55576b9e78efe5e851d54ef397e1a1afe80a3eb33d973ad  deterministic_render.wav
```

这些哈希同时记录在 `artifacts/verification/diagnostic_report.json` 的 `determinism` 字段中。`small_trajectory.jsonl` 来自完整异常/取消诊断工作区，包含成功、`E_FIELD_STALE`、四类注入失败和 `E_CANCELLED` 轨迹。

`deterministic_field.json` 引用的滤波数组保存在
`filter_849aeb4bf0c94f6ddd3f059a30623781f51024bef748d48d6a39cdaeaa4f9821.npy`，
该文件 SHA-256 与场系数中的 `filter_blob.hash` 一致。

## 恢复说明

验证覆盖正常异常和协作式取消。强制 `SIGKILL` 未作为可生成退出报告的路径测试；系统只承诺重启后从磁盘内容寻址缓存、节点元数据、字段版本和原子发布指针恢复。未完成的临时文件不参与读取，孤儿 blob 不影响已提交版本。

