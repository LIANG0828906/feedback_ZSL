"""命令行入口：

  python -m replay.cli gensuite --samples samples --baseline baseline
  python -m replay.cli gen --seed 7 --players 2 --frames 24 --disorder 1
  python -m replay.cli replay --sample samples/x.json --snapshots runs/x
  python -m replay.cli recover --sample samples/x.json --snapshots runs/x
  python -m replay.cli compare --trace runs/x.trace.jsonl \
      --baseline baseline/x.baseline.json
"""

from __future__ import annotations

import argparse
import json
import sys

from .compare import compare_trace
from .engine import ReplayEngine
from .errors import ReplayError
from .generator import (generate_sample, generate_suite, write_json,
                        write_trace)
from .snapshot import SnapshotStore


def _load_json(path: str):
    with open(path, "rb") as fh:
        return json.loads(fh.read().decode("utf-8"))


def _cmd_gensuite(args):
    names = generate_suite(args.samples, args.baseline)
    for n in names:
        print(f"generated {n}")


def _cmd_gen(args):
    sample = generate_sample(seed=args.seed, players=args.players,
                             frames=args.frames, disorder=args.disorder)
    write_json(args.out, sample)
    print(f"sample written to {args.out}")


def _run(sample_path, snapshot_dir, recover):
    match = _load_json(sample_path)
    store = SnapshotStore(snapshot_dir)
    engine = ReplayEngine.recover(match, store) if recover \
        else ReplayEngine(match)
    engine.run(on_frame=lambda r: store.commit(engine.snapshot_payload()))
    return engine


def _cmd_replay(args):
    engine = _run(args.sample, args.snapshots, recover=False)
    if args.trace:
        write_trace(args.trace, engine.trace_lines())
    print(json.dumps({"match_id": engine.match_id,
                      "frames": engine.frames,
                      "final_chain_hash": engine.chain_tip}, indent=2))


def _cmd_recover(args):
    engine = _run(args.sample, args.snapshots, recover=True)
    if args.trace:
        write_trace(args.trace, engine.trace_lines())
    print(json.dumps({"match_id": engine.match_id,
                      "recovered_to_frame": engine.frames,
                      "final_chain_hash": engine.chain_tip}, indent=2))


def _cmd_compare(args):
    trace = []
    with open(args.trace, "rb") as fh:
        for line in fh:
            line = line.strip()
            if line:
                trace.append(json.loads(line.decode("utf-8")))
    baseline = _load_json(args.baseline)
    result = compare_trace(trace, baseline)
    print(json.dumps(result, indent=2, ensure_ascii=False))
    sys.exit(0 if result["status"] == "match" else 1)


def main(argv=None):
    parser = argparse.ArgumentParser(prog="replay")
    sub = parser.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("gensuite")
    p.add_argument("--samples", default="samples")
    p.add_argument("--baseline", default="baseline")
    p.set_defaults(func=_cmd_gensuite)

    p = sub.add_parser("gen")
    p.add_argument("--seed", type=int, required=True)
    p.add_argument("--players", type=int, required=True)
    p.add_argument("--frames", type=int, required=True)
    p.add_argument("--disorder", type=int, default=1)
    p.add_argument("--out", required=True)
    p.set_defaults(func=_cmd_gen)

    p = sub.add_parser("replay")
    p.add_argument("--sample", required=True)
    p.add_argument("--snapshots", required=True)
    p.add_argument("--trace", default=None)
    p.set_defaults(func=_cmd_replay)

    p = sub.add_parser("recover")
    p.add_argument("--sample", required=True)
    p.add_argument("--snapshots", required=True)
    p.add_argument("--trace", default=None)
    p.set_defaults(func=_cmd_recover)

    p = sub.add_parser("compare")
    p.add_argument("--trace", required=True)
    p.add_argument("--baseline", required=True)
    p.set_defaults(func=_cmd_compare)

    args = parser.parse_args(argv)
    try:
        args.func(args)
    except ReplayError as exc:
        print(json.dumps(exc.to_dict(), indent=2, ensure_ascii=False))
        sys.exit(2)


if __name__ == "__main__":
    main()
