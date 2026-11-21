# 实际验证结果

环境：Python 3.11.18（venv）+ NumPy 2.4.6，本地运行，无网络/容器。
命令：`.venv/bin/python run_experiment.py workdir/experiment 20261122`（种子 20261122，12 样本 + 4 查询，含 8k–48kHz、单/双声道与 3 个非法布局样本）。

## 实验检查（26/26 通过，ALL_OK=True）

- `invalid_audio_fields`：96000Hz→`sample_rate`、8-bit→`bits_per_sample`、3 声道→`channels`，均返回 `E_AUDIO_UNSUPPORTED` 并定位字段。
- `pre_build_query_empty` / `post_build_query_hits`：建库前查询零命中；建库后 4 条查询全部命中正确样本（命中数 [2, 80, 4, 19]，top 候选偏移与真值一致，如 query-01→sample-003 @0.9s）。
- `build_hashes_equal_baseline`：入库指纹哈希与生成期基线完全一致。
- `busy_error`：批次运行中重复启动返回 `E_BUSY`。
- `byte_reproducible`：相同种子/配置/操作序列的两次完整运行，样本、指纹库、轨迹日志、检索结果逐字节一致（diff_files=[]）。
- `parallelism_invariant_lib` / `parallelism_invariant_query`：并行度 4 与 1 的库逐字节相同、检索结果逐候选一致。
- `degrade_stages_logged` / `degrade_hashes_stable` / `degrade_query_consistent`：单任务预算收紧到 600KiB 时 12 个样本全部按声明序列降级（stage 2/3，轨迹记录降级阶段与用量），指纹哈希与基线零漂移，检索结果与默认配置逐候选一致。
- `budget_rejected_some` / `budget_rejected_absent` / `tight_committed_hashes_stable`：预算收紧到 250KiB 时 10 个样本以 `E_BUDGET_EXCEEDED` 拒绝且库中无记录，2 个样本正常提交且哈希与基线一致，库清单校验通过（失败隔离）。
- `pool_wait_logged` / `pool_wait_lib_equal`：池预算 5MiB 时产生 30 条 `TASK_WAIT` 等待轨迹，最终库与默认运行逐字节一致。
- `kill_simulated` / `kill_partial_committed` / `recovery_lib_equal` / `recovery_query_consistent` / `recovery_trace_reproducible`：子进程在第 5 条提交后 `os._exit(137)` 模拟强杀；恢复时校验哈希链、跳过 5 条已提交样本、只补算其余 7 条；恢复后库与不间断运行逐字节一致，检索结果一致；重复整个“强杀+恢复”操作序列，两份轨迹日志逐字节一致。
- `lib_mismatch_error`：篡改记录文件后 `verify()` 返回 `E_LIB_MISMATCH` 并定位样本 sample-000。
- `recovery_invalid_error`：篡改清单第 3 条断链后 `recover()` 返回 `E_RECOVERY_INVALID` 并定位首个断链样本 sample-002。
- `align_mismatch_error`：查询量化基准 8 与库基准 4 不一致，返回 `E_ALIGN_MISMATCH`。
- `recompute_diff_summary`：帧移 512→256 重算后输出新旧指纹差异摘要（records 420→849，common 75，新旧哈希不同）。

## 单元测试

`.venv/bin/python -m unittest discover -s tests`：13 个测试全部通过（WAV 解析与字段定位、指纹跨降级阶段确定性、预算解析与耗尽、E_BUSY、预算拒绝隔离、并行度不变性、E_LIB_MISMATCH、E_RECOVERY_INVALID 定位、强杀恢复不重复写入、检索命中与版本回带、E_ALIGN_MISMATCH、全管线逐字节复现）。

## 产物位置

- `workdir/experiment/{a,b,p1,degrade,tight,poolwait,kill,kill2}/`：各场景完整工作目录（样本、库、轨迹、报告）。
- `workdir/experiment/reports/summary.json`：全部检查项与判定。
- `workdir/experiment/reports/consistency_report.txt`：逐查询/逐候选一致性明细与容差判定。
