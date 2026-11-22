"""Independent result validation tool.

Works only from persisted interval records and ground-truth fingerprints.
Re-checks line counts, quad IoU against ground truth, reading-order
continuity and per-char confidence distribution, then compares against the
pipeline's records field by field. The first mismatch is reported with
image id, line id and field name (E_DIFF style).
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Dict, List, Optional, Tuple

from . import engine
from .checkpoint import CheckpointStore
from .generator import gt_regions_of
from .pipeline import line_from_dict

IOU_THRESHOLD = 0.999


def _quad_bbox(quad) -> Tuple[float, float, float, float]:
    xs = [p[0] for p in quad]
    ys = [p[1] for p in quad]
    return min(xs), min(ys), max(xs), max(ys)


def quad_iou(a, b) -> float:
    ax1, ay1, ax2, ay2 = _quad_bbox(a)
    bx1, by1, bx2, by2 = _quad_bbox(b)
    ix1, iy1 = max(ax1, bx1), max(ay1, by1)
    ix2, iy2 = min(ax2, bx2), min(ay2, by2)
    inter = max(0.0, ix2 - ix1) * max(0.0, iy2 - iy1)
    area_a = (ax2 - ax1) * (ay2 - ay1)
    area_b = (bx2 - bx1) * (by2 - by1)
    union = area_a + area_b - inter
    return inter / union if union > 0 else 0.0


class ValidationFailure(Exception):
    def __init__(self, image_id: str, line_id: int, field: str, detail: str):
        super().__init__(
            f"[E_DIFF] image={image_id} line={line_id} field={field}: {detail}"
        )
        self.image_id = image_id
        self.line_id = line_id
        self.field = field


def validate_image(data_dir: Path, image_id: str) -> Dict:
    gt = json.loads(
        (data_dir / "ground_truth" / f"{image_id}.json").read_text(encoding="utf-8")
    )
    store = CheckpointStore(data_dir / "checkpoints")
    snapshot = store.load(image_id)
    if snapshot is None:
        raise ValidationFailure(image_id, -1, "checkpoint", "no persisted snapshot")
    manifest, records = snapshot

    if manifest["committed_intervals"] != manifest["total_intervals"]:
        raise ValidationFailure(
            image_id, -1, "progress",
            f"{manifest['committed_intervals']}/{manifest['total_intervals']} "
            "intervals committed; run is incomplete",
        )

    # Recompute expectations per interval from ground truth + recorded params.
    expected_lines: List = []
    for record in records:
        params = record["params"]
        lines = engine.detect_and_recognize(
            image_id,
            gt_regions_of(gt),
            merge_threshold=params["merge_threshold"],
            conf_threshold=params["conf_threshold"],
        )
        expected_lines.extend(lines[record["line_start"]:record["line_end"]])

    recorded_lines = []
    for record in records:
        recorded_lines.extend(line_from_dict(doc) for doc in record["lines"])

    if len(recorded_lines) != len(expected_lines):
        raise ValidationFailure(
            image_id, -1, "line_count",
            f"{len(recorded_lines)} recorded vs {len(expected_lines)} expected",
        )

    order_indices = sorted(ln.order_index for ln in recorded_lines)
    if order_indices != list(range(len(recorded_lines))):
        raise ValidationFailure(
            image_id, -1, "order_index", "reading order not continuous"
        )

    conf_values: List[float] = []
    for rec, exp in zip(recorded_lines, expected_lines):
        if rec.quad != exp.quad:
            iou = quad_iou(rec.quad, exp.quad)
            if iou < IOU_THRESHOLD:
                raise ValidationFailure(
                    image_id, rec.line_id, "quad",
                    f"IoU {iou:.6f} below {IOU_THRESHOLD}",
                )
        if rec.text != exp.text:
            raise ValidationFailure(image_id, rec.line_id, "text",
                                    f"{rec.text!r} != {exp.text!r}")
        if rec.char_count != exp.char_count:
            raise ValidationFailure(image_id, rec.line_id, "char_count",
                                    f"{rec.char_count} != {exp.char_count}")
        if rec.char_conf != exp.char_conf:
            raise ValidationFailure(image_id, rec.line_id, "char_conf",
                                    "per-char confidence distribution mismatch")
        if rec.lang != exp.lang:
            raise ValidationFailure(image_id, rec.line_id, "lang",
                                    f"{rec.lang!r} != {exp.lang!r}")
        conf_values.extend(rec.char_conf or ())

    distribution = {
        "count": len(conf_values),
        "min": min(conf_values) if conf_values else None,
        "max": max(conf_values) if conf_values else None,
        "mean": (sum(conf_values) / len(conf_values)) if conf_values else None,
    }
    return {
        "image_id": image_id,
        "lines": len(recorded_lines),
        "intervals": len(records),
        "confidence_distribution": distribution,
    }


def validate_all(data_dir) -> List[Dict]:
    data_dir = Path(data_dir)
    manifest = json.loads((data_dir / "manifest.json").read_text(encoding="utf-8"))
    reports = []
    for entry in sorted(manifest["images"], key=lambda e: e["image_id"]):
        reports.append(validate_image(data_dir, entry["image_id"]))
    return reports


def main(argv: Optional[List[str]] = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    data_dir = Path(argv[0] if argv else "data")
    try:
        reports = validate_all(data_dir)
    except ValidationFailure as exc:
        print(str(exc))
        return 1
    total_lines = sum(r["lines"] for r in reports)
    print(f"OK: {len(reports)} images, {total_lines} lines validated")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
