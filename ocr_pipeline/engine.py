"""Deterministic local pseudo-OCR engine.

Detection and recognition are pure functions of (image identity, ground-truth
regions, parameters). Nothing depends on cache state, interval partitioning
or commit history, which is what makes incremental re-recognition provably
identical to a from-scratch full run.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import List, Sequence, Tuple

from .model import Quad, TextLine

ALGO_VERSION = "algo-det1-rec1"
DEFAULT_MERGE_THRESHOLD = 12.0
DEFAULT_CONF_THRESHOLD = 0.60


@dataclass(frozen=True)
class GtRegion:
    quad: Quad
    text: str
    char_count: int
    avg_stroke_width: float


def param_version(merge_threshold: float, conf_threshold: float) -> str:
    digest = hashlib.sha256(
        f"merge={merge_threshold!r}|conf={conf_threshold!r}".encode("utf-8")
    ).hexdigest()
    return digest[:16]


def char_confidence(
    image_id: str, line_key: str, char_index: int, ch: str, stroke_width: float
) -> float:
    """Deterministic per-character confidence in [0.5, 1.0)."""
    payload = f"{image_id}|{line_key}|{char_index}|{ch}|{stroke_width:.3f}"
    digest = hashlib.sha256(payload.encode("utf-8")).digest()
    return 0.5 + (int.from_bytes(digest[:4], "big") % 5000) / 10000.0


def _quad_top(quad: Quad) -> float:
    return sum(p[1] for p in quad) / 4.0


def _quad_left(quad: Quad) -> float:
    return min(p[0] for p in quad)


def _quad_right(quad: Quad) -> float:
    return max(p[0] for p in quad)


def _quad_height(quad: Quad) -> float:
    ys = [p[1] for p in quad]
    return max(ys) - min(ys)


def _envelope(quads: Sequence[Quad]) -> Quad:
    xs = [p[0] for q in quads for p in q]
    ys = [p[1] for q in quads for p in q]
    return (
        (min(xs), min(ys)),
        (max(xs), min(ys)),
        (max(xs), max(ys)),
        (min(xs), max(ys)),
    )


def _group_lines(regions: Sequence[GtRegion]) -> List[List[GtRegion]]:
    """Group regions into visual lines by vertical-center proximity."""
    ordered = sorted(regions, key=lambda r: (_quad_top(r.quad), _quad_left(r.quad)))
    lines: List[List[GtRegion]] = []
    centers: List[float] = []
    for region in ordered:
        ys = [p[1] for p in region.quad]
        center = (min(ys) + max(ys)) / 2.0
        height = max(_quad_height(region.quad), 1.0)
        placed = False
        for idx, existing_center in enumerate(centers):
            if abs(center - existing_center) <= height * 0.5:
                lines[idx].append(region)
                placed = True
                break
        if not placed:
            lines.append([region])
            centers.append(center)
    return lines


def detect_and_recognize(
    image_id: str,
    regions: Sequence[GtRegion],
    *,
    merge_threshold: float = DEFAULT_MERGE_THRESHOLD,
    conf_threshold: float = DEFAULT_CONF_THRESHOLD,
) -> Tuple[TextLine, ...]:
    """Run deterministic detection + recognition for one image."""
    detected: List[Tuple[Quad, List[GtRegion]]] = []
    for line_regions in _group_lines(regions):
        ordered = sorted(line_regions, key=lambda r: _quad_left(r.quad))
        current: List[GtRegion] = []
        prev_right = None
        for region in ordered:
            left = _quad_left(region.quad)
            if prev_right is not None and left - prev_right > merge_threshold:
                detected.append((_envelope([r.quad for r in current]), current))
                current = []
            current.append(region)
            prev_right = max(_quad_right(r.quad) for r in current)
        if current:
            detected.append((_envelope([r.quad for r in current]), current))

    detected.sort(key=lambda item: (_quad_top(item[0]), _quad_left(item[0])))

    lines: List[TextLine] = []
    for order_index, (quad, members) in enumerate(detected):
        line_key = "|".join(str(p) for p in quad)
        text_chars: List[str] = []
        confs: List[float] = []
        char_index = 0
        for region in members:
            for ch in region.text:
                conf = char_confidence(
                    image_id, line_key, char_index, ch, region.avg_stroke_width
                )
                if conf >= conf_threshold:
                    text_chars.append(ch)
                    confs.append(conf)
                char_index += 1
        lines.append(
            TextLine(
                line_id=order_index,
                quad=quad,
                text="".join(text_chars),
                char_conf=tuple(confs),
                lang="eng",
                order_index=order_index,
            )
        )
    return tuple(lines)
