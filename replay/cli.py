"""命令行入口：样本生成、确定性回放、恢复与注入验证。"""
from __future__ import annotations

import argparse
import json
import sys

from .errors import ReplayError
from .generator import GenerationPlan, write_generation
from .pipeline import ReplayPipeline


def _print(payload: dict) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))


def cmd_generate(args: argparse.Namespace) -> int:
    plan = GenerationPlan(
        room_id=args.room,
        seed=args.seed,
        players=args.players,
        total_ticks=args.ticks,
        disorder=args.disorder,
        late_window=args.late_window,
    )
    paths = write_generation(args.out, plan)
    _print({name: str(path) for name, path in paths.items()})
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    pipeline = ReplayPipeline(
        snapshot_dir=args.snapshot_dir,
        baseline_path=args.baseline,
        snapshot_every=args.snapshot_every,
        fault=args.fault,
        fault_record=args.fault_record,
    )
    try:
        result = pipeline.run(args.replay)
    except ReplayError as exc:
        _print({"ok": False, **exc.to_dict()})
        return 2
    summary = result.to_summary()
    summary["ok"] = True
    summary["chain_length"] = len(result.hash_chain)
    _print(summary)
    return 0 if result.fork is None else 3


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="replay", description="本地赛事复盘回放管线")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="生成确定性三版本录像样本")
    gen.add_argument("--out", required=True)
    gen.add_argument("--room", default="room-demo")
    gen.add_argument("--seed", type=int, default=20261121)
    gen.add_argument("--players", type=int, default=3)
    gen.add_argument("--ticks", type=int, default=20)
    gen.add_argument("--disorder", type=int, default=2)
    gen.add_argument("--late-window", type=int, default=2)
    gen.set_defaults(func=cmd_generate)

    run = sub.add_parser("run", help="运行确定性回放与基线诊断")
    run.add_argument("replay")
    run.add_argument("--snapshot-dir", required=True)
    run.add_argument("--baseline", default=None)
    run.add_argument("--snapshot-every", type=int, default=1)
    run.add_argument(
        "--fault",
        choices=[
            "duplicate_op",
            "out_of_bounds",
            "late_op",
            "truncated_diff",
            "corrupt_snapshot",
            "format_order",
        ],
    )
    run.add_argument("--fault-record", type=int, default=0)
    run.set_defaults(func=cmd_run)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
