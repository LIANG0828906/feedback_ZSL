# 业务要求追踪

| 要求 | 实现 | 验证 |
|---|---|---|
| 1. 三版本元数据与统一模型、来源版本、`E_META` | `metadata.py`、`model.py` | `tests/test_metadata.py`；`selfcheck` 三版本导入 |
| 2. 裸流导入、计数/时基/时间戳校验、`E_TRACK` | `raw.py`、`pipeline.py` | `tests/test_raw.py`；样本真值导入 |
| 3. 确定性封装、章节约束、`E_CHAP`/`E_VER` | `package.py`、`model.py` | `test_package.py` 重复哈希、章节边界、未知版本 |
| 4. 跨版本导出、`E_LOSSY`、映射痕迹、回读等价 | `metadata.py` 的 `export_metadata/project_model` | `test_metadata.py`、`test_package.py`、CLI `roundtrip/readback` |
| 5. 整数有理换算、`E_PREC`、往返采样数/时基不变 | `timebase.py` | `test_non_integral_unit_conversion_returns_e_prec`、往返测试 |
| 6. 独立校验器、首个差异、实测/声明值 | `validator.py` | `test_validator.py`；CLI `validate` |
| 7. 分段检查点、原子串行写、版本/哈希全量失效、重启恢复 | `checkpoint.py`、`segmented.py` | `test_checkpoint.py` 复用、外部损坏、版本篡改、缓冲失败 |
| 8. 四类异常、单段失败重试、并发上限与串并行一致 | `injection.py`、`segmented.py`、`tests/test_raw.py` | 四个异常参数文件、`test_checkpoint.py`、端到端 demo |

## 验收命令

```bash
python3 -m unittest discover -v
python3 -m mediaforge selfcheck --samples samples --checkpoint artifacts/checkpoints
python3 scripts/run_demo.py
```

本地离线运行直接使用工作区可用的 `python3` 解释器；独立环境的依赖通过仓库内 `wheelhouse/` 以 `--no-index --find-links wheelhouse` 离线安装，运行期不访问网络。

可复现结果记录见 `docs/VALIDATION_RESULTS.md` 与 `artifacts/demo/validation_report.json`。
