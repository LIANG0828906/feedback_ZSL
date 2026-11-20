"""Command line interface for the deterministic replay service."""

from __future__ import annotations

import argparse
import json
import os
import sys

from . import inject as inject_mod
from .baseline import compare
from .errors import BizError, HardCrash
from .inject import OpInjector
from .runner import CANCEL_NAME, AdvanceTask
from .samplegen import generate_sample, write_baseline


def _emit(payload, exit_code=0):
    print(json.dumps(payload, sort_keys=True))
    return exit_code


def _read_chain_from_trace(trace_path):
    chain = []
    with open(trace_path, "r", encoding="utf-8") as fh:
        for raw in fh:
            if raw.strip():
                entry = json.loads(raw)
                chain.append({"step": entry["step"],
                              "state_hash": entry["state_hash"],
                              "chain_hash": entry["chain_hash"]})
    return chain


def cmd_gen(args):
    sample_path = generate_sample(args.seed, args.players, args.steps,
                                  args.disorder, args.out, room=args.room)
    ref_room = os.path.join(args.out, "reference_room")
    task = AdvanceTask(ref_room, sample_path, late_window=args.late_window)
    summary = task.run()
    baseline_path = write_baseline(
        task.trace_path, os.path.join(args.out, "baseline.jsonl"),
        meta={"seed": args.seed, "players": args.players,
              "disorder": args.disorder})
    return _emit({"status": "generated", "sample": sample_path,
                  "baseline": baseline_path, "reference_room": ref_room,
                  "tip": summary["tip"]})


def _run_task(args, resume):
    injector = None
    if getattr(args, "inject_kind", None):
        injector = OpInjector(args.inject_kind, args.inject_at_step,
                              args.late_window)
    task = AdvanceTask(args.room_dir, args.sample,
                       late_window=args.late_window)
    try:
        summary = task.run(resume=resume, crash_at=args.crash_at_step,
                           injector=injector)
    except HardCrash as crash:
        # Simulated kill -9: no exit report, no cleanup beyond what the
        # OS does; only the last committed snapshot survives on disk.
        print(json.dumps({"status": "hard_crash", "step": crash.step}),
              file=sys.stderr)
        os._exit(75)
    payload = dict(summary)
    if injector is not None and injector.report is not None:
        payload["injection"] = injector.report
    return _emit(payload)


def cmd_run(args):
    return _run_task(args, resume=False)


def cmd_resume(args):
    return _run_task(args, resume=True)


def cmd_verify(args):
    chain = _read_chain_from_trace(
        os.path.join(args.room_dir, "trace.jsonl"))
    return _emit(compare(chain, args.baseline))


def cmd_inject(args):
    if args.kind == "truncated-snapshot":
        report = inject_mod.truncate_snapshot(
            os.path.join(args.room_dir, "snapshot.json"))
    elif args.kind == "corrupt-baseline":
        report = inject_mod.corrupt_baseline(args.baseline,
                                             at_line=args.at_line)
    else:
        return _emit({"status": "error",
                      "message": f"use run --inject-kind for {args.kind}"}, 2)
    return _emit({"status": "injected", **report})


def cmd_cancel(args):
    open(os.path.join(args.room_dir, CANCEL_NAME), "w").close()
    return _emit({"status": "cancel_requested", "room_dir": args.room_dir})


def build_parser():
    parser = argparse.ArgumentParser(prog="replay_service")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("gen", help="generate sample + baseline")
    gen.add_argument("--seed", type=int, required=True)
    gen.add_argument("--players", type=int, default=4)
    gen.add_argument("--steps", type=int, default=60)
    gen.add_argument("--disorder", type=float, default=0.3)
    gen.add_argument("--room", default="room")
    gen.add_argument("--out", required=True)
    gen.add_argument("--late-window", type=int, default=2)
    gen.set_defaults(func=cmd_gen)

    def add_run_args(p):
        p.add_argument("--sample", required=True)
        p.add_argument("--room-dir", required=True)
        p.add_argument("--late-window", type=int, default=2)
        p.add_argument("--crash-at-step", type=int, default=None)
        p.add_argument("--inject-kind", default=None,
                       choices=inject_mod.OP_INJECTION_KINDS)
        p.add_argument("--inject-at-step", type=int, default=1)

    run = sub.add_parser("run", help="advance from the initial state")
    add_run_args(run)
    run.set_defaults(func=cmd_run)

    resume = sub.add_parser("resume", help="resume from the disk snapshot")
    add_run_args(resume)
    resume.set_defaults(func=cmd_resume)

    verify = sub.add_parser("verify", help="compare local trace vs baseline")
    verify.add_argument("--room-dir", required=True)
    verify.add_argument("--baseline", required=True)
    verify.set_defaults(func=cmd_verify)

    inject = sub.add_parser("inject", help="file-level fault injection")
    inject.add_argument("--kind", required=True,
                        choices=("truncated-snapshot", "corrupt-baseline"))
    inject.add_argument("--room-dir", default=None)
    inject.add_argument("--baseline", default=None)
    inject.add_argument("--at-line", type=int, default=2)
    inject.set_defaults(func=cmd_inject)

    cancel = sub.add_parser("cancel", help="request cooperative cancel")
    cancel.add_argument("--room-dir", required=True)
    cancel.set_defaults(func=cmd_cancel)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except BizError as err:
        return _emit({"status": "error", **err.to_dict()}, exit_code=1)


if __name__ == "__main__":
    sys.exit(main())
