from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

from .config import CalibrationConfig, TARGET_PRESETS
from .errors import CalibrationError
from .pipeline import CalibrationPipeline
from .report import write_diagnostic_bundle
from .samplegen import generate_dataset


def _config_from_args(args: argparse.Namespace) -> CalibrationConfig:
    if args.config:
        with open(args.config, "r", encoding="utf-8") as handle:
            config = CalibrationConfig.from_dict(json.load(handle))
    else:
        config = CalibrationConfig()
    changes: dict[str, object] = {}
    if args.reference:
        changes["reference_channel"] = args.reference
    if args.frame_size:
        changes["frame_size"] = args.frame_size
    if args.overlap is not None:
        changes["overlap"] = args.overlap
    if args.window:
        changes["window"] = args.window
    if args.band_fraction:
        changes["band_fraction"] = args.band_fraction
    if args.smoothing is not None:
        changes["smoothing_octaves"] = args.smoothing
    if args.taps:
        changes["filter_taps"] = args.taps
    if args.target_preset:
        changes["target_curve"] = TARGET_PRESETS[args.target_preset]
    if args.error_threshold is not None:
        changes["error_threshold_db"] = args.error_threshold
    return config.update(**changes) if changes else config


def _run(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    pipeline = CalibrationPipeline(args.cache, cache_enabled=not args.no_cache)
    result = pipeline.run(args.wav, config)
    paths = write_diagnostic_bundle(
        result,
        args.report,
        args.trace,
        args.coefficients,
    )
    affected_summary: dict[str, int] = {}
    for node in result.affected_nodes:
        stage = node.split(":", 1)[0]
        affected_summary[stage] = affected_summary.get(stage, 0) + 1
    summary = {
        "version": result.version,
        "content_hash": result.content_hash,
        "report_hash": paths["report_hash"],
        "trace_hash": paths["trace_hash"],
        "coefficients_hash": paths["coefficients_hash"],
        "cache": result.cache_summary,
        "affected_nodes": affected_summary,
        "warnings": result.consistency["warning_count"],
        "error_budget_bands": sum(
            1
            for channel_errors in result.errors.values()
            for item in channel_errors
            if item["status"] == "E_ERROR_BUDGET"
        ),
    }
    print(json.dumps(summary, sort_keys=True, indent=2, ensure_ascii=True))
    return 0


def _doctor_handler(args: argparse.Namespace) -> int:
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    os.environ.setdefault("QT_OPENGL", "software")
    import platform

    import numpy
    import PySide6
    import scipy
    from PySide6.QtCore import QRect
    from PySide6.QtGui import QColor, QImage, QPainter
    from PySide6.QtWidgets import QApplication

    app = QApplication.instance() or QApplication([])
    image = QImage(64, 64, QImage.Format_ARGB32)
    image.fill(QColor("#ffffff"))
    painter = QPainter(image)
    painter.fillRect(QRect(8, 8, 48, 48), QColor("#16a34a"))
    painter.end()
    rendered = image.pixelColor(32, 32).green() > 0
    report = {
        "python": platform.python_version(),
        "numpy": numpy.__version__,
        "scipy": scipy.__version__,
        "pyside6": PySide6.__version__,
        "qt_platform": app.platformName(),
        "offscreen_software_render": bool(rendered),
    }
    print(json.dumps(report, sort_keys=True, indent=2, ensure_ascii=True))
    return 0 if rendered else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="studio-calib")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic WAV samples")
    generate.add_argument("--out", default="samples")
    generate.add_argument("--seed", type=int, default=20261120)
    generate.set_defaults(handler=_generate_handler)

    run = subparsers.add_parser("run", help="run the processing chain and write diagnostics")
    run.add_argument("--wav", default="samples/surround51.wav")
    run.add_argument("--config")
    run.add_argument("--cache", default="cache")
    run.add_argument("--report", default="reports/diagnostic.json")
    run.add_argument("--trace", default="reports/trace.jsonl")
    run.add_argument("--coefficients", default="reports/filter_coefficients.json")
    run.add_argument("--reference")
    run.add_argument("--frame-size", type=int)
    run.add_argument("--overlap", type=float)
    run.add_argument("--window", choices=["hann", "hamming", "blackman", "rectangular"])
    run.add_argument("--band-fraction", type=int, choices=[1, 3])
    run.add_argument("--smoothing", type=float)
    run.add_argument("--taps", type=int)
    run.add_argument("--target-preset", choices=sorted(TARGET_PRESETS))
    run.add_argument("--error-threshold", type=float)
    run.add_argument("--no-cache", action="store_true")
    run.set_defaults(handler=_run)

    gui = subparsers.add_parser("gui", help="launch the PySide6 workbench")
    gui.add_argument("--wav", default="samples/surround51.wav")
    gui.add_argument("--cache", default="cache")
    gui.set_defaults(handler=_gui_handler)

    doctor = subparsers.add_parser(
        "doctor", help="verify Python packages and offscreen software rendering"
    )
    doctor.set_defaults(handler=_doctor_handler)
    return parser


def _generate_handler(args: argparse.Namespace) -> int:
    manifest = generate_dataset(args.out, seed=args.seed)
    print(json.dumps(manifest, sort_keys=True, indent=2, ensure_ascii=True))
    return 0


def _gui_handler(args: argparse.Namespace) -> int:
    from .gui import launch_gui

    return launch_gui(args.wav, args.cache)


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except CalibrationError as exc:
        print(json.dumps(exc.to_dict(), sort_keys=True), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

