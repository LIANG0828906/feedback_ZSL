from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path
from typing import Any, Sequence

from .errors import BusinessError, ErrorCode
from .generator import atomic_write
from .models import canonical_json
from .pipeline import Pipeline, load_artifacts_from_output
from .playback import (
    PlaybackConfig,
    load_bandwidth,
    simulate_playback,
)
from .samples import generate_samples


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def _parse_fault(value: str | None) -> tuple[str, int] | None:
    if value is None:
        return None
    try:
        tier, index_text = value.rsplit(":", 1)
        return tier, int(index_text)
    except ValueError as exc:
        raise BusinessError(
            ErrorCode.INVALID_CONFIG,
            "--inject-failure must be TIER:INDEX",
            {"value": value},
        ) from exc


def _write_lines(path: Path, documents: list[dict[str, Any]]) -> None:
    data = b"\n".join(canonical_json(item) for item in documents)
    if data:
        data += b"\n"
    atomic_write(path, data)


def _run_pipeline(args: argparse.Namespace) -> tuple[Pipeline, Any]:
    pipeline = Pipeline(
        config_path=Path(args.config),
        budget_path=Path(args.budgets),
        output_root=Path(args.output),
    )
    result = pipeline.run(
        restore_snapshot=Path(args.restore) if args.restore else None,
        fault_at=_parse_fault(args.inject_failure),
    )
    return pipeline, result


def cmd_generate(args: argparse.Namespace) -> int:
    paths = generate_samples(
        Path(args.samples),
        seed=args.seed,
        duration_ms=args.duration_ms,
        overwrite=not args.keep,
    )
    _print_json({name: str(path) for name, path in paths.items()})
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    _, result = _run_pipeline(args)
    _print_json(result.to_dict())
    return 0 if result.status == "complete" else 3


def _simulate_from_args(
    args: argparse.Namespace, output: Path | None = None
) -> tuple[Any, Any, Any]:
    pipeline = Pipeline(
        config_path=Path(args.config),
        budget_path=Path(args.budgets),
        output_root=Path(args.output),
    )
    artifacts = load_artifacts_from_output(Path(args.output), pipeline.project)
    bandwidth = load_bandwidth(Path(args.bandwidth))
    playback = PlaybackConfig.from_dict(
        json.loads(Path(args.playback).read_text(encoding="utf-8"))
    )
    result = simulate_playback(
        tiers=pipeline.project.tiers,
        slices=artifacts,
        bandwidth=bandwidth,
        playback=playback,
        budget=pipeline.project.budget,
    )
    return pipeline, bandwidth, result


def cmd_simulate(args: argparse.Namespace) -> int:
    pipeline, bandwidth, result = _simulate_from_args(args)
    output = Path(args.output)
    trace_path = output / "playback_trace.jsonl"
    _write_lines(trace_path, result.events)
    summary = {
        "errors": result.errors,
        "final_tier": result.final_tier,
        "max_buffer_bytes": result.max_buffer_bytes,
        "min_buffer_bytes": result.min_buffer_bytes,
        "switch_count": result.switch_count,
        "trace": str(trace_path),
    }
    atomic_write(output / "playback_summary.json", canonical_json(summary) + b"\n")
    _print_json(summary)
    return 0 if not result.errors else 3


def _demo(args: argparse.Namespace) -> int:
    sample_root = Path(args.samples)
    output = Path(args.output)
    paths = generate_samples(
        sample_root,
        seed=args.seed,
        duration_ms=args.duration_ms,
        overwrite=True,
    )
    if output.exists():
        shutil.rmtree(output)
    pipeline = Pipeline(
        config_path=paths["track_config"],
        budget_path=paths["budgets"],
        output_root=output,
    )
    run_result = pipeline.run()
    artifacts = load_artifacts_from_output(output, pipeline.project)
    playback = PlaybackConfig.from_dict(
        json.loads(paths["playback"].read_text(encoding="utf-8"))
    )
    playback_result = simulate_playback(
        tiers=pipeline.project.tiers,
        slices=artifacts,
        bandwidth=load_bandwidth(paths["bandwidth"]),
        playback=playback,
        budget=pipeline.project.budget,
    )
    scheduler_documents = [
        json.loads(line)
        for line in run_result.trace_path.read_text(encoding="utf-8").splitlines()
        if line
    ]
    playback_documents = [
        {"source": "playback", **event} for event in playback_result.events
    ]
    demo_trace = output / "demo_trace.jsonl"
    _write_lines(demo_trace, scheduler_documents + playback_documents)
    summary = {
        "errors": run_result.errors,
        "memory_peak_bytes": run_result.scheduler.peak_bytes,
        "playback": {
            "errors": playback_result.errors,
            "final_tier": playback_result.final_tier,
            "switch_count": playback_result.switch_count,
        },
        "snapshot": str(run_result.snapshot_path),
        "status": run_result.status,
        "tiers": {
            tier_id: {
                "completed": len(result_list),
                "hashes": [item.content_hash for item in result_list],
            }
            for tier_id, result_list in artifacts.items()
        },
        "trace": str(demo_trace),
    }
    atomic_write(output / "demo_summary.json", canonical_json(summary) + b"\n")
    _print_json(summary)
    return 0 if run_result.status == "complete" and not playback_result.errors else 3


def cmd_demo(args: argparse.Namespace) -> int:
    return _demo(args)


def cmd_gui(args: argparse.Namespace) -> int:
    from .gui import launch_gui

    return launch_gui(
        samples_root=Path(args.samples),
        output_root=Path(args.output),
        seed=args.seed,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="streamforge",
        description="Deterministic local multi-bitrate slicing service",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--samples", default="samples")
    generate.add_argument("--seed", type=int, default=20261121)
    generate.add_argument("--duration-ms", type=int, default=3200)
    generate.add_argument("--keep", action="store_true")
    generate.set_defaults(func=cmd_generate)

    run = subparsers.add_parser("run", help="run slicing pipeline")
    run.add_argument("--config", default="samples/track_config.json")
    run.add_argument("--budgets", default="samples/budgets.json")
    run.add_argument("--output", default="artifacts/run")
    run.add_argument("--restore")
    run.add_argument("--inject-failure")
    run.set_defaults(func=cmd_run)

    simulate = subparsers.add_parser("simulate", help="run adaptive playback simulator")
    simulate.add_argument("--config", default="samples/track_config.json")
    simulate.add_argument("--budgets", default="samples/budgets.json")
    simulate.add_argument("--output", default="artifacts/run")
    simulate.add_argument("--bandwidth", default="samples/bandwidth.json")
    simulate.add_argument("--playback", default="samples/playback.json")
    simulate.set_defaults(func=cmd_simulate)

    demo = subparsers.add_parser("demo", help="run the complete deterministic demo")
    demo.add_argument("--samples", default="samples")
    demo.add_argument("--output", default="artifacts/demo")
    demo.add_argument("--seed", type=int, default=20261121)
    demo.add_argument("--duration-ms", type=int, default=3200)
    demo.set_defaults(func=cmd_demo)

    gui = subparsers.add_parser("gui", help="launch the PySide6 desktop interface")
    gui.add_argument("--samples", default="samples")
    gui.add_argument("--output", default="artifacts/gui")
    gui.add_argument("--seed", type=int, default=20261121)
    gui.set_defaults(func=cmd_gui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BusinessError as exc:
        _print_json(exc.to_dict())
        return 2
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())
