"""Command line entry points: sample generation and sync runs."""

from __future__ import annotations

import argparse
import os
import shutil

from .generator import standard_suite
from .pipeline import SyncConfig, SyncSession


def cmd_generate(args):
    if os.path.isdir(args.data_dir):
        shutil.rmtree(args.data_dir)
    outputs = standard_suite(args.data_dir)
    for spec, paths, truth_path in outputs:
        print(f"generated {spec.name}: {len(paths)} wavs, truth={truth_path}")


def cmd_run(args):
    os.makedirs(args.logs_dir, exist_ok=True)
    if os.path.isdir(args.out_dir):
        shutil.rmtree(args.out_dir)
    session = SyncSession()
    config = SyncConfig(
        seg_len_s=args.seg_len,
        ref_channel=args.ref_channel,
        mode=args.mode,
        strategy_threshold_s=args.strategy_threshold,
    )
    trace_path = os.path.join(args.logs_dir, f"{args.dataset}.trace.log")
    result = session.run(
        os.path.join(args.data_dir, args.dataset), config, args.out_dir, trace_path
    )
    print(f"strategy={result['strategy']} segments={result['n_segments']}")
    print(f"mode_comparison={result['mode_comparison']}")
    print(f"strategy_comparison={result['strategy_comparison']}")
    print(f"stitch={result['stitch_path']} shape={result['stitch_shape']}")
    print(f"report={result['report_path']} trace={result['trace_path']}")


def cmd_gui(args):
    from .gui import run_gui

    trace_path = os.path.join(args.logs_dir, "gui.trace.log")
    run_gui(
        os.path.join(args.data_dir, args.dataset),
        args.out_dir,
        trace_path,
        n_channels=args.channels,
    )


def main(argv=None):
    parser = argparse.ArgumentParser(prog="syncbench")
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_gen = sub.add_parser("generate", help="generate deterministic samples")
    p_gen.add_argument("--data-dir", default="data")
    p_gen.set_defaults(func=cmd_generate)

    p_run = sub.add_parser("run", help="run synchronization pipeline")
    p_run.add_argument("--data-dir", default="data")
    p_run.add_argument("--dataset", default="skew4")
    p_run.add_argument("--out-dir", default="reports/run")
    p_run.add_argument("--logs-dir", default="logs")
    p_run.add_argument("--seg-len", type=float, default=2.0)
    p_run.add_argument("--ref-channel", type=int, default=0)
    p_run.add_argument("--mode", choices=["linear", "sinc"], default="linear")
    p_run.add_argument("--strategy-threshold", type=float, default=10.0)
    p_run.set_defaults(func=cmd_run)

    p_gui = sub.add_parser("gui", help="launch diagnostic workbench")
    p_gui.add_argument("--data-dir", default="data")
    p_gui.add_argument("--dataset", default="skew4")
    p_gui.add_argument("--out-dir", default="reports/gui")
    p_gui.add_argument("--logs-dir", default="logs")
    p_gui.add_argument("--channels", type=int, default=4)
    p_gui.set_defaults(func=cmd_gui)

    args = parser.parse_args(argv)
    args.func(args)


if __name__ == "__main__":
    main()
