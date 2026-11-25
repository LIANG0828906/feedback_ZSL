# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）

环境：

```text
$ go version
go version go1.22.12 linux/amd64

$ uname -s -r -m
Linux 6.18.33.1-microsoft-standard-WSL2 x86_64
```

## 自动化测试

命令：

```bash
go test ./...
```

结果：通过。覆盖：

- 三版 schema 装载后规范模型一致。
- Q16 已知向量、权重换算、溢出检测和窗口饱和。
- 手工推导的前六帧调度、双窗口零归因、RST 释放额。
- 固定种子端到端验收报告。
- `reparent` 与 `inherit` 两种孤儿流规则。

## 全量验收

命令：

```bash
./scripts/generate.sh data
./scripts/verify.sh data artifacts/demo
```

实测摘要：

```text
OK=true cross-version=true truth=true numerical=true incremental=true parallel=true checkpoint=true
events=42 sends=29 exceptions=12
```

异常码全部匹配：

```text
negative_window                  E_WINDOW_NEGATIVE
window_update_over_cap           E_WINDOW_OVER_CAP
weight_out_of_range              E_WEIGHT_RANGE
zero_weight_report               E_WEIGHT_ZERO
dependency_cycle                 E_DEPENDENCY_CYCLE
frame_too_large                  E_FRAME_TOO_LARGE
frame_too_large_attribution      E_FRAME_TOO_LARGE
budget_out_of_range              E_BUDGET_RANGE
buffer_full_backpressure         E_BUFFER_FULL
thread_budget_exhausted          E_THREAD_BUDGET_EXHAUSTED
checkpoint_truncated             E_CHECKPOINT_CORRUPT
checkpoint_version_mismatch      E_CHECKPOINT_VERSION
```

增量缓存实测：

```text
weight-stream-5          reused=0  recomputed=42 PASS
parent-stream-9-to-5     reused=0  recomputed=42 PASS
window-stream-7          reused=0  recomputed=42 PASS
update-wu-stream-1       reused=31 recomputed=11 PASS
delete-data-event-20     reused=20 recomputed=21 PASS
append-wu-stream-1       reused=42 recomputed=1  PASS
```

并行实测：4 条连接、2 worker，每条 42 个事件均为 `E_OK`；线程探针返回 `E_THREAD_BUDGET_EXHAUSTED`，并行缓冲探针返回 `E_BUFFER_FULL`。

定点报告：`artifacts/demo/fixed-point-report.json` 中 5 个流的累积分配量与真值精确相等，Q16 份额差为 0，剩余窗口逐项匹配；8 个数值用例全部通过。

## SQLite 强杀恢复

命令：

```bash
./scripts/checkpoint-crash-demo.sh
```

实测：

```text
crash_exit_code=137 (137 means SIGKILL)
visible_global_seq_after_kill= 3
{
  "code": "E_OK",
  "final_committed": 41,
  "matches_full_run": true,
  "recovered_traces": 38,
  "suffix_start_index": 4
}
```

含义：在事件 4 的“流状态/调度进度已写入事务、全局序号尚未更新”点 SIGKILL 后，未提交事务回滚，外部只能看到全局序号 3；重启从事件 4 续算，恢复后缀与全量重算逐项一致。`after_commit` 注入则可观察到全局序号已推进到注入事件，再从下一事件恢复。

完整命令输出另存于 `artifacts/demo/final-validation.log`。
