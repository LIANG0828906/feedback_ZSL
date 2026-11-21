# 实际验证记录

验证日期：2026-11-22（Asia/Shanghai）

## 环境

```text
Python       3.12.16
PySide6      6.12.0
NumPy        2.5.5
Platform     Linux-6.18.33.1-microsoft-standard-WSL2-x86_64
Display      Wayland normal window verified; offscreen used for automated smoke
```

项目声明最低 Python 3.11，依赖见 `pyproject.toml`。

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest -q
```

结果：

```text
......................                                                   [100%]
24 passed
```

静态检查：

```bash
uv tool run ruff check tactical_review tests
```

```text
All checks passed!
```

关键断言：

- 同参数样本生成的所有 JSON 文件字节一致。
- 同一物理步内投递列表逆序、到达序号逆序后，哈希链和所有掩体开裂事件仍一致。
- 每一步 `remaining_total + applied_damage_total == initial_total`。
- 每个掩体 `remaining + damage == max_durability`，账本满足
  `display_remaining + pending == authority_remaining`。
- 样本包含 `E_OP_LATE`、`E_OP_DUPLICATE`、`E_REVOKE_CLOSED`、`E_DAMAGE_CONFLICT`。
- 第 17 步发布后恢复，推进到第 45 步，最终哈希链与完整重放完全相同。
- `missing_field`、`hash_break`、`durability_tamper`、`prefix_digest` 四种恢复异常均返回
  `E_RECOVERY_INVALID` 并定位到第 7 步。
- 同 runner 重复启动以及同分支第二个 runner 启动均返回 `E_BUSY`。
- 取消后快照集合不再增长，最后快照仍能通过完整链校验。
- 推进注入返回 `E_PROGRESS_INJECTED`，最后保留快照为第 5 步。
- 重算注入返回 `E_RECOMPUTE_INJECTED`。
- 两次独立 CLI 运行的最终哈希、哈希链和 `trajectory.jsonl` 字节一致。
- PySide6 offscreen 工作台读取 3 个已发布快照后显示 8 块掩体、步号 `03`。

## 完整样本轨迹

生成命令：

```bash
.venv/bin/python -m tactical_review generate \
  --out samples/demo --seed 20261122 --covers 8 --shots 48 --end-step 45
```

重放命令：

```bash
.venv/bin/python -m tactical_review run \
  --sample samples/demo \
  --run-dir artifacts/demo_run \
  --report artifacts/demo_run/review_report.json \
  --trajectory artifacts/demo_run/trajectory.jsonl
```

最后一步轨迹：

```json
{
  "step": 45,
  "time_ms": 2250,
  "state_hash": "673457357510b3087eceaf80afe42e3b4e456e74329039419c7bb2bf9c8ce0c4",
  "applied_damage_total": 2446896121,
  "remaining_total": 243103879,
  "initial_total": 2690000000,
  "buffer_watermark": {
    "total": 0,
    "per_player": {},
    "earliest_due_step": null
  }
}
```

业务结果：

```text
initial hash      d30a49c2e60767149dbb2ea87eeca9788251e9536c85497b0200ffeaa8d0f0c4
final hash        673457357510b3087eceaf80afe42e3b4e456e74329039419c7bb2bf9c8ce0c4
baseline hash     673457357510b3087eceaf80afe42e3b4e456e74329039419c7bb2bf9c8ce0c4
hash chain links  45
damage balance    true
baseline diffs    0
trajectory sha256 10039cff02deb512cd762ce689b4f5e7eb8991f5d639787c7898bf50d239532a
report sha256     1c99d0fd03f440e78b9f62dd5fb2c2c9f507cfc84ae0b9d6d80cc6fcf52fbf76

E_DAMAGE_CONFLICT  20
E_OP_LATE          2  (lag steps 5 and 7; window 3)
E_OP_DUPLICATE     2
E_REVOKE_CLOSED    1
accepted revoke    1  (restored 79.548879 durability units)

cracked covers     8 / 8
alive covers       6 / 8
crack events       27 (22 direct, 5 chain)
duplicate segment  0
```

## 暂停与恢复

先重放 17 步：

```bash
.venv/bin/python -m tactical_review run \
  --sample samples/demo --run-dir /tmp/demo-restore-verify --max-steps 17
```

再从第 17 步快照恢复：

```bash
.venv/bin/python -m tactical_review restore \
  --sample samples/demo --run-dir /tmp/demo-restore-verify
```

恢复运行输出：

```text
steps                 29 (published while resuming; chain still has all 45)
final_step            45
final_hash            673457357510b3087eceaf80afe42e3b4e456e74329039419c7bb2bf9c8ce0c4
fatal_codes           []
```

恢复没有再次调用第 1-17 步的 `advance()`，因此未重复应用已提交物理步。快照中已结算键、缓冲、撤回、挂账和链前缀均被重建。

## 强杀恢复

使用带 `--step-delay 0.08` 的独立进程，在第 7 个快照发布后发送 `SIGKILL`：

```text
snapshots before kill   7
last snapshot           step_000007.json
review_report.json      absent
restore exit            0
restored final step     45
restored final hash     673457357510b3087eceaf80afe42e3b4e456e74329039419c7bb2bf9c8ce0c4
```

强杀时没有生成退出报告；恢复后最终哈希与完整重放相同。该结果证明只依赖原子提交的快照和缓冲恢复即可继续，不依赖终止时的进程内状态。

## 异常注入

推进异常：

```bash
.venv/bin/python -m tactical_review run \
  --sample samples/demo --run-dir /tmp/demo-progress-fault \
  --progress-fault-step 9
```

结果：退出码 `3`，`E_PROGRESS_INJECTED`，`last_published_step=8`。

重算异常：

```bash
.venv/bin/python -m tactical_review run \
  --sample samples/demo --run-dir /tmp/demo-recompute-fault \
  --recompute-fault-step 5
```

结果：退出码 `2`，报告含 `E_RECOMPUTE_INJECTED`，步号为 5。

恢复异常：

```bash
.venv/bin/python -m tactical_review restore \
  --sample samples/demo --run-dir /tmp/demo-recovery-fault \
  --recovery-fault hash_break --recovery-fault-step 18
```

结果：退出码 `1`。

```json
{"code":"E_RECOVERY_INVALID","message":"hash-chain predecessor is broken","step":18}
```

## 图形界面

启动：

```bash
.venv/bin/python -m tactical_review gui \
  --sample samples/demo --run-dir artifacts/demo_run
```

验证内容：

- 发布步号显示 `45 / 45`。
- 8 块掩体耐久与开裂状态从最新快照读取。
- 时间轴显示 45 步耐久曲线、开裂点、死亡段和事件标记。
- 挂账曲线读取每步快照的 `pending`。
- 延迟柱状图只使用生成器显式 `latency_steps`。
- 逐条对账显示 `APPLIED`、`REFUNDED`、`CONFLICT`。
- 撤回表显示 1 条 accepted、1 条 `E_REVOKE_CLOSED`。
- 分叉诊断逐步骤比较当前链与基线，演示样本无分歧。

截图：

- `docs/workbench-main.png`
- `docs/workbench-reconciliation.png`
- `docs/workbench-latency.png`

