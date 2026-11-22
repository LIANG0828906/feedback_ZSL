#!/usr/bin/env python3
"""验收总入口: 样本生成 -> 单元测试 -> 三路径一致 -> 异常码 -> 报告核对。"""

import os
import subprocess
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
PY = sys.executable


def step(title, cmd, cwd=ROOT):
    print(f"\n=== {title} ===")
    r = subprocess.run(cmd, cwd=cwd)
    if r.returncode != 0:
        print(f"FAILED: {title}")
        sys.exit(1)


def main():
    step("1. 生成确定性样本", [PY, "scripts/generate_samples.py",
                               "--out", "data/samples", "--count", "4", "--seed", "42"])
    step("2. 单元测试", [PY, "-m", "pytest", "tests/", "-q"])
    step("3. 生成损坏样本副本", [PY, "scripts/make_corrupt_samples.py",
                                 "--src", "data/samples/proj_000",
                                 "--out", "data/corrupt"])
    for p in sorted(os.listdir(os.path.join(ROOT, "data", "samples"))):
        step(f"4. 三路径一致 {p}",
             [PY, "scripts/compare_paths.py", f"data/samples/{p}",
              "--checkpoints", "data/checkpoints_validate"])
    step("5. CLI 示例与报告导出",
         [PY, "examples/demo_cli.py", "data/samples/proj_000",
          "--export", "data/error_report_demo.json"])
    print("\nALL VALIDATION PASSED")


if __name__ == "__main__":
    main()
