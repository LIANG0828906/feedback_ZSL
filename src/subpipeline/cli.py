"""Command line entry points for generation, runs and verification."""

import argparse
import json
import os
import sys

from .checkpoint import CheckpointStore
from .diagnostics import render_report, try_gui_panel
from .errors import PipelineError
from .loader import load_ground_truth, load_project
from .pipeline import Pipeline
from .samples import generate_all
from .truth import build_ground_truth
from .validator import verify_run
from .version import ALGORITHM_VERSION


def cmd_generate(args):
    index = generate_all(args.samples_root, seed=args.seed)
    # Build ground truth per project using the deterministic algorithm.
    skipped = {}
    for entry in index["projects"]:
        project_dir = os.path.join(args.samples_root, entry["dir"])
        project, _m, _w, _h = load_project(project_dir)
        try:
            from .samples import write_truth
            write_truth(project_dir, build_ground_truth(project))
        except PipelineError as exc:
            # Error-path projects (E_GAP/E_PARA/...) ship without truth.
            skipped[project.project_id] = exc.to_dict()
    print(json.dumps(index, ensure_ascii=False, indent=2))
    if skipped:
        print(json.dumps({"skipped_ground_truth": skipped},
                         ensure_ascii=False, indent=2))
    return 0


def cmd_run(args):
    project, manifest, wave, _h = load_project(args.project_dir)
    pipe = Pipeline(project, manifest, wave, runs_root=args.runs_root,
                    max_workers=args.workers, max_buffers=args.buffers,
                    run_dir=args.run_dir)
    results, failed = pipe.run(parallel=not args.serial)
    if args.gui:
        try_gui_panel(pipe.board, failed, pipe.run_dir)
    else:
        print(render_report(pipe.board, failed, pipe.run_dir), file=sys.stderr)
    payload = {
        "run_dir": pipe.run_dir,
        "committed": [r.to_dict() for r in results],
        "failed": {str(sid): exc.to_dict() for sid, exc in failed.items()},
        "algorithm_version": ALGORITHM_VERSION,
    }
    print(json.dumps(payload, ensure_ascii=False, indent=2))
    return 0 if not failed else 3


def cmd_verify(args):
    truth = load_ground_truth(args.project_dir)
    try:
        violation = verify_run(args.project_dir, args.run_dir,
                               args.tolerance, truth)
    except PipelineError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))
        return 2
    if violation is None:
        print(json.dumps({"status": "ok"}, ensure_ascii=False))
        return 0
    print(json.dumps(violation.to_dict(), ensure_ascii=False, indent=2))
    return 1


def build_parser():
    parser = argparse.ArgumentParser(prog="subpipeline")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="generate deterministic samples")
    gen.add_argument("samples_root")
    gen.add_argument("--seed", type=int, default=20261123)
    gen.set_defaults(func=cmd_generate)

    run = sub.add_parser("run", help="run the layout pipeline")
    run.add_argument("project_dir")
    run.add_argument("--runs-root", default="runs")
    run.add_argument("--workers", type=int, default=4)
    run.add_argument("--buffers", type=int, default=8)
    run.add_argument("--serial", action="store_true")
    run.add_argument("--gui", action="store_true")
    run.add_argument("--run-dir", default=None,
                     help="resume an existing run directory")
    run.set_defaults(func=cmd_run)

    ver = sub.add_parser("verify", help="verify a persisted run")
    ver.add_argument("project_dir")
    ver.add_argument("run_dir")
    ver.add_argument("--tolerance", type=float, default=0.02)
    ver.set_defaults(func=cmd_verify)
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except PipelineError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2),
              file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
