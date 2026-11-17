"""Command line entry points for the stage-noise toolkit."""

from __future__ import annotations

import argparse
import json
import os
import sys
import time

import numpy as np
import soundfile as sf

from .errors import BusinessCode, StageNoiseError, fail
from .generator import generate, verify_wav_headers
from .manifest import load_manifest
from .persistence.snapshot import SnapshotStore
from .pipeline import PipelineInterrupted, run_pipeline
from .reporting.reports import build_report, write_recovery_report, write_report


def _default_paths(manifest_path: str) -> dict:
    base = os.path.dirname(os.path.abspath(manifest_path))
    return {
        "state_dir": os.path.join(base, ".render_state"),
        "master": os.path.join(base, "master_stereo.wav"),
        "report": os.path.join(base, "noise_report.json"),
        "recovery": os.path.join(base, "recovery_report.json"),
    }


def cmd_generate(args) -> int:
    manifest = load_manifest(args.manifest)
    truth = generate(manifest)
    print(json.dumps({
        "status": "ok",
        "microphones": len(manifest.mics),
        "action_segments": len(manifest.actions),
        "truth_file": os.path.basename(truth["truth_path"]),
    }, indent=2))
    return 0


def cmd_render(args) -> int:
    started = time.monotonic()
    manifest = load_manifest(args.manifest)
    paths = _default_paths(args.manifest)
    if args.state_dir:
        paths["state_dir"] = args.state_dir
    if args.master:
        paths["master"] = args.master
    if args.report:
        paths["report"] = args.report

    try:
        result = run_pipeline(
            manifest, paths["state_dir"], paths,
            interrupt_after_chunk=args.interrupt_after_chunk)
    except PipelineInterrupted:
        store = SnapshotStore(paths["state_dir"])
        snapshot = store.load()
        write_recovery_report(paths["recovery"], manifest, snapshot,
                              time.monotonic() - started)
        print(json.dumps({
            "status": "interrupted_resumable",
            "completed_chunks": snapshot["completed_chunks"],
            "total_chunks": snapshot["total_chunks"],
            "recovery_report": os.path.basename(paths["recovery"]),
        }, indent=2))
        return 0

    report = build_report(manifest, result, time.monotonic() - started)
    write_report(report, paths["report"])
    print(json.dumps({
        "status": "ok",
        "master": os.path.basename(paths["master"]),
        "report": os.path.basename(paths["report"]),
        "final_loudness_lufs":
            report["mastering"]["final_integrated_loudness_lufs"],
        "final_true_peak_dbtp":
            report["mastering"]["final_true_peak_dbtp"],
    }, indent=2))
    return 0


def cmd_validate(args) -> int:
    manifest = load_manifest(args.manifest)
    verify_wav_headers(manifest)
    print(json.dumps({
        "status": "ok",
        "microphones": [m.name for m in manifest.mics],
        "action_segments": [a.segment_id for a in manifest.actions],
    }, indent=2))
    return 0


def cmd_compare(args) -> int:
    a, sr_a = sf.read(args.reference, dtype="float64", always_2d=True)
    b, sr_b = sf.read(args.candidate, dtype="float64", always_2d=True)
    if sr_a != sr_b:
        raise StageNoiseError(BusinessCode.SAMPLE_RATE_MISMATCH,
                              "compare: sample rates differ")
    if a.shape != b.shape:
        raise StageNoiseError(
            BusinessCode.INVALID_MANIFEST,
            "compare: shapes differ",
            details={"reference_shape": list(a.shape),
                     "candidate_shape": list(b.shape)})
    diff = np.nonzero(~np.isclose(a, b, atol=0.0, rtol=0.0))
    if diff[0].size == 0:
        print(json.dumps({"status": "identical",
                          "samples": int(a.shape[0])}, indent=2))
        return 0
    first = diff[0][0]
    channels = sorted(int(c) for c in np.unique(diff[1][:1000]))
    payload = {
        "status": "different",
        "first_difference_sample_offset": int(first),
        "difference_channels": channels,
        "difference_sample_count": int(diff[0].size),
        "max_abs_delta": float(np.max(np.abs(a - b))),
    }
    json.dump(payload, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 40


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="stage-noise",
        description="Deterministic stage machinery noise tracing/denoise kit")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="synthesize deterministic WAVs")
    gen.add_argument("manifest")
    gen.set_defaults(func=cmd_generate)

    render = sub.add_parser("render", help="trace, denoise, render master")
    render.add_argument("manifest")
    render.add_argument("--state-dir")
    render.add_argument("--master")
    render.add_argument("--report")
    render.add_argument("--interrupt-after-chunk", type=int, default=None,
                        help="test hook: stop gracefully after N chunks")
    render.set_defaults(func=cmd_render)

    validate = sub.add_parser("validate", help="validate manifest and WAVs")
    validate.add_argument("manifest")
    validate.set_defaults(func=cmd_validate)

    compare = sub.add_parser("compare", help="byte-level master comparison")
    compare.add_argument("reference")
    compare.add_argument("candidate")
    compare.set_defaults(func=cmd_compare)
    return parser


def main(argv: list | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except StageNoiseError as error:
        fail(error)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
