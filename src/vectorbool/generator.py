from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from .boolean import boolean_paths
from .errors import BusinessError, ErrorCode
from .injection import apply_injection, validate_entry
from .model import Operation
from .parser import parse_path
from .preview import render_preview
from .project import canonical_json

DEFAULT_SEED = 20261121
DEFAULT_TOLERANCES = (1.0e-6, 1.0e-4)
DEFAULT_OPERATIONS = tuple(operation.value for operation in Operation)


def _line_subpath(points: list[tuple[float, float]]) -> dict[str, Any]:
    return {
        "closed": True,
        "segments": [
            {
                "kind": "L",
                "points": [
                    [float(points[index][0]), float(points[index][1])],
                    [
                        float(points[(index + 1) % len(points)][0]),
                        float(points[(index + 1) % len(points)][1]),
                    ],
                ],
            }
            for index in range(len(points))
        ],
    }


def rectangle_path(
    x0: float, y0: float, x1: float, y1: float
) -> dict[str, Any]:
    return {
        "fill_rule": "nonzero",
        "subpaths": [_line_subpath([(x0, y0), (x1, y0), (x1, y1), (x0, y1)])],
    }


def rounded_path(x0: float, y0: float, x1: float, y1: float, radius: float) -> dict[str, Any]:
    radius = min(radius, (x1 - x0) * 0.5, (y1 - y0) * 0.5)
    k = radius * 0.5522847498307936
    segments = [
        {"kind": "L", "points": [[x0 + radius, y0], [x1 - radius, y0]]},
        {
            "kind": "C",
            "points": [
                [x1 - radius, y0],
                [x1 - radius + k, y0],
                [x1, y0 + radius - k],
                [x1, y0 + radius],
            ],
        },
        {"kind": "L", "points": [[x1, y0 + radius], [x1, y1 - radius]]},
        {
            "kind": "C",
            "points": [
                [x1, y1 - radius],
                [x1, y1 - radius + k],
                [x1 - radius + k, y1],
                [x1 - radius, y1],
            ],
        },
        {"kind": "L", "points": [[x1 - radius, y1], [x0 + radius, y1]]},
        {
            "kind": "C",
            "points": [
                [x0 + radius, y1],
                [x0 + radius - k, y1],
                [x0, y1 - radius + k],
                [x0, y1 - radius],
            ],
        },
        {"kind": "L", "points": [[x0, y1 - radius], [x0, y0 + radius]]},
        {
            "kind": "C",
            "points": [
                [x0, y0 + radius],
                [x0, y0 + radius - k],
                [x0 + radius - k, y0],
                [x0 + radius, y0],
            ],
        },
    ]
    return {"fill_rule": "nonzero", "subpaths": [{"closed": True, "segments": segments}]}


def curved_partial_path(scale: float) -> tuple[dict[str, Any], dict[str, Any]]:
    path_a = {
        "fill_rule": "nonzero",
        "subpaths": [
            {
                "closed": True,
                "segments": [
                    {
                        "kind": "Q",
                        "points": [
                            [0.0, 0.0],
                            [1.5 * scale, -0.75 * scale],
                            [3.0 * scale, 0.0],
                        ],
                    },
                    {"kind": "L", "points": [[3.0 * scale, 0.0], [3.0 * scale, 3.0 * scale]]},
                    {"kind": "L", "points": [[3.0 * scale, 3.0 * scale], [0.0, 3.0 * scale]]},
                    {"kind": "L", "points": [[0.0, 3.0 * scale], [0.0, 0.0]]},
                ],
            }
        ],
    }
    path_b = rectangle_path(
        1.5 * scale, 1.0 * scale, 4.5 * scale, 4.0 * scale
    )
    return path_a, path_b


def _hole_path() -> dict[str, Any]:
    outer = _line_subpath([(0, 0), (4, 0), (4, 4), (0, 4)])
    hole_points = [(1.0, 3.0), (3.0, 3.0), (3.0, 1.0), (1.0, 1.0)]
    return {
        "fill_rule": "nonzero",
        "subpaths": [outer, _line_subpath(hole_points)],
    }


def _self_cross_path() -> dict[str, Any]:
    return {
        "fill_rule": "evenodd",
        "subpaths": [
            _line_subpath([(0.0, 0.0), (2.0, 2.0), (2.0, 0.0), (0.0, 2.0)])
        ],
    }


def build_catalog() -> list[dict[str, Any]]:
    reducers = {
        "separated": (-1.0, 0.5, 1.0, 2.5),
        "partial": (1.0, 1.0, 3.0, 3.0),
        "contained": (0.5, 0.5, 1.5, 1.5),
        "near_coincident": (-1.0e-5, 1.0e-5, 2.0 + 1.0e-5, 2.0 + 1.0e-5),
    }
    catalog: list[dict[str, Any]] = []
    for scale in (1.0, 4.0, 16.0):
        for overlap, (x0, y0, x1, y1) in reducers.items():
            if overlap == "near_coincident":
                offset = 1.0e-5 * scale
                path_b = rectangle_path(
                    -offset,
                    offset,
                    2.0 * scale + offset,
                    2.0 * scale + offset,
                )
            else:
                path_b = rectangle_path(
                    x0 * scale,
                    y0 * scale,
                    x1 * scale,
                    y1 * scale,
                )
            catalog.append(
                {
                    "name": f"rect_{overlap}_s{int(scale)}",
                    "overlap": overlap,
                    "path_a": rectangle_path(0.0, 0.0, 2.0 * scale, 2.0 * scale),
                    "path_b": path_b,
                    "scale": scale,
                }
            )
    curved_a, curved_b = curved_partial_path(1.0)
    catalog.append(
        {
            "name": "curved_partial_q",
            "overlap": "partial",
            "path_a": curved_a,
            "path_b": curved_b,
            "scale": 1.0,
        }
    )
    rounded_a = rounded_path(0.0, 0.0, 4.0, 3.0, 0.75)
    rounded_b = rectangle_path(2.0, 1.0, 5.5, 4.0)
    catalog.append(
        {
            "name": "rounded_cubic_partial",
            "overlap": "partial",
            "path_a": rounded_a,
            "path_b": rounded_b,
            "scale": 1.0,
            "tolerances": [1.0e-4],
        }
    )
    catalog.append(
        {
            "name": "hole_vs_rect",
            "overlap": "partial",
            "path_a": _hole_path(),
            "path_b": rectangle_path(2.0, 2.0, 5.0, 5.0),
            "scale": 1.0,
        }
    )
    catalog.append(
        {
            "name": "self_cross_pair",
            "overlap": "partial",
            "path_a": _self_cross_path(),
            "path_b": rectangle_path(0.5, 0.5, 2.5, 2.5),
            "scale": 1.0,
        }
    )
    return catalog


def build_injections() -> list[dict[str, Any]]:
    base_a = rectangle_path(0.0, 0.0, 2.0, 2.0)
    base_b = rectangle_path(3.0, 0.0, 5.0, 2.0)
    result = []
    for kind in (
        "coincident_edge",
        "degenerate_segment",
        "zero_area_contour",
        "hole_reversed",
        "non_finite_coordinate",
    ):
        _, _, location = apply_injection(
            base_a, base_b, kind, tolerance=1.0e-6
        )
        result.append(
            {
                "name": f"inj_{kind}",
                "injections": [location.to_dict()],
                "path_a": base_a,
                "path_b": base_b,
            }
        )
    return result


def generate_samples(output: str | Path) -> dict[str, Any]:
    document = {
        "seed": DEFAULT_SEED,
        "generator": "vectorbool.generator/v1",
        "operations": list(DEFAULT_OPERATIONS),
        "tolerances": list(DEFAULT_TOLERANCES),
        "catalog": build_catalog(),
        "injections": build_injections(),
    }
    payload = _write_json(output, document)
    return {
        "path": str(Path(output)),
        "sha256": hashlib.sha256(payload).hexdigest(),
        "cases": len(document["catalog"]),
        "injections": len(document["injections"]),
    }


def load_samples(filename: str | Path) -> dict[str, Any]:
    with Path(filename).open("r", encoding="utf-8") as stream:
        document = json.load(stream)
    if not isinstance(document, dict) or not isinstance(document.get("catalog"), list):
        raise TypeError("samples file has no catalog")
    return document


def _write_json(filename: str | Path, value: Any) -> bytes:
    path = Path(filename)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = (
        json.dumps(
            value,
            ensure_ascii=False,
            sort_keys=True,
            indent=2,
            allow_nan=False,
        )
        + "\n"
    ).encode("utf-8")
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_bytes(payload)
    temporary.replace(path)
    return payload


def _result_hash(result) -> str:
    payload = {
        "operation": result.operation.value,
        "tolerance": result.tolerance,
        "intersection_count": result.intersection_count,
        "contours": [contour.to_dict() for contour in result.contours],
    }
    return hashlib.sha256(canonical_json(payload)).hexdigest()


def run_regression(
    samples_file: str | Path,
    *,
    baseline_file: str | Path,
    trace_file: str | Path,
    report_file: str | Path,
    check_existing: bool = False,
) -> dict[str, Any]:
    samples_path = Path(samples_file)
    samples = load_samples(samples_path)
    tolerances = [float(value) for value in samples.get("tolerances", DEFAULT_TOLERANCES)]
    operations = [
        str(value) for value in samples.get("operations", DEFAULT_OPERATIONS)
    ]
    existing: dict[str, str] = {}
    if check_existing and Path(baseline_file).exists():
        with Path(baseline_file).open("r", encoding="utf-8") as stream:
            existing = json.load(stream)

    baseline: dict[str, str] = {}
    trace: list[dict[str, Any]] = []
    failures: list[dict[str, str]] = []
    for case in samples["catalog"]:
        case_tolerances = [
            float(value) for value in case.get("tolerances", tolerances)
        ]
        for tolerance in case_tolerances:
            try:
                path_a = parse_path(
                    case["path_a"], tolerance=tolerance, path_name=f"{case['name']}.a"
                )
                path_b = parse_path(
                    case["path_b"], tolerance=tolerance, path_name=f"{case['name']}.b"
                )
            except BusinessError as error:
                for operation in operations:
                    key = _baseline_key(case["name"], operation, tolerance)
                    baseline[key] = f"error:{_error_code(error)}"
                continue
            for operation in operations:
                key = _baseline_key(case["name"], operation, tolerance)
                try:
                    result = boolean_paths(
                        path_a,
                        path_b,
                        operation,
                        tolerance=tolerance,
                        reject_self_intersections=False,
                    )
                    value = _result_hash(result)
                    baseline[key] = value
                    trace.append(
                        {
                            "case": case["name"],
                            "operation": operation,
                            "tolerance": tolerance,
                            "status": result.report.status,
                            "intersections": result.intersection_count,
                            "output_contours": len(result.contours),
                            "area_relative_deviation": result.report.area_relative_deviation,
                            "boundary_deviation_upper_bound": result.report.boundary_deviation_upper_bound,
                            "content_hash": value,
                        }
                    )
                except BusinessError as error:
                    code = _error_code(error)
                    baseline[key] = f"error:{code}"
                    trace.append(
                        {
                            "case": case["name"],
                            "operation": operation,
                            "tolerance": tolerance,
                            "status": "error",
                            "error_code": code,
                            "location": error.location.to_dict(),
                            "content_hash": f"error:{code}",
                        }
                    )

    injection_codes: dict[str, str] = {}
    for injection in samples.get("injections", []):
        metadata = injection.get("injections", [])
        kind = metadata[0]["kind"] if metadata else "unknown"
        code = ""
        try:
            raw_a, raw_b, _ = apply_injection(
                injection["path_a"],
                injection["path_b"],
                kind,
                tolerance=1.0e-6,
            )
            validate_entry(
                raw_a,
                raw_b,
                tolerance=1.0e-6,
                injections=metadata,
            )
            parse_path(raw_a, tolerance=1.0e-6, path_name="a")
            parse_path(raw_b, tolerance=1.0e-6, path_name="b")
            code = "NO_ERROR"
        except BusinessError as error:
            code = _error_code(error)
        injection_codes[injection["name"]] = code
        baseline[injection["name"]] = f"error:{code}"
        trace.append(
            {
                "case": injection["name"],
                "operation": "entry_validation",
                "tolerance": 1.0e-6,
                "status": "rejected",
                "error_code": code,
                "content_hash": f"error:{code}",
            }
        )

    preview_checks = _preview_consistency_checks(samples)
    trace.sort(
        key=lambda entry: (
            entry["case"],
            entry["operation"],
            float(entry["tolerance"]),
        )
    )
    baseline_payload = _write_json(baseline_file, baseline)
    trace_payload = _write_json(trace_file, trace)
    for key, expected in existing.items():
        actual = baseline.get(key)
        if actual != expected:
            failures.append(
                {
                    "key": key,
                    "expected": expected,
                    "actual": actual or "<missing>",
                }
            )
    for key, value in baseline.items():
        if check_existing and key not in existing:
            failures.append(
                {
                    "key": key,
                    "expected": "<missing>",
                    "actual": value,
                }
            )
    report = {
        "checked": len(baseline),
        "operations_checked": len(operations),
        "tolerances_checked": len(tolerances),
        "failures": failures,
        "injection_codes": injection_codes,
        "preview_byte_identical": preview_checks["byte_identical"],
        "preview_sha256": preview_checks["sha256"],
        "samples_sha256": hashlib.sha256(samples_path.read_bytes()).hexdigest(),
        "baseline_sha256": hashlib.sha256(baseline_payload).hexdigest(),
        "trace_sha256": hashlib.sha256(trace_payload).hexdigest(),
    }
    _write_json(report_file, report)
    return report


def _preview_consistency_checks(samples: dict[str, Any]) -> dict[str, Any]:
    case = next(
        (item for item in samples["catalog"] if item["name"] == "rounded_cubic_partial"),
        samples["catalog"][0],
    )
    path_a = parse_path(case["path_a"], tolerance=1.0e-4, path_name="preview.a")
    path_b = parse_path(case["path_b"], tolerance=1.0e-4, path_name="preview.b")
    result = boolean_paths(path_a, path_b, "union", tolerance=1.0e-4)
    first = render_preview(path_a, path_b, result, tolerance=1.0e-4)
    second = render_preview(path_a, path_b, result, tolerance=1.0e-4)
    return {
        "byte_identical": first.png == second.png and first.rgba == second.rgba,
        "sha256": first.png_sha256,
    }


def _baseline_key(case: str, operation: str, tolerance: float) -> str:
    return f"{case}|{operation}|{tolerance:.0e}"


def _error_code(error: BusinessError) -> str:
    return error.code.value if isinstance(error.code, ErrorCode) else str(error.code)

