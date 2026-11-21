"""Scriptable command-line entry point.

Subcommands:
  generate  deterministic sample suite
  run       full retargeting run (checkpoints + finalize)
  resume    recover from disk snapshots, recompute unfinished intervals
            (never writes the exit report)
  finalize  assemble poses / trajectory log / report from checkpoints

Business errors are printed as JSON and exit with code 3; success exits 0.
"""

import argparse
import json
import sys

from .errors import RetargetError
from .pipeline import Config
from .runner import finalize, resume, run
from .samplegen import gen_suite


def _add_run_args(p):
    p.add_argument("--mode", choices=["float", "fixed", "both"],
                   default="both")
    p.add_argument("--decimals", type=int, default=4)
    p.add_argument("--rounding",
                   choices=["half_even", "half_up", "down", "floor"],
                   default="half_even")
    p.add_argument("--tolerance-deg", type=float, default=0.5)
    p.add_argument("--scale-tolerance", type=float, default=1e-6)
    p.add_argument("--interval", type=int, default=16)
    p.add_argument("--interval-delay-ms", type=int, default=0,
                   help="test hook: sleep between interval commits")
    p.add_argument("--inject-scale-dup", action="store_true",
                   help="test hook: apply bone scale twice (fault "
                        "injection for the E_SCALE_APPLIED check)")


def _config_from(args):
    return Config(
        mode=args.mode, decimals=args.decimals, rounding=args.rounding,
        tolerance_deg=args.tolerance_deg,
        scale_tolerance=args.scale_tolerance,
        interval_frames=args.interval,
        interval_delay_ms=args.interval_delay_ms,
        inject_scale_dup=args.inject_scale_dup,
    )


def main(argv=None):
    parser = argparse.ArgumentParser(prog="retarget")
    sub = parser.add_subparsers(dest="command", required=True)

    p_gen = sub.add_parser("generate", help="generate deterministic samples")
    p_gen.add_argument("--out", required=True)
    p_gen.add_argument("--seed", type=int, default=20261122)

    p_run = sub.add_parser("run", help="full retargeting run")
    p_run.add_argument("--anim", required=True)
    p_run.add_argument("--target", required=True)
    p_run.add_argument("--out", required=True)
    _add_run_args(p_run)

    p_resume = sub.add_parser("resume", help="recover an interrupted run")
    p_resume.add_argument("--out", required=True)
    p_resume.add_argument("--interval-delay-ms", type=int, default=0)

    p_fin = sub.add_parser("finalize", help="write final artifacts")
    p_fin.add_argument("--out", required=True)

    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            baseline = gen_suite(args.seed, args.out)
            print(json.dumps({"status": "ok",
                              "samples": len(baseline["samples"]),
                              "suite_hash": baseline["suite_hash"]},
                             sort_keys=True))
        elif args.command == "run":
            run(args.anim, args.target, args.out, _config_from(args))
            print(json.dumps({"status": "ok", "out": args.out},
                             sort_keys=True))
        elif args.command == "resume":
            info = resume(args.out, args.interval_delay_ms)
            print(json.dumps({"status": "ok", **info}, sort_keys=True))
        elif args.command == "finalize":
            report = finalize(args.out)
            print(json.dumps({
                "status": "ok",
                "verdicts": {p: r["verdict"]
                             for p, r in report["paths"].items()},
            }, sort_keys=True))
    except RetargetError as exc:
        print(json.dumps(exc.to_dict(), sort_keys=True))
        return 3
    return 0


if __name__ == "__main__":
    sys.exit(main())
