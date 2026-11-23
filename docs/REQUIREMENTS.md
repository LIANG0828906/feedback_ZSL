# 运行环境与依赖

## 环境

- Python 3.11（开发验证版本：3.11.18）
- NumPy 2.4.6（仅用于栅格数据的整型校验与存储；生成器与全部数值
  计算只使用标准库，保证跨版本逐位可复现）
- 无其他第三方依赖；不访问任何外部服务或账号；全流程可离线运行。

## 复现步骤

```bash
python3.11 -m venv .venv
.venv/bin/pip install numpy==2.4.6

# 生成确定性样本（地图 + 解析真值）并运行全部实验
.venv/bin/python scripts/run_experiment.py all

# 运行测试套件（28 个用例）
.venv/bin/python -m unittest discover -s tests
```

## 目录

- `tacpath/` 组件包（定点搜索、平滑、推进、报告、检查点、流水线）
- `scripts/run_experiment.py` 可重复实验脚本（gen / run / all）
- `maps/` 生成的确定性样本与真值清单（manifest.json）
- `results/` 实验输出（逐对报告、summary.json、检查点快照）
- `tests/` 单元与端到端测试
- `docs/TECHNICAL.md` 技术设计说明
