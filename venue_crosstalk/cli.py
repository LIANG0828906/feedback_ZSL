"""Command-line entry points for generation, analysis, and diagnostics."""

from __future__ import annotations

import argparse
import shutil
import sys
from pathlib import Path
from typing import Any

from .config import load_config, save_config
from .errors import E_BUSY, E_CANCELLED, VenueError
from .pipeline import AnalysisPipeline
from .samplegen import generate_sample_workspace
from .utils import atomic_write_json, canonical_json


def _parse_band_arguments(values: list[str] | None) -> list[tuple[str, float, float]] | None:
    if not values:
        return None
    parsed: list[tuple[str, float, float]] = []
    for value in values:
        parts = value.split(":")
        if len(parts) != 3:
            raise ValueError(f"band must be NAME:LOW_HZ:HIGH_HZ, got {value!r}")
        parsed.append((parts[0], float(parts[1]), float(parts[2])))
    return parsed


def _result_summary(result: dict[str, Any]) -> dict[str, Any]:
    return {
        "run_id": result.get("run_id"),
        "status": result.get("status"),
        "effective_config": result.get("effective_config"),
        "incremental": result.get("incremental"),
        "summary": result.get("summary"),
        "errors": result.get("errors"),
        "estimation_changes": result.get("estimation_changes"),
        "trace": result.get("trace"),
    }


def _print_result(result: dict[str, Any], as_json: bool) -> None:
    if as_json:
        print(canonical_json(_result_summary(result)), end="")
        return
    summary = result.get("summary", {})
    incremental = result.get("incremental", {})
    print(f"run: {result.get('run_id')}")
    print(f"status: {result.get('status')}")
    print(
        f"bands={summary.get('band_count')} pairs={summary.get('pair_count')} "
        f"cache_hits={incremental.get('cache_hits')} "
        f"cache_misses={incremental.get('cache_misses')}"
    )
    print(
        "inverted_pairs="
        + ",".join(summary.get("inverted_pairs", []))
    )
    print(
        "unknown_polarity_pairs="
        + ",".join(summary.get("unknown_polarity_pairs", []))
    )
    print("shortfall_bands=" + ",".join(summary.get("shortfall_bands", [])))
    print(f"errors={len(result.get('errors', []))}")
    if result.get("estimation_changes"):
        print(f"estimation_changes={len(result['estimation_changes'])}")
    print(f"trace={result.get('trace', {}).get('path')}")
    print(
        "trace_chain_hash="
        + str(result.get("trace", {}).get("final_chain_hash", ""))
    )


def command_generate(args: argparse.Namespace) -> int:
    truth = generate_sample_workspace(
        args.output,
        seed=args.seed,
        duration_s=args.duration,
        overwrite=args.overwrite,
    )
    payload = {
        "ok": True,
        "output": str(Path(args.output).resolve()),
        "seed": args.seed,
        "pairs": len(truth["pairs"]),
        "frame_count": truth["frame_count"],
        "layouts": sorted({item["layout"] for item in truth["pairs"]}),
    }
    print(canonical_json(payload), end="")
    return 0


def command_analyze(args: argparse.Namespace) -> int:
    bands = _parse_band_arguments(args.band)
    pipeline = AnalysisPipeline(
        args.workspace,
        reset_trace=args.reset_trace,
    )
    result = pipeline.run(
        operation=args.operation,
        target_suppression_db=args.target_db,
        bands=bands,
        strict=args.strict,
        reset_trace=args.reset_trace,
    )
    if args.persist:
        from .models import load_workspace

        workspace = load_workspace(args.workspace)
        config_path = workspace.root / str(
            workspace.manifest.get("config_file", "crossover.json")
        )
        effective = load_config(config_path)
        if bands is not None:
            effective = effective.with_bands(bands)
        if args.target_db is not None:
            effective = effective.with_target_suppression(args.target_db)
        save_config(config_path, effective)
    if args.json_out:
        atomic_write_json(args.json_out, result)
    _print_result(result, args.json)
    return 0


def command_show(args: argparse.Namespace) -> int:
    pipeline = AnalysisPipeline(args.workspace)
    result = pipeline.latest()
    if result is None:
        error = VenueError(
            "E_NOT_PUBLISHED",
            "workspace has no published snapshot",
            {"workspace": str(Path(args.workspace).resolve())},
        )
        if args.json:
            print(canonical_json({"ok": False, **error.to_dict()}), end="")
        else:
            print(str(error), file=sys.stderr)
        return 2
    _print_result(result, args.json)
    return 0


def command_verify(args: argparse.Namespace) -> int:
    from .verification import verify_published_workspace

    report = verify_published_workspace(
        args.workspace,
        max_relative_magnitude_error=args.max_magnitude_error,
        max_phase_error_deg=args.max_phase_error_deg,
        max_alignment_residual_samples=args.max_alignment_residual,
        output=args.json_out,
    )
    if args.json:
        print(canonical_json(report), end="")
    else:
        print(f"workspace: {report['workspace']}")
        print(f"snapshot: {report['snapshot_run_id']}")
        print(f"trace chain: {report['trace_chain_hash']}")
        for key, value in report["metrics"].items():
            print(f"{key}: {value}")
        failed = [item for item in report["checks"] if not item["ok"]]
        print(f"checks: {len(report['checks']) - len(failed)}/{len(report['checks'])} passed")
        for item in failed:
            print(f"FAILED {item['name']}: {item['detail']}")
    return 0 if report["ok"] else 1


def _reset_runtime_dirs(root: Path) -> None:
    for name in ("cache", "publish", "logs"):
        path = root / name
        if path.exists():
            shutil.rmtree(path)


def _changed_band_plan(
    result: dict[str, Any],
    *,
    band_name: str = "low",
    new_high_hz: float | None = None,
) -> list[tuple[str, float, float]]:
    bands = result["effective_config"]["bands"]
    target_index = next(
        (index for index, band in enumerate(bands) if band["name"] == band_name),
        None,
    )
    plan: list[tuple[str, float, float]] = []
    changed_high: float | None = None
    for index, band in enumerate(bands):
        low = float(band["low_hz"])
        high = float(band["high_hz"])
        if index == target_index:
            high = (
                float(new_high_hz)
                if new_high_hz is not None
                else max(low + 40.0, high - 30.0)
            )
            changed_high = high
        elif (
            target_index is not None
            and index == target_index + 1
            and changed_high is not None
        ):
            low = changed_high
        plan.append((str(band["name"]), low, high))
    return plan


def command_diagnose(args: argparse.Namespace) -> int:
    root = Path(args.workspace).resolve()
    if args.regenerate or not (root / "manifest.json").exists():
        generate_sample_workspace(
            root,
            seed=args.seed,
            duration_s=args.duration,
            overwrite=True,
        )
    if args.fresh:
        _reset_runtime_dirs(root)
    pipeline = AnalysisPipeline(root, reset_trace=True)
    baseline = pipeline.run(
        operation="diagnose-baseline",
        reset_trace=True,
    )
    target = max(0.0, float(baseline["effective_config"]["target_suppression_db"]) - 2.0)
    target_change = pipeline.run(
        operation="diagnose-target-change",
        target_suppression_db=target,
    )
    band_plan = _changed_band_plan(baseline)
    band_change = pipeline.run(
        operation="diagnose-band-change",
        target_suppression_db=target,
        bands=band_plan,
    )
    report = {
        "ok": True,
        "workspace": str(root),
        "seed": args.seed,
        "sequence": [
            "diagnose-baseline",
            "diagnose-target-change",
            "diagnose-band-change",
        ],
        "baseline": _result_summary(baseline),
        "target_change": _result_summary(target_change),
        "band_change": _result_summary(band_change),
        "final_band_plan": [
            {"name": name, "low_hz": low, "high_hz": high}
            for name, low, high in band_plan
        ],
        "final_publish": str(root / "publish" / "snapshot.json"),
        "trace": str(root / "logs" / "trace.jsonl"),
        "trace_final_chain_hash": band_change.get("trace", {}).get(
            "final_chain_hash"
        ),
    }
    if args.json_out:
        atomic_write_json(args.json_out, report)
    if args.json:
        print(canonical_json(report), end="")
    else:
        print(f"workspace: {root}")
        print(
            f"baseline: run={baseline['run_id'][:12]} status={baseline['status']} "
            f"bands={baseline['summary']['band_count']} "
            f"pairs={baseline['summary']['pair_count']}"
        )
        target_inc = target_change["incremental"]
        print(
            f"target change -> {target:.2f} dB: changed_bands="
            f"{target_inc['changed_bands']} cache_hits={target_inc['cache_hits']} "
            f"cache_misses={target_inc['cache_misses']}"
        )
        band_inc = band_change["incremental"]
        print(
            f"band boundary change: changed_bands={band_inc['changed_bands']} "
            f"cache_hits={band_inc['cache_hits']} "
            f"cache_misses={band_inc['cache_misses']} "
            f"estimation_changes={len(band_change['estimation_changes'])}"
        )
        print(f"final status: {band_change['status']}")
        print(f"errors: {len(band_change['errors'])}")
        print(f"publish: {report['final_publish']}")
        print(f"trace: {report['trace']}")
        print(f"trace chain hash: {report['trace_final_chain_hash']}")
    return 0


def command_gui(args: argparse.Namespace) -> int:
    try:
        from .gui import main as gui_main
    except ImportError as exc:
        raise VenueError(
            "E_GUI_UNAVAILABLE",
            f"PyQt5 is required for the GUI: {exc}",
        ) from exc
    return int(gui_main([] if args.workspace is None else [args.workspace]))


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="venue-crosstalk",
        description="Deterministic venue crosstalk analysis workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic sample data")
    generate.add_argument("output")
    generate.add_argument("--seed", type=int, default=20_261_122)
    generate.add_argument("--duration", type=float, default=0.5)
    generate.add_argument("--overwrite", action="store_true")
    generate.set_defaults(handler=command_generate)

    analyze = subparsers.add_parser("analyze", help="analyze or incrementally recompute")
    analyze.add_argument("workspace")
    analyze.add_argument("--target-db", type=float)
    analyze.add_argument(
        "--band",
        action="append",
        metavar="NAME:LOW:HIGH",
        help="replace the band plan; repeat for each band",
    )
    analyze.add_argument("--operation", default="analyze")
    analyze.add_argument("--strict", action="store_true")
    analyze.add_argument("--persist", action="store_true")
    analyze.add_argument("--reset-trace", action="store_true")
    analyze.add_argument("--json", action="store_true")
    analyze.add_argument("--json-out")
    analyze.set_defaults(handler=command_analyze)

    show = subparsers.add_parser("show", help="show the latest published result")
    show.add_argument("workspace")
    show.add_argument("--json", action="store_true")
    show.set_defaults(handler=command_show)

    verify = subparsers.add_parser(
        "verify",
        help="verify the published result against generator ground truth",
    )
    verify.add_argument("workspace")
    verify.add_argument("--max-magnitude-error", type=float, default=0.16)
    verify.add_argument("--max-phase-error-deg", type=float, default=12.0)
    verify.add_argument("--max-alignment-residual", type=int, default=2)
    verify.add_argument("--json", action="store_true")
    verify.add_argument("--json-out")
    verify.set_defaults(handler=command_verify)

    diagnose = subparsers.add_parser(
        "diagnose",
        help="run baseline, target-change, and band-change trace sequence",
    )
    diagnose.add_argument("workspace")
    diagnose.add_argument("--seed", type=int, default=20_261_122)
    diagnose.add_argument("--duration", type=float, default=0.5)
    diagnose.add_argument("--fresh", action="store_true")
    diagnose.add_argument("--regenerate", action="store_true")
    diagnose.add_argument("--json", action="store_true")
    diagnose.add_argument("--json-out")
    diagnose.set_defaults(handler=command_diagnose)

    gui = subparsers.add_parser("gui", help="launch the PyQt5 workbench")
    gui.add_argument("workspace", nargs="?")
    gui.set_defaults(handler=command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except VenueError as exc:
        if getattr(args, "json", False):
            print(canonical_json({"ok": False, **exc.to_dict()}), end="")
        else:
            print(str(exc), file=sys.stderr)
        if exc.code == E_BUSY:
            return 4
        if exc.code == E_CANCELLED:
            return 5
        return 2
    except (ValueError, FileExistsError, OSError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
