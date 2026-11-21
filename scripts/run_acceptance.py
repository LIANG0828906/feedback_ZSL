#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import platform
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

import numpy
import PySide6

from vectorbool.boolean import boolean_paths
from vectorbool.errors import BusinessError
from vectorbool.generator import load_samples
from vectorbool.injection import apply_injection, validate_entry
from vectorbool.parser import parse_path
from vectorbool.preview import render_preview
from vectorbool.project import canonical_json, load_project, save_project

TOLERANCE = 1.0e-6
OPERATIONS = ("union", "intersect", "difference", "xor")


def _content_hash(result) -> str:
    payload = {
        "operation": result.operation.value,
        "tolerance": result.tolerance,
        "intersection_count": result.intersection_count,
        "contours": [contour.to_dict() for contour in result.contours],
    }
    return hashlib.sha256(canonical_json(payload)).hexdigest()


def main() -> int:
    samples_path = ROOT / "artifacts/samples/samples.json"
    samples = load_samples(samples_path)
    case = next(item for item in samples["catalog"] if item["name"] == "curved_partial_q")
    path_a = parse_path(case["path_a"], tolerance=TOLERANCE, path_name="curved.a")
    path_b = parse_path(case["path_b"], tolerance=TOLERANCE, path_name="curved.b")

    operations = []
    results = {}
    for operation in OPERATIONS:
        result = boolean_paths(path_a, path_b, operation, tolerance=TOLERANCE)
        results[operation] = result
        operations.append(
            {
                "operation": operation,
                "status": result.report.status,
                "preview_allowed": result.preview_allowed,
                "intersections": result.intersection_count,
                "contours": len(result.contours),
                "area_relative_deviation": result.report.area_relative_deviation,
                "max_boundary_deviation": result.report.max_boundary_deviation,
                "boundary_deviation_upper_bound": result.report.boundary_deviation_upper_bound,
                "tolerance_amplification": result.report.tolerance_amplification,
                "content_hash": _content_hash(result),
            }
        )

    injections = []
    base_a = next(item for item in samples["catalog"] if item["name"] == "rect_partial_s1")[
        "path_a"
    ]
    base_b = next(item for item in samples["catalog"] if item["name"] == "rect_partial_s1")[
        "path_b"
    ]
    for kind in (
        "coincident_edge",
        "degenerate_segment",
        "zero_area_contour",
        "hole_reversed",
        "non_finite_coordinate",
    ):
        raw_a, raw_b, location = apply_injection(
            base_a, base_b, kind, tolerance=TOLERANCE
        )
        try:
            validate_entry(
                raw_a,
                raw_b,
                tolerance=TOLERANCE,
                injections=[kind],
                injection_metadata=[location.to_dict()],
            )
        except BusinessError as error:
            injections.append(
                {
                    "kind": kind,
                    "code": error.code.value,
                    "location": error.location.to_dict(),
                }
            )
        else:
            injections.append({"kind": kind, "code": "NO_ERROR", "location": {}})

    preview = render_preview(
        path_a,
        path_b,
        results["union"],
        tolerance=TOLERANCE,
    )
    second_preview = render_preview(
        path_a,
        path_b,
        results["union"],
        tolerance=TOLERANCE,
    )
    preview_path = ROOT / "artifacts/samples/curved_union_preview.png"
    preview.write_png(preview_path)

    project_path = ROOT / "artifacts/samples/acceptance_project.vbproj.json"
    digest = save_project(
        project_path,
        path_a=path_a,
        path_b=path_b,
        operation="union",
        tolerance=TOLERANCE,
        result=results["union"],
    )
    restored = load_project(project_path)

    sample_files = {}
    for relative in (
        "artifacts/samples/samples.json",
        "artifacts/samples/baseline.json",
        "artifacts/samples/trace_log.json",
        "artifacts/samples/regression_report.json",
    ):
        path = ROOT / relative
        sample_files[relative] = hashlib.sha256(path.read_bytes()).hexdigest()

    report = {
        "environment": {
            "python": sys.version.split()[0],
            "numpy": numpy.__version__,
            "pyside6": PySide6.__version__,
            "platform": platform.system(),
            "platform_release": platform.release(),
        },
        "commands": [
            ".venv/bin/pytest -q",
            ".venv/bin/vectorbool generate --out artifacts/samples/samples.json",
            ".venv/bin/vectorbool regression --check-existing",
            ".venv/bin/python scripts/run_acceptance.py",
        ],
        "sample_case": case["name"],
        "tolerance": TOLERANCE,
        "operations": operations,
        "injections": injections,
        "preview": {
            "path": str(preview_path.relative_to(ROOT)),
            "byte_identical": preview.png == second_preview.png
            and preview.rgba == second_preview.rgba,
            "png_sha256": preview.png_sha256,
            "rgba_sha256": preview.rgba_sha256,
        },
        "project": {
            "path": str(project_path.relative_to(ROOT)),
            "saved_hash": digest,
            "loaded_hash": restored.content_hash,
            "committed": restored.committed,
            "has_result": restored.result is not None,
            "roundtrip_ok": digest == restored.content_hash
            and restored.committed
            and restored.result is not None,
        },
        "files": sample_files,
        "regression": json.loads(
            (ROOT / "artifacts/samples/regression_report.json").read_text(
                encoding="utf-8"
            )
        ),
    }
    output = ROOT / "artifacts/acceptance_report.json"
    output.write_text(
        json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    print(
        json.dumps(
            {
                "operations_ok": all(item["status"] == "ok" for item in operations),
                "injection_codes": {item["kind"]: item["code"] for item in injections},
                "preview_byte_identical": report["preview"]["byte_identical"],
                "project_roundtrip_ok": report["project"]["roundtrip_ok"],
                "report": str(output.relative_to(ROOT)),
            },
            ensure_ascii=False,
            sort_keys=True,
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

