# 无窗口运行入口：样本生成、管线执行、工程摘要、结果验证。
from __future__ import annotations

import argparse
import json
import sys

from caption_pipeline.errors import PipelineError


def _cmd_gen(args):
    import samplegen
    samplegen.generate(args.out, args.seed, args.projects)
    return 0


def _cmd_summary(args):
    from caption_pipeline.loader import load_project
    project = load_project(args.project)
    print(json.dumps(project.summary(), ensure_ascii=False, indent=2))
    return 0


def _cmd_run(args):
    from caption_pipeline.engine import run_project

    def on_commit(line_id, committed, total):
        print(f"[commit] {line_id}  进度 {committed}/{total}")

    hooks = {"on_commit": on_commit}
    if args.commit_delay:
        hooks["commit_delay_s"] = args.commit_delay
    summary = run_project(args.project, args.persist,
                          workers=1 if args.serial else args.workers,
                          hooks=hooks, max_commits=args.max_commits)
    print(f"[done] 已提交 {summary['committed']}/{summary['total']} 句台词")
    return 0


def _cmd_verify(args):
    import verify
    return verify.verify_project(args.project, args.persist)


def main(argv=None):
    ap = argparse.ArgumentParser(description="语音字幕时序对齐管线（无窗口入口）")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("gen", help="生成确定性样本")
    p.add_argument("--out", default="samples")
    p.add_argument("--seed", type=int, default=20261123)
    p.add_argument("--projects", type=int, default=3)
    p.set_defaults(fn=_cmd_gen)

    p = sub.add_parser("summary", help="输出工程摘要与台词总数")
    p.add_argument("--project", required=True)
    p.set_defaults(fn=_cmd_summary)

    p = sub.add_parser("run", help="运行管线（支持断点续跑）")
    p.add_argument("--project", required=True)
    p.add_argument("--persist", required=True)
    p.add_argument("--workers", type=int, default=4)
    p.add_argument("--serial", action="store_true", help="单线程串行运行")
    p.add_argument("--max-commits", type=int, default=None,
                   help="最多提交句数（演示暂停/退出）")
    p.add_argument("--commit-delay", type=float, default=0.0,
                   help="每句提交后延时秒数（演示强制终止）")
    p.set_defaults(fn=_cmd_run)

    p = sub.add_parser("verify", help="独立验证持久化结果")
    p.add_argument("--project", required=True)
    p.add_argument("--persist", required=True)
    p.set_defaults(fn=_cmd_verify)

    args = ap.parse_args(argv)
    try:
        return args.fn(args)
    except PipelineError as exc:
        print(str(exc), file=sys.stderr)
        if exc.details:
            print(json.dumps(exc.details, ensure_ascii=False), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
