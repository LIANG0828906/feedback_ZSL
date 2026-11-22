"""Command-line entry point for the image-forensics workbench.

Subcommands:
  generate   -- (re)create the deterministic sample dataset
  analyze    -- run the full dual-path pipeline over the dataset and export
  gui        -- launch the PyQt5 desktop workbench
"""

import argparse
import json
import os

from forensics.loader import load_dataset
from forensics.params import AnalysisParams, VersionControl
from forensics.engine import dual_path_analysis
from forensics.report import build_report, confusion_against_truth, LatencyTracker
from forensics.sample_generator import generate_all


ROOT = os.path.dirname(os.path.abspath(__file__))


def cmd_generate(args):
    manifests = generate_all(args.sample_dir)
    print("Generated %d deterministic samples in %s" % (len(manifests), args.sample_dir))


def cmd_analyze(args):
    params = AnalysisParams(
        block_size=args.block_size,
        preview_scale=args.preview_scale,
        noise_threshold=args.noise_threshold,
    )
    vc = VersionControl(params)
    samples = load_dataset(args.sample_dir, block_size=params.block_size)
    tracker = LatencyTracker()
    summary = []
    for sample in samples:
        dual = dual_path_analysis(sample, vc.latest.params)
        tracker.record(dual["preview_ms"])
        report = build_report(sample, dual, vc.latest, args.report_dir)
        confusion = confusion_against_truth(sample, dual["precise"]["decisions"])
        summary.append(
            {
                "sample": sample.name,
                "channels": sample.channels,
                "blocks": len(sample.block_index),
                "preview_ms": round(dual["preview_ms"], 3),
                "consistent": dual["consistent"],
                "ground_truth": confusion,
            }
        )
    latency = tracker.quantiles()
    result = {"samples": summary, "latency": latency}
    with open(os.path.join(args.report_dir, "run_summary.json"), "w") as fh:
        json.dump(result, fh, indent=2)
    print(json.dumps(result, indent=2))


def cmd_gui(args):
    from forensics.gui import run_gui

    return run_gui(args.sample_dir, args.report_dir, args.checkpoint_dir)


def build_parser():
    parser = argparse.ArgumentParser(description="图像取证与完整性工作台")
    parser.add_argument(
        "--sample-dir", default=os.path.join(ROOT, "samples")
    )
    parser.add_argument(
        "--report-dir", default=os.path.join(ROOT, "reports")
    )
    parser.add_argument(
        "--checkpoint-dir", default=os.path.join(ROOT, "checkpoints")
    )
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="generate deterministic samples")
    gen.set_defaults(func=cmd_generate)

    ana = sub.add_parser("analyze", help="run dual-path analysis and export")
    ana.add_argument("--block-size", type=int, default=32)
    ana.add_argument("--preview-scale", type=int, default=2)
    ana.add_argument("--noise-threshold", type=float, default=100.0)
    ana.set_defaults(func=cmd_analyze)

    gui = sub.add_parser("gui", help="launch the desktop GUI")
    gui.set_defaults(func=cmd_gui)
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    os.makedirs(args.report_dir, exist_ok=True)
    os.makedirs(args.checkpoint_dir, exist_ok=True)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
