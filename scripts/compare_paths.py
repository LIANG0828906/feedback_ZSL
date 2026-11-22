#!/usr/bin/env python3
"""三路径一致性比对入口: 实时 / 断点恢复 / 全量重算。

对同一工程、同一语速与同一字符权重:
1. realtime : 一次性实时推进(写检查点);
2. resume   : 模拟处理若干段后强制终止, 重启后从检查点继续;
3. full     : 一次性全量重算(不读不写检查点)。
任一不一致输出首个不一致位置(句号/字符序号/字段名/两路径取值)并返回 E_DIVERGE。
"""

import argparse
import os
import shutil
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from a11y_media import compare, engine as engine_mod
from a11y_media.api import MediaAccessibilityService
from a11y_media.checkpoint import CheckpointStore
from a11y_media.engine import Engine, _Crash
from a11y_media.errors import ComponentError


def run_three_paths(project_dir: str, checkpoint_root: str) -> dict:
    svc = MediaAccessibilityService(project_dir, checkpoint_root=checkpoint_root)
    project = svc.project

    # 路径 1: 实时处理
    rt_dir = os.path.join(checkpoint_root, "rt")
    shutil.rmtree(rt_dir, ignore_errors=True)
    realtime = Engine(project, store=CheckpointStore(rt_dir)).run()

    # 路径 2: 断点恢复 —— 模拟强制终止后重启续跑
    rs_dir = os.path.join(checkpoint_root, "rs")
    shutil.rmtree(rs_dir, ignore_errors=True)
    store = CheckpointStore(rs_dir)
    crash_at = max(1, project.num_chunks // 2)
    if crash_at < project.num_chunks:
        try:
            Engine(project, store=store).run(crash_after=crash_at)
            raise AssertionError("应当模拟崩溃")
        except _Crash:
            pass
    resumed = Engine(project, store=store).run()  # 重启: 仅凭已持久化分段恢复

    # 路径 3: 一次性全量重算
    full = engine_mod.run_full(project)

    return {"realtime": realtime, "resume": resumed, "full": full}


def main() -> int:
    ap = argparse.ArgumentParser(description="三路径一致性比对")
    ap.add_argument("project", help="工程目录(含 manifest.json)")
    ap.add_argument("--checkpoints", default="data/checkpoints_compare")
    args = ap.parse_args()

    try:
        results = run_three_paths(args.project, args.checkpoints)
        compare.assert_consistent(results)
    except ComponentError as exc:
        print(f"DIVERGED: {exc}")
        return 1

    done, total = results["full"].progress
    report = results["full"].report
    print(f"OK: 三路径一致 | 分段 {done}/{total} | 句子 {len(report['sentences'])}")
    print(f"总朗读时长: actual={report['total_read']['actual']} "
          f"truth={report['total_read']['truth']} "
          f"deviation={report['total_read']['deviation']} "
          f"bound={report['total_read']['bound']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
