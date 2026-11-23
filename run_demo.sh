#!/usr/bin/env bash
# 一键可重复演示：生成确定性样本 -> 全链路 -> 导出帧与报告
set -euo pipefail
cd "$(dirname "$0")"
export QT_QPA_PLATFORM="${QT_QPA_PLATFORM:-offscreen}"
PY=.venv/bin/python
[ -x "$PY" ] || PY=python3
PYTHONPATH=src "$PY" -m cadrender.demo \
  --workdir run_workdir --checkpoint run_ckpt --export demo_output \
  --size 256 --workers 3
