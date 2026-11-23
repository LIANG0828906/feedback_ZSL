#!/usr/bin/env bash
# 本地确定性验收：生成样本 -> 三路径一致性 -> 全量测试
set -euo pipefail

cd "$(dirname "$0")"
export PYTHONPATH=src
export QT_QPA_PLATFORM=offscreen

echo "[1/3] 生成确定性样本"
.venv/bin/python samples/generate_samples.py --out samples/data

echo "[2/3] 三路径（并行/串行/恢复）逐样本一致性验收"
.venv/bin/python -m loudbench.cli.main verify

echo "[3/3] 全量单元/集成测试"
.venv/bin/python -m unittest discover -s tests -v
