from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from .io import atomic_write_json, parse_path, serialize_v2


SAMPLE_SEED = 20261122
SAMPLE_VERSION = 1


def _contour(
    contour_id: str,
    start: tuple[float, float],
    segments: list[dict[str, Any]],
) -> dict[str, Any]:
    return {
        "id": contour_id,
        "start": list(start),
        "segments": segments,
    }


def _line(x: float, y: float) -> dict[str, Any]:
    return {"type": "line", "to": [x, y]}


def _quad(cx: float, cy: float, x: float, y: float) -> dict[str, Any]:
    return {"type": "quad", "control": [cx, cy], "to": [x, y]}


def _cubic(
    c1x: float,
    c1y: float,
    c2x: float,
    c2y: float,
    x: float,
    y: float,
) -> dict[str, Any]:
    return {
        "type": "cubic",
        "control1": [c1x, c1y],
        "control2": [c2x, c2y],
        "to": [x, y],
    }


def _v1_document(contours: list[dict[str, Any]], fill_rule: str = "nonzero") -> dict[str, Any]:
    return {
        "format": "vector-path",
        "version": 1,
        "fill_rule": fill_rule,
        "contours": contours,
    }


def _base_a_contours() -> list[dict[str, Any]]:
    outer = _contour(
        "a-outer",
        (20, 10),
        [
            _line(80, 10),
            _cubic(90, 10, 100, 20, 100, 30),
            _line(100, 70),
            _cubic(100, 80, 90, 90, 80, 90),
            _line(20, 90),
            _cubic(10, 90, 0, 80, 0, 70),
            _line(0, 30),
            _cubic(0, 20, 10, 10, 20, 10),
        ],
    )
    # Deliberately clockwise-inconsistent for v1: the loader must apply the
    # declared nesting rule and correct this hole without changing its shape.
    hole = _contour(
        "a-hole",
        (35, 35),
        [
            _line(65, 35),
            _line(65, 65),
            _line(35, 65),
            _line(35, 35),
        ],
    )
    return [outer, hole]


def _base_b_contours() -> list[dict[str, Any]]:
    outer = _contour(
        "b-outer",
        (75, 25),
        [
            _line(125, 25),
            _quad(145, 25, 145, 45),
            _line(145, 85),
            _quad(145, 110, 120, 110),
            _line(75, 110),
            _line(55, 85),
            _line(55, 50),
            _quad(55, 25, 75, 25),
        ],
    )
    hole = _contour(
        "b-hole",
        (90, 50),
        [
            _line(120, 50),
            _line(120, 80),
            _line(90, 80),
            _line(90, 50),
        ],
    )
    return [outer, hole]


def _nested_contours() -> list[dict[str, Any]]:
    return [
        _contour(
            "nested-0",
            (0, 0),
            [_line(120, 0), _line(120, 120), _line(0, 120), _line(0, 0)],
        ),
        _contour(
            "nested-1",
            (20, 20),
            [_line(100, 20), _line(100, 100), _line(20, 100), _line(20, 20)],
        ),
        _contour(
            "nested-2",
            (40, 40),
            [_line(80, 40), _line(80, 80), _line(40, 80), _line(40, 40)],
        ),
    ]


def _near_a_contours() -> list[dict[str, Any]]:
    return [
        _contour(
            "near-a",
            (0, 0),
            [
                _line(20, 0),
                _line(20, 20),
                _line(0, 20),
                _line(0, 0),
            ],
        )
    ]


def _near_b_contours() -> list[dict[str, Any]]:
    return [
        _contour(
            "near-b",
            (20.004, 0),
            [
                _line(40, 0),
                _line(40, 20),
                _line(20.004, 20),
                _line(20.004, 0),
            ],
        )
    ]


def _degenerate_contours() -> list[dict[str, Any]]:
    return [
        _contour(
            "degenerate",
            (0, 0),
            [
                _line(0, 0),
                _line(10, 0),
                _line(10.00001, 0),
                _line(10, 10),
                _line(0, 10),
                _line(0, 0),
            ],
        )
    ]


def _shared_contours() -> list[dict[str, Any]]:
    return [
        _contour(
            "shared-left",
            (0, 0),
            [_line(10, 0), _line(10, 10), _line(0, 10), _line(0, 0)],
        ),
        _contour(
            "shared-right",
            (10, 10),
            [_line(20, 10), _line(20, 20), _line(10, 20), _line(10, 10)],
        ),
    ]


def _to_v2(
    v1_payload: dict[str, Any],
    tolerance: float = 1e-2,
) -> dict[str, Any]:
    path = parse_path(v1_payload)
    payload = serialize_v2(path)
    payload["tolerance"] = tolerance
    payload["tolerance_source"] = "v2_declared"
    for index, contour in enumerate(payload["contours"]):
        contour["group"] = "body" if index == 0 else "cutout"
    payload["groups"] = ["body", "cutout"]
    payload["metadata"] = {
        "sample_generator": "vpipe.samplegen",
        "sample_seed": SAMPLE_SEED,
        "derived_from": "v1",
    }
    return payload


def sample_documents() -> dict[str, dict[str, Any]]:
    documents: dict[str, dict[str, Any]] = {}
    named: dict[str, list[dict[str, Any]]] = {
        "base_a": _base_a_contours(),
        "base_b": _base_b_contours(),
        "nested": _nested_contours(),
        "near_a": _near_a_contours(),
        "near_b": _near_b_contours(),
        "degenerate": _degenerate_contours(),
        "shared": _shared_contours(),
    }
    for name, contours in named.items():
        v1 = _v1_document(contours)
        documents[f"v1/{name}.json"] = v1
        documents[f"v2/{name}.json"] = _to_v2(v1)
    return documents


def _hash_document(document: dict[str, Any]) -> str:
    encoded = json.dumps(
        document,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def ensure_samples(root: str | Path) -> dict[str, Any]:
    directory = Path(root)
    directories = {
        "v1": directory / "v1",
        "v2": directory / "v2",
    }
    for path in directories.values():
        path.mkdir(parents=True, exist_ok=True)
    documents = sample_documents()
    manifest_path = directory / "manifest.json"
    expected_files = {
        relative: _hash_document(document) for relative, document in documents.items()
    }
    if manifest_path.exists():
        try:
            existing = json.loads(manifest_path.read_text(encoding="utf-8"))
            if (
                existing.get("sample_version") == SAMPLE_VERSION
                and existing.get("seed") == SAMPLE_SEED
                and existing.get("files") == expected_files
                and all((directory / relative).exists() for relative in expected_files)
            ):
                return existing
        except (OSError, json.JSONDecodeError):
            pass
    for relative, document in documents.items():
        atomic_write_json(directory / relative, document)
    manifest = {
        "sample_version": SAMPLE_VERSION,
        "seed": SAMPLE_SEED,
        "generator": "vpipe.samplegen.ensure_samples",
        "files": expected_files,
    }
    atomic_write_json(manifest_path, manifest)
    return manifest

