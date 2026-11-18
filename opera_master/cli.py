"""Command line interface for the opera mastering renderer."""

from __future__ import annotations

import argparse
import json
import os
import sys

from . import (
    ALL_PARTS,
    PART_BANGU,
    PART_BANGQIANG,
    PART_DALUO,
    PART_XIAOLUO,
)
from .cue import load_cue
from .errors import CueValidationError, OperaError, ResumeMismatchError
from .pipeline import InterruptController, render
from .synthesis import generate_channels


def build_demo_cue(workdir, seed=20261118):
    sr = 48000
    refs = [0.8, 2.0, 3.2, 4.4, 5.4]
    channels = []
    jitter = {
        PART_DALUO: 9,
        PART_XIAOLUO: -7,
        PART_BANGU: 5,
    }
    crosstalk = {
        PART_BANGQIANG: (PART_DALUO, -14.0, [0.0, 6.0]),
        PART_BANGU: (PART_XIAOLUO, -11.0, [0.0, 6.0]),
    }
    for part in ALL_PARTS:
        spec = {
            "part": part,
            "path": os.path.join("sources", f"{part}.wav"),
            "jitter_samples": jitter.get(part, 0),
        }
        if part in crosstalk:
            source, gain, window = crosstalk[part]
            spec["crosstalk_source"] = source
            spec["crosstalk_gain_db"] = gain
            spec["crosstalk_window"] = window
        channels.append(spec)
    return {
        "sample_rate": sr,
        "bit_depth": 16,
        "duration_seconds": 6.0,
        "seed": seed,
        "reference_times": refs,
        "channels": channels,
        "sections": [
            {
                "name": "起·锣鼓段",
                "start": 0.0,
                "end": 2.6,
                "group_gains_db": {
                    "打击乐组": 0.0,
                    "帮腔组": -3.0,
                    "月琴组": -2.0,
                },
            },
            {
                "name": "承·帮腔段",
                "start": 2.6,
                "end": 4.6,
                "group_gains_db": {
                    "打击乐组": -2.5,
                    "帮腔组": 0.5,
                    "月琴组": -1.0,
                },
            },
            {
                "name": "合·收束段",
                "start": 4.6,
                "end": 6.0,
                "group_gains_db": {
                    "打击乐组": 0.5,
                    "帮腔组": -1.0,
                    "月琴组": 0.0,
                },
            },
        ],
        "target_lufs": -18.0,
        "true_peak_limit_db": -1.0,
        "crossfade_seconds": 0.01,
        "block_samples": 24000,
        "output_master": os.path.join("out", "master.wav"),
        "output_report": os.path.join("out", "decouple_report.json"),
        "snapshot_dir": os.path.join("out", "snapshots"),
    }


def _emit_error(err):
    if isinstance(err, CueValidationError):
        payload = err.to_payload()
    elif isinstance(err, OperaError):
        payload = err.to_payload()
    else:
        payload = {"code": "UNHANDLED", "message": str(err)}
    sys.stderr.write(json.dumps(payload, ensure_ascii=False, indent=2) + "\n")
    if isinstance(err, ResumeMismatchError):
        return err.exit_code
    if isinstance(err, OperaError):
        return err.exit_code
    return 1


def main(argv=None):
    parser = argparse.ArgumentParser(prog="opera-master")
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo-cue", help="write the deterministic demo cue")
    demo.add_argument("workdir")
    demo.add_argument("--seed", type=int, default=20261118)

    gen = sub.add_parser("generate", help="synthesize the six source WAVs")
    gen.add_argument("cue")

    rnd = sub.add_parser("render", help="decouple, align, mix and master")
    rnd.add_argument("cue")
    rnd.add_argument(
        "--disable-detection",
        action="append",
        default=[],
        help="part name whose onset detection should fail (test hook)",
    )

    args = parser.parse_args(argv)
    try:
        if args.command == "demo-cue":
            os.makedirs(args.workdir, exist_ok=True)
            cue_path = os.path.join(args.workdir, "cue.json")
            payload = build_demo_cue(args.workdir, args.seed)
            with open(cue_path, "w", encoding="utf-8") as fh:
                json.dump(payload, fh, ensure_ascii=False, indent=2)
            print(cue_path)
            return 0

        cue = load_cue(args.cue)
        base_dir = os.path.dirname(os.path.abspath(args.cue))
        if args.command == "generate":
            signals = generate_channels(cue, base_dir)
            print(json.dumps({
                "generated": [ch.part for ch in cue.channels],
                "samples": next(iter(signals.values())).shape[0],
            }, ensure_ascii=False))
            return 0

        if args.command == "render":
            controller = InterruptController()
            controller.install()
            result = render(
                cue,
                base_dir=base_dir,
                disabled_detection=tuple(args.disable_detection),
                interrupt=controller,
            )
            if result.get("interrupted"):
                print(json.dumps({
                    "status": "INTERRUPTED",
                    "exit_code": 0,
                    "completed_blocks": result["completed_blocks"],
                    "total_blocks": result["total_blocks"],
                }, ensure_ascii=False))
            else:
                print(json.dumps({
                    "status": "OK",
                    "exit_code": 0,
                    "report": cue.output_report,
                    "master": cue.output_master,
                }, ensure_ascii=False))
            return 0
    except (OperaError, CueValidationError) as exc:
        return _emit_error(exc)
    return 0


if __name__ == "__main__":
    sys.exit(main())
