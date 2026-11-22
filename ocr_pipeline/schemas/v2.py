"""Schema v2.0: normalized float quads, required per-char confidence,
language and reading-order index stored per region.

{
  "schema_version": "2.0",
  "image_id": ...,
  "image_size": {"w": W, "h": H},
  "regions": [{
    "quad_norm": [[x, y] x4],
    "content": ...,
    "char_confidence": [...],
    "lang": "eng",
    "order": 0
  }]
}
"""

from __future__ import annotations

from typing import Any, Dict, List, Tuple

from ..coords import normalized_to_pixel, pixel_to_normalized
from ..errors import VersionError
from ..model import ImageResult, TextLine
from .base import require, require_type

VERSION = "2.0"


def detect(doc: Dict[str, Any]) -> bool:
    return doc.get("schema_version") == VERSION


def load(doc: Dict[str, Any], *, rounding: str = "half_up") -> ImageResult:
    image_id = require_type(require(doc, "image_id", ""), str, "image_id")
    size = require_type(require(doc, "image_size", ""), dict, "image_size")
    width = require_type(require(size, "w", "image_size"), int, "image_size.w")
    height = require_type(require(size, "h", "image_size"), int, "image_size.h")
    raw_regions = require_type(require(doc, "regions", ""), list, "regions")

    lines: List[TextLine] = []
    for i, raw in enumerate(raw_regions):
        path = f"regions[{i}]"
        raw = require_type(raw, dict, path)
        nquad = require_type(require(raw, "quad_norm", path), list, f"{path}.quad_norm")
        if len(nquad) != 4 or not all(
            isinstance(pt, list) and len(pt) == 2 for pt in nquad
        ):
            raise VersionError(
                f"{path}.quad_norm must be 4 [x, y] pairs",
                field_path=f"{path}.quad_norm",
            )
        quad = normalized_to_pixel(
            tuple(tuple(pt) for pt in nquad),
            width,
            height,
            rounding=rounding,
            line_id=i,
        )
        text = require_type(require(raw, "content", path), str, f"{path}.content")
        conf = require_type(
            require(raw, "char_confidence", path), list, f"{path}.char_confidence"
        )
        if len(conf) != len(text) or not all(
            isinstance(c, (int, float)) for c in conf
        ):
            raise VersionError(
                f"{path}.char_confidence must be one float per character",
                field_path=f"{path}.char_confidence",
            )
        lang = require_type(require(raw, "lang", path), str, f"{path}.lang")
        order = require_type(require(raw, "order", path), int, f"{path}.order")
        lines.append(
            TextLine(
                line_id=i,
                quad=quad,
                text=text,
                char_conf=tuple(float(c) for c in conf),
                lang=lang,
                order_index=order,
            )
        )
    return ImageResult(image_id, width, height, tuple(lines), VERSION)


def dump(
    result: ImageResult, *, precision: int = 6, rounding: str = "half_up"
) -> Tuple[Dict[str, Any], list]:
    regions = []
    for pos, ln in enumerate(result.ordered_lines()):
        nquad = pixel_to_normalized(
            ln.quad,
            result.width,
            result.height,
            precision=precision,
            rounding=rounding,
            line_id=ln.line_id,
        )
        conf = ln.char_conf if ln.char_conf is not None else ()
        regions.append(
            {
                "quad_norm": [[nx, ny] for nx, ny in nquad],
                "content": ln.text,
                "char_confidence": list(conf),
                "lang": ln.lang,
                "order": pos,
            }
        )
    doc = {
        "schema_version": VERSION,
        "image_id": result.image_id,
        "image_size": {"w": result.width, "h": result.height},
        "regions": regions,
    }
    return doc, []
