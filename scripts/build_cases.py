#!/usr/bin/env python3
"""Build scripted TF/view cases and valid/corrupt snapshot fixtures."""

from __future__ import annotations

import base64
import hashlib
import json
import sys
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender.camera import default_view_matrix, look_at_view  # noqa: E402
from volrender.render import VolumeRenderer, canonical_json_bytes  # noqa: E402
from volrender.transfer import TransferFunction  # noqa: E402
from volrender.volume import VolumeData  # noqa: E402


VALID_POINTS = [
    {"x": 0.00, "opacity": 0.00, "r": 0.00, "g": 0.00, "b": 0.00},
    {"x": 0.12, "opacity": 0.01, "r": 0.18, "g": 0.42, "b": 0.95},
    {"x": 0.42, "opacity": 0.16, "r": 0.95, "g": 0.62, "b": 0.18},
    {"x": 0.72, "opacity": 0.58, "r": 1.00, "g": 0.88, "b": 0.74},
    {"x": 1.00, "opacity": 0.94, "r": 1.00, "g": 1.00, "b": 1.00},
]
LOW_OPACITY_POINTS = [
    {"x": 0.00, "opacity": 0.00, "r": 0.10, "g": 0.15, "b": 0.30},
    {"x": 0.25, "opacity": 0.002, "r": 0.20, "g": 0.45, "b": 0.90},
    {"x": 0.50, "opacity": 0.012, "r": 0.45, "g": 0.75, "b": 0.95},
    {"x": 0.75, "opacity": 0.050, "r": 1.00, "g": 0.65, "b": 0.20},
    {"x": 1.00, "opacity": 0.120, "r": 1.00, "g": 1.00, "b": 1.00},
]


def sha256_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> int:
    cases_dir = ROOT / "cases"
    snapshot_dir = cases_dir / "snapshots"
    snapshot_dir.mkdir(parents=True, exist_ok=True)

    volume = VolumeData.from_file(ROOT / "samples" / "density_64.vol")
    views = {
        "default": default_view_matrix(volume.world_extent).tolist(),
        "oblique": look_at_view((105.0, -35.0, 55.0), (0.0, 0.0, 0.0), (0.0, 0.0, 1.0)).tolist(),
    }
    cases = {
        "schema_version": 1,
        "views": [
            {"name": name, "matrix": matrix} for name, matrix in views.items()
        ],
        "transfer_functions": [
            {"name": "medical_default", "points": VALID_POINTS, "expected_code": "OK"},
            {"name": "low_opacity", "points": LOW_OPACITY_POINTS, "expected_code": "OK"},
            {
                "name": "invalid_nonmonotonic",
                "points": [
                    {"x": 0.0, "opacity": 0.0, "r": 0, "g": 0, "b": 0},
                    {"x": 0.7, "opacity": 0.2, "r": 0.5, "g": 0.5, "b": 0.5},
                    {"x": 0.4, "opacity": 0.5, "r": 1, "g": 1, "b": 1},
                ],
                "expected_code": "E_TF_INVALID",
            },
            {
                "name": "invalid_duplicate_x",
                "points": [
                    {"x": 0.0, "opacity": 0.0, "r": 0, "g": 0, "b": 0},
                    {"x": 0.5, "opacity": 0.2, "r": 0.5, "g": 0.5, "b": 0.5},
                    {"x": 0.5, "opacity": 0.5, "r": 1, "g": 1, "b": 1},
                ],
                "expected_code": "E_TF_INVALID",
            },
            {
                "name": "invalid_color_range",
                "points": [
                    {"x": 0.0, "opacity": 0.0, "r": 0, "g": 0, "b": 0},
                    {"x": 1.0, "opacity": 0.5, "r": 1.2, "g": 1, "b": 1},
                ],
                "expected_code": "E_TF_INVALID",
            },
            {
                "name": "invalid_nan",
                "points": [
                    {"x": 0.0, "opacity": 0.0, "r": 0, "g": 0, "b": 0},
                    {"x": 1.0, "opacity": "nan", "r": 1, "g": 1, "b": 1},
                ],
                "expected_code": "E_TF_INVALID",
            },
        ],
        "precision_modes": [
            {"name": "float", "expected_code": "OK"},
            {"name": "fixed", "expected_code": "OK"},
            {"name": "int8", "expected_code": "E_MODE_INVALID"},
        ],
        "snapshot_cases": [
            {"name": "good_float", "file": "snapshots/good_float.volsnap.json", "expected_code": "OK"},
            {"name": "good_fixed", "file": "snapshots/good_fixed.volsnap.json", "expected_code": "OK"},
            {"name": "corrupt_truncated", "file": "snapshots/corrupt_truncated.volsnap.json", "expected_code": "E_SNAPSHOT_INVALID"},
            {"name": "corrupt_content", "file": "snapshots/corrupt_content.volsnap.json", "expected_code": "E_SNAPSHOT_INVALID"},
            {"name": "corrupt_missing_field", "file": "snapshots/corrupt_missing_field.volsnap.json", "expected_code": "E_SNAPSHOT_INVALID"},
            {"name": "stale_view", "file": "snapshots/stale_view.volsnap.json", "expected_code": "E_SNAPSHOT_STALE"},
            {"name": "stale_step", "file": "snapshots/stale_step.volsnap.json", "expected_code": "E_SNAPSHOT_STALE"},
        ],
    }
    cases_path = cases_dir / "cases.json"
    cases_path.write_text(json.dumps(cases, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    transfer = TransferFunction(VALID_POINTS)
    good_paths: dict[str, Path] = {}
    for mode in ("float", "fixed"):
        renderer = VolumeRenderer(
            volume=volume,
            transfer=transfer,
            mode=mode,
            view_matrix=np.asarray(views["default"]),
            step=1.0,
            image_size=(48, 48),
            error_threshold=1.0e-3,
        )
        result = renderer.render()
        path = snapshot_dir / f"good_{mode}.volsnap.json"
        renderer.save_snapshot(path, result=result)
        good_paths[mode] = path

    good_bytes = good_paths["float"].read_bytes()
    (snapshot_dir / "corrupt_truncated.volsnap.json").write_bytes(good_bytes[: len(good_bytes) // 3])

    good_doc = json.loads(good_bytes.decode("utf-8"))

    stale_view = json.loads(json.dumps(good_doc))
    stale_view["state"]["view_matrix"][0][0] += 0.01
    (snapshot_dir / "stale_view.volsnap.json").write_text(
        json.dumps(stale_view, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )

    stale_step = json.loads(json.dumps(good_doc))
    stale_step["state"]["step"] = 1.5
    (snapshot_dir / "stale_step.volsnap.json").write_text(
        json.dumps(stale_step, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )

    corrupt_content = json.loads(json.dumps(good_doc))
    rgba = bytearray(base64.b64decode(corrupt_content["result"]["rgba8"]))
    rgba[0] ^= 0xFF
    corrupt_content["result"]["rgba8"] = base64.b64encode(bytes(rgba)).decode("ascii")
    unhashed = dict(corrupt_content)
    unhashed.pop("snapshot_hash", None)
    corrupt_content["snapshot_hash"] = hashlib.sha256(
        canonical_json_bytes(unhashed)
    ).hexdigest()
    (snapshot_dir / "corrupt_content.volsnap.json").write_text(
        json.dumps(corrupt_content, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )

    missing_field = json.loads(json.dumps(good_doc))
    del missing_field["result"]["error_summary"]
    unhashed = dict(missing_field)
    unhashed.pop("snapshot_hash", None)
    missing_field["snapshot_hash"] = hashlib.sha256(
        canonical_json_bytes(unhashed)
    ).hexdigest()
    (snapshot_dir / "corrupt_missing_field.volsnap.json").write_text(
        json.dumps(missing_field, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )

    manifest = {
        "cases": str(cases_path.relative_to(ROOT)),
        "cases_sha256": sha256_file(cases_path),
        "snapshots": {
            path.name: {
                "sha256": sha256_file(path),
                "bytes": path.stat().st_size,
            }
            for path in sorted(snapshot_dir.glob("*.json"))
        },
    }
    (cases_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(manifest, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
