#!/usr/bin/env python3
"""完整本地示例(无界面): 三路径处理 + 一致性比对 + 误差报告导出。"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from a11y_media import compare
from a11y_media.api import MediaAccessibilityService
from scripts.compare_paths import run_three_paths


def main() -> int:
    ap = argparse.ArgumentParser(description="无障碍媒体呈现组件 CLI 示例")
    ap.add_argument("project", help="工程目录(含 manifest.json)")
    ap.add_argument("--checkpoints", default="data/checkpoints_demo")
    ap.add_argument("--export", default="", help="误差报告导出路径(JSON)")
    args = ap.parse_args()

    results = run_three_paths(args.project, args.checkpoints)
    compare.assert_consistent(results)
    print("[一致] 实时 / 断点恢复 / 全量重算 三路径结果一致")

    svc = MediaAccessibilityService(args.project)
    svc.set_result(results["full"])
    n = len(svc.project.subtitles)
    print(f"[工程] {svc.project.project_id} 句子={n} "
          f"采样率={svc.project.sample_rate} 帧长={svc.project.frame_len} "
          f"帧移={svc.project.hop}")
    for i in range(n):
        w0, w1 = svc.get_window(i)
        s0, s1 = svc.get_char_interval(i, 0)
        print(f"  句{i:02d} 窗口=[{w0},{w1}) 首字符高亮=[{s0},{s1}) "
              f"文本={svc.project.subtitles[i].text!r}")
    report = svc.get_error_report()
    tr = report["total_read"]
    print(f"[误差] 上界={report['bounds']} 总时长偏差={tr['deviation']}"
          f"(上界 {tr['bound']})")
    if args.export:
        os.makedirs(os.path.dirname(args.export) or ".", exist_ok=True)
        with open(args.export, "w", encoding="utf-8") as f:
            json.dump(report, f, ensure_ascii=False, indent=1)
        print(f"[导出] {args.export}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
