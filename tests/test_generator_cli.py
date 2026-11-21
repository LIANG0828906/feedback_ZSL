from __future__ import annotations

import json
import subprocess
import sys

from vectorbool.generator import build_catalog, build_injections, generate_samples


def test_generator_catalog_contains_scale_overlap_and_curve_cases() -> None:
    catalog = build_catalog()
    names = {case["name"] for case in catalog}
    assert {"rect_partial_s1", "rect_partial_s16", "curved_partial_q", "hole_vs_rect"} <= names
    assert len(build_injections()) == 5


def test_generate_samples_is_byte_deterministic(tmp_path) -> None:
    first = tmp_path / "first.json"
    second = tmp_path / "second.json"
    one = generate_samples(first)
    two = generate_samples(second)
    assert one["sha256"] == two["sha256"]
    assert first.read_bytes() == second.read_bytes()
    document = json.loads(first.read_text(encoding="utf-8"))
    assert document["operations"] == ["union", "intersect", "difference", "xor"]


def test_cli_injection_exits_with_business_error(tmp_path) -> None:
    path_a = tmp_path / "a.json"
    path_b = tmp_path / "b.json"
    path_a.write_text(
        json.dumps(
            {
                "fill_rule": "nonzero",
                "subpaths": [
                    {
                        "closed": True,
                        "segments": [
                            {"kind": "L", "points": [[0, 0], [2, 0]]},
                            {"kind": "L", "points": [[2, 0], [2, 2]]},
                            {"kind": "L", "points": [[2, 2], [0, 2]]},
                            {"kind": "L", "points": [[0, 2], [0, 0]]},
                        ],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    path_b.write_text(path_a.read_text(encoding="utf-8"), encoding="utf-8")
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "vectorbool",
            "run",
            "--a",
            str(path_a),
            "--b",
            str(path_b),
            "--operation",
            "union",
            "--tolerance",
            "1e-6",
            "--inject",
            "non_finite_coordinate",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    assert completed.returncode == 2
    payload = json.loads(completed.stdout)
    assert payload["code"] == "E_NON_FINITE_COORDINATE"
    assert "contours" not in payload


def test_cli_intersections_reports_tolerance_difference(tmp_path) -> None:
    segment_a = tmp_path / "a.json"
    segment_b = tmp_path / "b.json"
    segment_a.write_text(
        json.dumps({"kind": "L", "points": [[0, 0], [2, 2]]}),
        encoding="utf-8",
    )
    segment_b.write_text(
        json.dumps({"kind": "L", "points": [[0, 2], [2, 0]]}),
        encoding="utf-8",
    )
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "vectorbool",
            "intersections",
            "--a",
            str(segment_a),
            "--b",
            str(segment_b),
            "--old-tolerance",
            "1e-6",
            "--new-tolerance",
            "1e-4",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    assert completed.returncode == 0
    payload = json.loads(completed.stdout)
    assert len(payload["intersections"]) == 1
    assert payload["difference"]["summary"] == "unchanged"
    assert payload["difference"]["max_position_deviation"] <= 1.0e-12

