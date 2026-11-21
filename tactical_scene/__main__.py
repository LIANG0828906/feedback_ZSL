"""Command-line entry points for deterministic samples and benchmarks."""

from __future__ import annotations

import argparse
import json
import statistics
import time
from pathlib import Path
from typing import Sequence

from .camera import Camera
from .errors import TacticalError
from .generator import generate_sample_suite, verify_sample_suite
from .query import BudgetConfig, QueryEngine
from .trace import load_trace
from .versioning import load_published_scene


def _parse_sizes(value: str) -> tuple[int, ...]:
    sizes = tuple(int(part.strip()) for part in value.split(",") if part.strip())
    if not sizes:
        raise argparse.ArgumentTypeError("at least one sample size is required")
    return sizes


def _generate(args: argparse.Namespace) -> int:
    suite = generate_sample_suite(
        args.output,
        seed=args.seed,
        sizes=args.sizes,
        clean=not args.keep_existing,
    )
    print(json.dumps(suite, sort_keys=True, indent=2))
    return 0


def _verify(args: argparse.Namespace) -> int:
    result = verify_sample_suite(args.suite)
    print(json.dumps(result, sort_keys=True, indent=2))
    return 0


def _benchmark(args: argparse.Namespace) -> int:
    suite_path = Path(args.suite)
    suite = json.loads((suite_path / "suite.json").read_text(encoding="utf-8"))
    case = max(suite["cases"], key=lambda item: item["unit_count"])
    case_dir = suite_path / case["directory"]
    scene = load_published_scene(
        case_dir / "scene.bfscene",
        case_dir / "manifest.json",
        expected_version=case["version"],
        expected_hash=case["content_hash"],
    )
    camera = Camera.from_dict(json.loads((case_dir / "camera.json").read_text(encoding="utf-8")))
    budgets = [float(value) for value in args.budgets.split(",") if value.strip()]
    timings: dict[str, list[float]] = {}
    summaries: list[dict[str, object]] = []
    for budget_ms in budgets:
        config = BudgetConfig(
            budget_ms=budget_ms,
            memory_budget_bytes=args.memory_bytes,
            bucket_grids=(64, 32, 16, 8, 1),
        )
        engine = QueryEngine(scene, camera, budget_config=config)
        samples: list[float] = []
        last = None
        try:
            for repeat in range(args.repeats):
                if repeat == 0:
                    started = time.perf_counter()
                    last = engine.select_rect(100, 140, 1320, 720)
                    elapsed = (time.perf_counter() - started) * 1000.0
                else:
                    started_ns = time.perf_counter_ns()
                    last = engine.select_rect(100, 140, 1320, 720)
                    elapsed = (time.perf_counter_ns() - started_ns) / 1_000_000.0
                samples.append(elapsed)
        except TacticalError as exc:
            summaries.append(
                {
                    "budget_ms": budget_ms,
                    "error": exc.to_dict()["error"],
                }
            )
            timings[str(budget_ms)] = samples
            continue
        assert last is not None
        timings[str(budget_ms)] = samples
        summaries.append(
            {
                "budget_ms": budget_ms,
                "strategy": last.strategy.value,
                "bucket_grid": [last.bucket_columns, last.bucket_rows],
                "selected": len(last.selected),
                "candidates": dict(sorted(last.stage_counts.items())),
                "reported_elapsed_ms": last.total_elapsed_ms,
                "degraded_stages": list(last.degraded_stages),
                "coverage_error_upper_bound": last.coverage_error_upper_bound,
                "wall_ms_median": statistics.median(samples),
                "wall_ms_p95": sorted(samples)[max(0, int(len(samples) * 0.95) - 1)],
                "within_budget": last.within_budget,
                "memory_bytes": last.memory_bytes,
            }
        )
    payload = {
        "case": case["name"],
        "unit_count": case["unit_count"],
        "repeats": args.repeats,
        "summaries": summaries,
        "timings_ms": timings,
    }
    print(json.dumps(payload, sort_keys=True, indent=2))
    return 0


def _trace_info(args: argparse.Namespace) -> int:
    records = load_trace(args.trace)
    print(
        json.dumps(
            {
                "records": len(records),
                "final_hash": records[-1]["record_hash"] if records else None,
                "versions": [record["event"]["scene_version"] for record in records],
            },
            sort_keys=True,
            indent=2,
        )
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m tactical_scene")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--output", type=Path, default=Path("artifacts/samples"))
    generate.add_argument("--seed", type=int, default=20261122)
    generate.add_argument("--sizes", type=_parse_sizes, default=(64, 2048, 20000))
    generate.add_argument("--keep-existing", action="store_true")
    generate.set_defaults(handler=_generate)

    verify = subparsers.add_parser("verify", help="verify sample baselines and traces")
    verify.add_argument("--suite", type=Path, default=Path("artifacts/samples"))
    verify.set_defaults(handler=_verify)

    benchmark = subparsers.add_parser("benchmark", help="benchmark the largest sample")
    benchmark.add_argument("--suite", type=Path, default=Path("artifacts/samples"))
    benchmark.add_argument("--budgets", default="5,20,80,500")
    benchmark.add_argument("--repeats", type=int, default=5)
    benchmark.add_argument("--memory-bytes", type=int, default=128 * 1024 * 1024)
    benchmark.set_defaults(handler=_benchmark)

    trace_info = subparsers.add_parser("trace-info", help="validate a trace hash chain")
    trace_info.add_argument("trace", type=Path)
    trace_info.set_defaults(handler=_trace_info)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except TacticalError as exc:
        print(json.dumps(exc.to_dict(), sort_keys=True, indent=2))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
