"""Command line entry points.

    python -m syncdemo generate [--seed N] [--out DIR] [--ticks N]
    python -m syncdemo verify   [--sample DIR] [--work DIR]
    python -m syncdemo demo     [--sample DIR] [--headless FRAMES]
"""

from __future__ import annotations

import argparse
import sys
import tempfile

from . import engine, generator, runner
from .checkpoint import CheckpointStore
from .errors import SyncError
from .schema import load_sample
from .workers import WindowPool


def _cmd_generate(args):
    path = generator.generate(outdir=args.out, seed=args.seed, ticks=args.ticks,
                              num_players=args.players)
    print(f"sample generated at {path} (seed={args.seed}, ticks={args.ticks})")
    return 0


def _cmd_verify(args):
    sample = load_sample(args.sample)
    ecfg = engine.EngineConfig()
    rcfg = runner.RunConfig()
    meta = sample.meta

    work = args.work or tempfile.mkdtemp(prefix="syncdemo-verify-")
    pool = WindowPool(max_workers=rcfg.max_workers, queue_size=rcfg.queue_size,
                      result_buffer=rcfg.result_buffer)
    try:
        store_rt = CheckpointStore(
            f"{work}/rt", seed=meta["seed"],
            input_fingerprint=meta["input_fingerprint"],
            algo_version=meta["algo_version"],
        )
        realtime = runner.run_realtime(sample, rcfg, ecfg, store=store_rt,
                                       pool=pool)

        store_rs = CheckpointStore(
            f"{work}/rs", seed=meta["seed"],
            input_fingerprint=meta["input_fingerprint"],
            algo_version=meta["algo_version"],
        )
        cut = max(1, sample.max_tick // 2)
        partial = runner.run_realtime(sample, rcfg, ecfg, store=store_rs,
                                      cancel_after_tick=cut)
        assert partial.status == "cancelled"
        resumed = runner.run_resume(sample, rcfg, ecfg, store=store_rs)

        replay = runner.run_replay(sample, rcfg, ecfg)
    finally:
        pool.close()

    runner.compare(realtime, resumed)
    runner.compare(realtime, replay)

    # Cross-check the run against the independently generated truth:
    # key-tick authoritative states, per-client visible states, settlement.
    runner.compare_truth(realtime, sample.truth)

    print("three-path consistency: OK (realtime == resumed == replay)")
    print("truth check: OK (key states + client visible + settlement)")
    print(f"settlement: {realtime.settlement}")
    input_violations = realtime.input_delay_violations or []
    correction_violations = realtime.correction_delay_violations or []
    print(f"input delay violations: {input_violations or 'none'}")
    print(f"correction delay violations: {correction_violations or 'none'}")
    for cid, corr in realtime.corrections.items():
        total = sum(c.total for c in corr)
        print(f"client {cid}: rollbacks={len(corr)} "
              f"correction_total={total:.3f}")
    return 0


def _cmd_demo(args):
    from .ui import DemoApp

    app = DemoApp(args.sample, headless=args.headless is not None)
    app.run(max_frames=args.headless)
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(prog="syncdemo")
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_gen = sub.add_parser("generate", help="generate a deterministic sample")
    p_gen.add_argument("--seed", type=int, default=generator.DEFAULT_SEED)
    p_gen.add_argument("--out", default="sample")
    p_gen.add_argument("--ticks", type=int, default=generator.DEFAULT_TICKS)
    p_gen.add_argument("--players", type=int, default=generator.DEFAULT_PLAYERS)
    p_gen.set_defaults(fn=_cmd_generate)

    p_ver = sub.add_parser("verify", help="run three-path consistency check")
    p_ver.add_argument("--sample", default="sample")
    p_ver.add_argument("--work", default=None)
    p_ver.set_defaults(fn=_cmd_verify)

    p_demo = sub.add_parser("demo", help="interactive sync demo (pygame)")
    p_demo.add_argument("--sample", default="sample")
    p_demo.add_argument("--headless", type=int, default=None,
                        metavar="FRAMES", help="run N frames without a display")
    p_demo.set_defaults(fn=_cmd_demo)

    args = parser.parse_args(argv)
    try:
        return args.fn(args)
    except SyncError as exc:
        print(str(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
