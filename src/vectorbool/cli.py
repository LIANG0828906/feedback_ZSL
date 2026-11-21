from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .boolean import boolean_paths
from .errors import BusinessError, ErrorCode
from .generator import generate_samples, run_regression
from .injection import apply_injection, validate_entry
from .intersect import recompute_intersections
from .parser import parse_path, parse_segment
from .preview import render_preview
from .project import load_project, save_project


def _read_json(filename: str) -> Any:
    with Path(filename).open("r", encoding="utf-8") as stream:
        return json.load(stream)


def _emit(value: Any) -> None:
    sys.stdout.write(
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    )


def _add_injection_argument(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--inject",
        action="append",
        default=[],
        choices=[
            "coincident_edge",
            "degenerate_segment",
            "zero_area_contour",
            "hole_reversed",
            "non_finite_coordinate",
        ],
        help="inject a deterministic anomaly at entry parsing",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="vectorbool",
        description="Deterministic tolerant vector path boolean operations",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument(
        "--out", default="artifacts/samples/samples.json", help="output samples file"
    )

    regression = subparsers.add_parser(
        "regression", help="run sample regression and write baseline/log/report"
    )
    regression.add_argument("--samples", default="artifacts/samples/samples.json")
    regression.add_argument("--baseline", default="artifacts/samples/baseline.json")
    regression.add_argument("--trace", default="artifacts/samples/trace_log.json")
    regression.add_argument("--report", default="artifacts/samples/regression_report.json")
    regression.add_argument("--check-existing", action="store_true")

    run = subparsers.add_parser("run", help="run one boolean operation")
    run.add_argument("--a", required=True, help="path A JSON file")
    run.add_argument("--b", required=True, help="path B JSON file")
    run.add_argument(
        "--operation",
        required=True,
        choices=["union", "intersect", "difference", "xor"],
    )
    run.add_argument("--tolerance", type=float, default=1.0e-6)
    run.add_argument("--preview", help="write deterministic preview PNG")
    run.add_argument("--project", help="save a versioned project file")
    _add_injection_argument(run)

    project = subparsers.add_parser("project-show", help="validate and print a project")
    project.add_argument("--project", required=True)

    intersections = subparsers.add_parser(
        "intersections", help="recompute segment intersections across tolerances"
    )
    intersections.add_argument("--a", required=True, help="segment A JSON")
    intersections.add_argument("--b", required=True, help="segment B JSON")
    intersections.add_argument("--old-tolerance", type=float, default=1.0e-6)
    intersections.add_argument("--new-tolerance", type=float, default=1.0e-4)

    ui = subparsers.add_parser("ui", help="launch the PySide6 editor")
    ui.add_argument("--samples", default="artifacts/samples/samples.json")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    arguments = parser.parse_args(argv)
    try:
        if arguments.command == "generate":
            _emit(generate_samples(arguments.out))
            return 0
        if arguments.command == "regression":
            report = run_regression(
                arguments.samples,
                baseline_file=arguments.baseline,
                trace_file=arguments.trace,
                report_file=arguments.report,
                check_existing=arguments.check_existing,
            )
            _emit(report)
            return 0 if not report["failures"] else 1
        if arguments.command == "project-show":
            loaded = load_project(arguments.project)
            _emit(
                {
                    "operation": loaded.operation,
                    "tolerance": loaded.tolerance,
                    "committed": loaded.committed,
                    "content_hash": loaded.content_hash,
                    "has_result": loaded.result is not None,
                    "report": loaded.report,
                }
            )
            return 0
        if arguments.command == "intersections":
            segment_a = parse_segment(
                _read_json(arguments.a),
                tolerance=arguments.old_tolerance,
                path_name=Path(arguments.a).name,
            )
            segment_b = parse_segment(
                _read_json(arguments.b),
                tolerance=arguments.old_tolerance,
                path_name=Path(arguments.b).name,
            )
            hits, difference = recompute_intersections(
                segment_a,
                segment_b,
                old_tolerance=arguments.old_tolerance,
                new_tolerance=arguments.new_tolerance,
            )
            _emit(
                {
                    "intersections": [hit.to_dict() for hit in hits],
                    "difference": difference.to_dict(),
                }
            )
            return 0
        if arguments.command == "ui":
            from .ui import launch_ui

            return launch_ui(arguments.samples)
        return _run_command(arguments)
    except BusinessError as error:
        _emit(error.to_dict())
        return 2


def _run_command(arguments: argparse.Namespace) -> int:
    raw_a = _read_json(arguments.a)
    raw_b = _read_json(arguments.b)
    metadata: list[dict[str, Any]] = []
    if arguments.inject:
        raw_a, raw_b, location = apply_injection(
            raw_a,
            raw_b,
            arguments.inject[0],
            tolerance=arguments.tolerance,
        )
        metadata = [location.to_dict()]
    validate_entry(
        raw_a,
        raw_b,
        tolerance=arguments.tolerance,
        injections=arguments.inject,
        injection_metadata=metadata,
    )
    path_a = parse_path(
        raw_a, tolerance=arguments.tolerance, path_name=Path(arguments.a).name
    )
    path_b = parse_path(
        raw_b, tolerance=arguments.tolerance, path_name=Path(arguments.b).name
    )
    result = boolean_paths(
        path_a,
        path_b,
        arguments.operation,
        tolerance=arguments.tolerance,
    )
    preview_sha = None
    if arguments.preview:
        if not result.preview_allowed:
            raise BusinessError(
                ErrorCode.RESULT_DEGRADED,
                "degraded result is retained in the report but not rendered",
            )
        preview = render_preview(
            path_a,
            path_b,
            result,
            tolerance=arguments.tolerance,
        )
        preview.write_png(arguments.preview)
        preview_sha = preview.png_sha256
    if arguments.project:
        save_project(
            arguments.project,
            path_a=path_a,
            path_b=path_b,
            operation=arguments.operation,
            tolerance=arguments.tolerance,
            result=result,
        )
    payload = result.to_dict()
    payload["preview_sha256"] = preview_sha
    _emit(payload)
    return 0 if result.preview_allowed else 3


if __name__ == "__main__":
    raise SystemExit(main())

