"""Command-line entry points for generation, runs, verification, and UI."""

from __future__ import annotations

import argparse
import copy
import json
import os
import platform
import sys
import threading
import time
from datetime import datetime, timezone
from importlib.metadata import PackageNotFoundError, version as package_version
from pathlib import Path
from typing import Any

import numpy as np

from . import __version__
from .compression import CompressionConfig
from .errors import ALGORITHM_VERSION, WorkbenchError
from .metrics import evaluate_whole
from .pipeline import PipelineResult, compare_pipeline_results, run_pipeline
from .pngio import load_image
from .report import export_quality_report
from .samples import generate_samples, load_manifest
from .state import BlockStateMachine, COMMITTED
from .util import check_writable_directory

DEFAULT_SAMPLES_DIR = Path("artifacts/samples")
DEFAULT_QUICK_SAMPLES_DIR = Path("artifacts/samples_quick")
DEFAULT_CHECKPOINT = Path("artifacts/state/checkpoints.sqlite")
DEFAULT_RUNS_DIR = Path("artifacts/runs")


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False))


def _resolve_sample(samples_dir: Path, name: str) -> tuple[dict[str, Any], np.ndarray, np.ndarray]:
    manifest = load_manifest(samples_dir)
    entry = next((item for item in manifest["samples"] if item["name"] == name), None)
    if entry is None:
        raise WorkbenchError("E_DECODE", "sample is not present in the manifest", {"sample": name, "samples_dir": str(samples_dir)})
    image = load_image(samples_dir / entry["image_path"])
    truth_path = samples_dir / entry["truth_path"]
    try:
        with np.load(truth_path, allow_pickle=False) as truth:
            edge_mask = truth["edge_mask"].astype(bool, copy=False)
    except (OSError, ValueError, KeyError) as exc:
        raise WorkbenchError("E_DECODE", "sample truth cannot be decoded", {"path": str(truth_path), "reason": str(exc)}) from exc
    if image.shape[:2] != edge_mask.shape:
        raise WorkbenchError("E_DECODE", "sample truth shape does not match image", {"path": str(truth_path)})
    return entry, image, edge_mask


def _write_verification_artifact(payload: dict[str, Any], output: Path) -> None:
    from .util import atomic_write_json

    atomic_write_json(output, payload)


def _versions() -> dict[str, Any]:
    versions = {
        "python": platform.python_version(),
        "platform": platform.platform(),
        "numpy": np.__version__,
        "imq_workbench": __version__,
        "algorithm_version": ALGORITHM_VERSION,
    }
    try:
        versions["pip"] = package_version("pip")
    except PackageNotFoundError:
        versions["pip"] = "unavailable"
    try:
        from PySide6 import __version__ as pyside_version

        versions["PySide6"] = pyside_version
    except Exception:
        versions["PySide6"] = "unavailable"
    return versions


def command_generate(args: argparse.Namespace) -> int:
    manifest = generate_samples(args.output_dir, preset=args.preset, seed=args.seed)
    summary = {
        "status": "ok",
        "manifest": str((Path(args.output_dir) / "manifest.json").resolve()),
        "preset": manifest["preset"],
        "seed": manifest["seed"],
        "sample_count": manifest["sample_count"],
        "samples": [
            {
                "name": item["name"],
                "shape": [item["height"], item["width"], item["channels"]],
                "content_hash": item["content_hash"],
                "edge_pixel_count": item["edge_pixel_count"],
            }
            for item in manifest["samples"]
        ],
    }
    if args.json:
        _print_json(summary)
    else:
        print(f"generated {summary['sample_count']} samples in {Path(args.output_dir).resolve()}")
        for item in summary["samples"]:
            print(f"  {item['name']}: {item['shape'][1]}x{item['shape'][0]}x{item['shape'][2]}, edges={item['edge_pixel_count']}")
    return 0


def _make_config(args: argparse.Namespace) -> CompressionConfig:
    return CompressionConfig(
        block_size=args.block_size,
        quant_step=args.quant_step,
        evaluation_window=args.eval_window,
        max_workers=args.workers,
        max_buffer_pixels=args.max_buffer_pixels,
    )


def _run_command(
    args: argparse.Namespace,
    *,
    image_key: str,
    image: np.ndarray,
    truth: np.ndarray,
    config: CompressionConfig,
    mode: str,
    checkpoint: Path,
    cancel_event: threading.Event | None = None,
    progress_callback=None,
) -> PipelineResult:
    return run_pipeline(
        image,
        truth,
        config,
        checkpoint_path=checkpoint,
        image_key=image_key,
        mode=mode,
        cancel_event=cancel_event,
        progress_callback=progress_callback,
    )


def command_run(args: argparse.Namespace) -> int:
    samples_dir = Path(args.samples_dir)
    if args.image:
        image = load_image(args.image)
        if not args.truth:
            raise WorkbenchError("E_PARA", "--truth is required with --image", {"field": "truth"})
        try:
            with np.load(args.truth, allow_pickle=False) as truth_file:
                truth = truth_file["edge_mask"].astype(bool, copy=False)
        except (OSError, ValueError, KeyError) as exc:
            raise WorkbenchError("E_DECODE", "truth cannot be decoded", {"path": args.truth, "reason": str(exc)}) from exc
        if truth.shape != image.shape[:2]:
            raise WorkbenchError("E_PARA", "truth mask shape must match image", {"field": "truth"})
        sample_metadata = {"source": str(Path(args.image).resolve()), "truth": str(Path(args.truth).resolve())}
        image_key = args.image_key or Path(args.image).stem
    else:
        entry, image, truth = _resolve_sample(samples_dir, args.sample)
        sample_metadata = entry
        image_key = args.image_key or entry["name"]

    config = _make_config(args)
    event = threading.Event() if args.stop_after is not None else None

    def progress(done: int, total: int, index: int, state: str) -> None:
        if args.crash_after is not None and state != "loaded" and done >= args.crash_after:
            os._exit(137)
        if event is not None and state != "loaded" and done >= args.stop_after:
            event.set()

    result = _run_command(
        args,
        image_key=image_key,
        image=image,
        truth=truth,
        config=config,
        mode=args.mode,
        checkpoint=Path(args.checkpoint),
        cancel_event=event,
        progress_callback=progress,
    )
    report = None
    if result.complete and not args.no_report:
        output_dir = Path(args.output_dir) if args.output_dir else DEFAULT_RUNS_DIR / f"{image_key}_{args.mode}"
        report = export_quality_report(result, output_dir, sample_metadata=sample_metadata)
    payload = result.summary()
    payload["report"] = report
    if args.json:
        _print_json(payload)
    else:
        print(f"{result.mode}: {result.progress['committed']}/{result.progress['total']} blocks committed")
        print(f"complete={result.complete} processed={result.processed_blocks} resumed={result.resumed_blocks}")
        if result.metrics:
            print(
                "metrics: "
                f"PSNR={result.metrics['psnr_db']:.6f} dB, "
                f"MSE={result.metrics['mse']:.9f}, "
                f"SSIM={result.metrics['ssim']:.9f}, "
                f"edge={result.metrics['edge_fidelity']:.9f}"
            )
        else:
            print("incomplete snapshot: no complete quality report is claimed")
        if report:
            print(f"report: {report['report_path']}")
    return 0 if result.complete else 3


def _check(name: str, passed: bool, **details: Any) -> dict[str, Any]:
    return {"name": name, "passed": bool(passed), **details}


def command_verify(args: argparse.Namespace) -> int:
    started = time.perf_counter()
    samples_dir = Path(args.samples_dir)
    try:
        manifest = load_manifest(samples_dir)
        if manifest.get("preset") != "quick":
            manifest = generate_samples(samples_dir, preset="quick")
    except WorkbenchError:
        manifest = generate_samples(samples_dir, preset="quick")
    entry, image, truth = _resolve_sample(samples_dir, args.sample)
    config = CompressionConfig(
        block_size=args.block_size,
        quant_step=args.quant_step,
        evaluation_window=args.eval_window,
        max_workers=args.workers,
        max_buffer_pixels=args.max_buffer_pixels,
    )
    config.validate_for_image(image)
    checkpoint = Path(args.checkpoint)
    checks: list[dict[str, Any]] = []

    realtime = _run_command(
        args,
        image_key=f"verify:{entry['name']}",
        image=image,
        truth=truth,
        config=config,
        mode="realtime",
        checkpoint=checkpoint,
    )
    checks.append(_check("realtime_complete", realtime.complete, progress=realtime.progress))
    checks.append(
        _check(
            "reconstruction_contract",
            realtime.reconstructed.shape == image.shape and realtime.reconstructed.dtype == image.dtype,
            shape=list(realtime.reconstructed.shape),
            dtype=str(realtime.reconstructed.dtype),
        )
    )

    partial_event = threading.Event()
    partial_target = max(1, min(realtime.progress["total"] - 1, args.pause_after))

    def partial_progress(done: int, total: int, index: int, state: str) -> None:
        if state != "loaded" and done >= partial_target:
            partial_event.set()

    partial = _run_command(
        args,
        image_key=f"verify:{entry['name']}",
        image=image,
        truth=truth,
        config=config,
        mode="realtime",
        checkpoint=checkpoint,
        cancel_event=partial_event,
        progress_callback=partial_progress,
    )
    checks.append(
        _check(
            "paused_snapshot_is_incomplete",
            not partial.complete and partial.progress["committed"] > 0,
            progress=partial.progress,
        )
    )
    report_blocked = False
    try:
        export_quality_report(partial, Path(args.runs_dir) / "_must_not_exist")
    except WorkbenchError as exc:
        report_blocked = exc.code == "E_STATE"
    checks.append(_check("incomplete_report_rejected", report_blocked))

    resumed = _run_command(
        args,
        image_key=f"verify:{entry['name']}",
        image=image,
        truth=truth,
        config=config,
        mode="resume",
        checkpoint=checkpoint,
    )
    checks.append(
        _check(
            "resume_complete",
            resumed.complete and resumed.resumed_blocks == partial.progress["committed"],
            resumed_blocks=resumed.resumed_blocks,
            processed_blocks=resumed.processed_blocks,
        )
    )
    recomputed = _run_command(
        args,
        image_key=f"verify:{entry['name']}",
        image=image,
        truth=truth,
        config=config,
        mode="recompute",
        checkpoint=checkpoint,
    )
    checks.append(_check("recompute_complete", recomputed.complete, progress=recomputed.progress))

    for label, first, second in (
        ("realtime_vs_resume", realtime, resumed),
        ("realtime_vs_recompute", realtime, recomputed),
        ("resume_vs_recompute", resumed, recomputed),
    ):
        try:
            comparison = compare_pipeline_results(first, second)
            checks.append(_check(label, True, **comparison))
        except WorkbenchError as exc:
            checks.append(_check(label, False, error=exc.to_dict()))

    whole = evaluate_whole(image, recomputed.reconstructed, truth, evaluation_window=config.evaluation_window)
    metric_deltas = {
        key: float(abs(recomputed.metrics[key] - whole[key]))
        for key in ("mse", "psnr_db", "ssim", "edge_fidelity")
    }
    checks.append(
        _check(
            "blockwise_matches_whole_image",
            max(metric_deltas.values()) <= 1e-6,
            deltas=metric_deltas,
            tolerance=1e-6,
        )
    )

    monotonic_rows = []
    previous_mse = -1.0
    previous_psnr = float("inf")
    monotonic = True
    for quant_step in (1.0, 2.0, 4.0, 8.0):
        run = _run_command(
            args,
            image_key=f"verify-monotonic:{entry['name']}",
            image=image,
            truth=truth,
            config=CompressionConfig(
                block_size=config.block_size,
                quant_step=quant_step,
                evaluation_window=config.evaluation_window,
                max_workers=config.max_workers,
                max_buffer_pixels=config.max_buffer_pixels,
            ),
            mode="recompute",
            checkpoint=Path(args.monotonic_checkpoint),
        )
        assert run.metrics is not None
        monotonic = monotonic and run.metrics["mse"] >= previous_mse and run.metrics["psnr_db"] <= previous_psnr
        previous_mse = run.metrics["mse"]
        previous_psnr = run.metrics["psnr_db"]
        monotonic_rows.append(
            {
                "quant_step": quant_step,
                "mse": run.metrics["mse"],
                "psnr_db": run.metrics["psnr_db"],
            }
        )
    checks.append(_check("quant_step_monotonic", monotonic, rows=monotonic_rows))

    error_checks = []
    try:
        CompressionConfig(quant_step=0.0).validate()
    except WorkbenchError as exc:
        error_checks.append(_check("E_PARA_quant_step", exc.code == "E_PARA" and exc.details.get("field") == "quant_step", error=exc.to_dict()))
    try:
        CompressionConfig(block_size=12).validate()
    except WorkbenchError as exc:
        error_checks.append(_check("E_PARA_block_size", exc.code == "E_PARA" and exc.details.get("field") == "block_size", error=exc.to_dict()))
    try:
        CompressionConfig(block_size=8, evaluation_window=33).validate()
    except WorkbenchError as exc:
        error_checks.append(_check("E_BAND", exc.code == "E_BAND", error=exc.to_dict()))
    state = BlockStateMachine(0)
    try:
        state.transition(COMMITTED)
    except WorkbenchError as exc:
        error_checks.append(
            _check("E_STATE_preserves_state", exc.code == "E_STATE" and state.state == "pending", error=exc.to_dict(), state=state.state)
        )

    corrupted = Path(args.runs_dir) / "_corrupted.png"
    corrupted.parent.mkdir(parents=True, exist_ok=True)
    corrupted.write_bytes(b"\x89PNG\r\n\x1a\nbroken")
    try:
        load_image(corrupted)
    except WorkbenchError as exc:
        error_checks.append(_check("E_DECODE", exc.code == "E_DECODE", error=exc.to_dict()))

    divergent = copy.copy(recomputed)
    divergent.reconstructed = recomputed.reconstructed.copy()
    tamper_y, tamper_x = min(1, image.shape[0] - 1), min(1, image.shape[1] - 1)
    if divergent.reconstructed.ndim == 2:
        divergent.reconstructed[tamper_y, tamper_x] ^= 1
    else:
        divergent.reconstructed[tamper_y, tamper_x, 0] ^= 1
    try:
        compare_pipeline_results(recomputed, divergent)
    except WorkbenchError as exc:
        expected_block = (tamper_y // config.block_size) * recomputed.grid[1] + tamper_x // config.block_size
        error_checks.append(
            _check(
                "E_DIFF_first_pixel",
                exc.code == "E_DIFF"
                and exc.details.get("stage") == "reconstruction"
                and exc.details.get("first_pixel") == {"y": tamper_y, "x": tamper_x}
                and exc.details.get("block_index") == expected_block,
                error=exc.to_dict(),
            )
        )
    checks.extend(error_checks)

    report = export_quality_report(
        realtime,
        Path(args.runs_dir) / "verification",
        sample_metadata=entry,
    )
    all_passed = all(check["passed"] for check in checks)
    payload = {
        "status": "ok" if all_passed else "failed",
        "verified_utc": datetime.now(timezone.utc).isoformat(),
        "versions": _versions(),
        "sample": entry["name"],
        "config": config.to_dict(),
        "checks": checks,
        "all_passed": all_passed,
        "quality_report": report,
        "elapsed_seconds": time.perf_counter() - started,
    }
    output = Path(args.output) if args.output else Path(args.runs_dir) / "verification.json"
    _write_verification_artifact(payload, output)
    _print_json(payload)
    return 0 if all_passed else 1


def command_check(args: argparse.Namespace) -> int:
    checks = []
    checks.append(_check("python_3_11_plus", sys.version_info >= (3, 11), version=platform.python_version()))
    probes = {}
    for key, path in (
        ("samples", Path(args.samples_dir)),
        ("checkpoints", Path(args.checkpoint).parent),
        ("runs", Path(args.runs_dir)),
    ):
        probes[key] = check_writable_directory(path)
    checks.append(_check("workspace_writable", True, paths=probes))
    checks.append(_check("numpy_available", True, version=np.__version__))
    try:
        pip_version = package_version("pip")
        checks.append(_check("pip_available", True, version=pip_version))
    except PackageNotFoundError:
        checks.append(_check("pip_available", False, error="pip is not installed in the active environment"))
    try:
        from PySide6 import __version__ as pyside_version

        checks.append(_check("pyside6_available", True, version=pyside_version))
    except Exception as exc:
        checks.append(_check("pyside6_available", False, error=str(exc), cli_fallback=True))
    payload = {
        "status": "ok",
        "versions": _versions(),
        "checks": checks,
        "start_commands": {
            "generate": f"python -m imq_workbench generate --output-dir {args.samples_dir}",
            "ui": f"python -m imq_workbench app --samples-dir {args.samples_dir}",
            "cli": f"python -m imq_workbench run --samples-dir {args.samples_dir} --sample gradient_gray_512 --mode realtime",
            "resume": f"python -m imq_workbench run --samples-dir {args.samples_dir} --sample gradient_gray_512 --mode resume",
            "verify": "python -m imq_workbench verify",
        },
    }
    _print_json(payload)
    return 0


def command_app(args: argparse.Namespace) -> int:
    try:
        from .ui import launch
    except Exception as exc:
        raise WorkbenchError(
            "E_UI",
            "PySide6 UI is unavailable; use the command-line entry point",
            {"reason": str(exc)},
        ) from exc
    return launch(
        samples_dir=Path(args.samples_dir),
        checkpoint=Path(args.checkpoint),
        runs_dir=Path(args.runs_dir),
    )


def _add_run_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--samples-dir", default=str(DEFAULT_SAMPLES_DIR))
    parser.add_argument("--sample", default="gradient_gray_512")
    parser.add_argument("--image", help="explicit image path (.png or .npy)")
    parser.add_argument("--truth", help="explicit truth .npz with edge_mask")
    parser.add_argument("--image-key", help="stable image identifier for checkpoint selection")
    parser.add_argument("--block-size", type=int, default=16)
    parser.add_argument("--quant-step", type=float, default=8.0)
    parser.add_argument("--eval-window", type=int, default=7)
    parser.add_argument("--workers", type=int, default=2)
    parser.add_argument("--max-buffer-pixels", type=int, default=8_000_000)
    parser.add_argument("--checkpoint", default=str(DEFAULT_CHECKPOINT))
    parser.add_argument("--output-dir")
    parser.add_argument("--mode", choices=("realtime", "resume", "recompute"), default="realtime")
    parser.add_argument("--stop-after", type=int, help="request a cooperative pause after N newly committed blocks")
    parser.add_argument("--crash-after", type=int, help="verification-only hard process exit after N commits")
    parser.add_argument("--no-report", action="store_true")
    parser.add_argument("--json", action="store_true")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="imq_workbench", description="Deterministic image compression quality workbench")
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--output-dir", default=str(DEFAULT_SAMPLES_DIR))
    generate.add_argument("--preset", choices=("full", "quick"), default="full")
    generate.add_argument("--seed", type=int, default=0x5EED2026)
    generate.add_argument("--json", action="store_true")
    generate.set_defaults(func=command_generate)

    run = subparsers.add_parser("run", help="run, resume, or recompute a compression snapshot")
    _add_run_arguments(run)
    run.set_defaults(func=command_run)

    verify = subparsers.add_parser("verify", help="run the complete local acceptance matrix")
    verify.add_argument("--samples-dir", default=str(DEFAULT_QUICK_SAMPLES_DIR))
    verify.add_argument("--sample", default="quick_mixed_rgb")
    verify.add_argument("--block-size", type=int, default=16)
    verify.add_argument("--quant-step", type=float, default=8.0)
    verify.add_argument("--eval-window", type=int, default=7)
    verify.add_argument("--workers", type=int, default=4)
    verify.add_argument("--max-buffer-pixels", type=int, default=8_000_000)
    verify.add_argument("--pause-after", type=int, default=24)
    verify.add_argument("--checkpoint", default="artifacts/state/verify.sqlite")
    verify.add_argument("--monotonic-checkpoint", default="artifacts/state/monotonic.sqlite")
    verify.add_argument("--runs-dir", default=str(DEFAULT_RUNS_DIR))
    verify.add_argument("--output")
    verify.set_defaults(func=command_verify)

    check = subparsers.add_parser("check", help="check local runtime and workspace readiness")
    check.add_argument("--samples-dir", default=str(DEFAULT_SAMPLES_DIR))
    check.add_argument("--checkpoint", default=str(DEFAULT_CHECKPOINT))
    check.add_argument("--runs-dir", default=str(DEFAULT_RUNS_DIR))
    check.set_defaults(func=command_check)

    app = subparsers.add_parser("app", help="launch the PySide6 desktop workbench")
    app.add_argument("--samples-dir", default=str(DEFAULT_SAMPLES_DIR))
    app.add_argument("--checkpoint", default=str(DEFAULT_CHECKPOINT))
    app.add_argument("--runs-dir", default=str(DEFAULT_RUNS_DIR))
    app.set_defaults(func=command_app)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        _print_json(exc.to_dict())
        return 2
    except KeyboardInterrupt:
        _print_json({"status": "cancelled", "code": "E_INTERRUPT", "message": "interrupted; committed blocks remain resumable"})
        return 130
