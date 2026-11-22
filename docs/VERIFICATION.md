# 实际验收记录

完整机器可读记录：[`artifacts/verification.json`](../artifacts/verification.json)

## 环境

- Python 3.11.18
- PySide6 6.12.0
- NumPy 2.4.6
- WSL2 / Ubuntu 26.04，无容器、GPU、外部服务
- Windows 目标运行脚本：`scripts/setup_windows.ps1`

## 结果

| 检查项 | 结果 |
|---|---|
| 自动化测试 | `30 passed` |
| 固定种子生成 | 两次生成逐字节一致 |
| 工程错误注入 | 7/7 命中 `E_SCHEMA/E_CYCLE/E_REF/E_PARA` |
| 运行时异常 | `E_STATE`、`E_PARA`、`E_DIFF` 测试通过 |
| 串行 vs 并行 | 结果、提交顺序一致；并行峰值在实际记录中为 2–3，均不超过工程上限 3 |
| 实时 vs 全量 | 逐场一致 |
| 断点恢复 vs 全量 | 逐场一致 |
| 恢复复用 | 15 个组合复用，27 个组合补算 |
| 检查点整体失效 | 版本不匹配时 49 行整体删除为 0 |
| 置信区间 | 同胜率 24→240 样本，Wilson 宽度 3716bp→1256bp |
| 工程运行 | 3 套 × 42 组合全部提交 |

## 三套工程

| 工程 | 组合 | 采样 | 峰值槽位 | 排队峰值 | 报告 |
|---|---:|---:|---:|---:|---|
| Alpha | 42 | 1200 | 3 | 42 | [Markdown](../artifacts/reports/alpha_report.md) |
| Beta | 42 | 900 | 2 | 42 | [Markdown](../artifacts/reports/beta_report.md) |
| Gamma | 42 | 1800 | 4 | 42 | [Markdown](../artifacts/reports/gamma_report.md) |

预算均不足以覆盖“每组合目标采样”。工作台按每个组合的内存/时间硬上限与全局预算扩样，记录 `MEMORY_BUFFER+TOTAL_SAMPLE_BUDGET` 或 `TOTAL_SAMPLE_BUDGET` 原因，但 42 个组合全部提交。

## 阈值诊断

每套工程 84 个阈值检查，其中 78 个通过。失败的 6 个来自故意注入的“玻璃大炮”方案：防御缩放为基准的 88%，低于 90% 防御阈值，界面和报告均标红。这说明阈值链路不是静态展示，而是按候选的有效属性计算。

## 复现

```bash
python scripts/generate_samples.py
python -m pytest -q
python scripts/run_demo.py
python run_app.py
```

`scripts/run_demo.py` 会重新生成样本、运行三路径/串并行/预算/检查点/错误注入检查、执行 pytest，并重写验收 JSON/Markdown 与三套报告。

## 强制终止说明

强制终止进程后不存在“内存中未提交结果”的承诺。重启时只加载版本匹配且完整提交的 SQLite 行；版本或内容哈希不匹配会整体作废。应用会提示用户安全暂停退出，报告状态也区分 `COMPLETE` 与 `PARTIAL`。

