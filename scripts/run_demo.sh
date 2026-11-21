#!/usr/bin/env bash
# 点云工作台脚本化演示入口：生成样本、运行演示、执行测试。
set -euo pipefail
cd "$(dirname "$0")/.."

SAMPLES_DIR="${SAMPLES_DIR:-samples}"
OUT_DIR="${OUT_DIR:-out}"

echo "== 1/3 OpenGL 离屏上下文检查 =="
QT_QPA_PLATFORM=offscreen python3 -m pcwb.glcheck

echo "== 2/3 运行演示（样本生成 + 巡览 + 编辑 + 原子提交）=="
QT_QPA_PLATFORM=offscreen python3 -m pcwb.demo \
    --samples-dir "$SAMPLES_DIR" --out-dir "$OUT_DIR" "$@"

echo "== 3/3 单元测试 =="
python3 -m unittest discover -s tests -v

echo "完成。轨迹日志: $OUT_DIR/trace.log"
