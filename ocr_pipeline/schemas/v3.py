"""Schema v3.0: pixel-int point quads, optional per-char confidence,
default language in meta, reading-order index per item.

{
  "meta": {
    "schema": 3,
    "image": {"id": ..., "width": W, "height": H},
    "default_lang": "eng"
  },
  "items": [{
    "geometry": {"points_px": [[x, y] x4]},
    "text": ...,
    "confidence": {"per_char": [...]},        # optional
    "reading": {"index": 0, "direction": "ltr"},
    "lang": "eng"                              # optional; falls back to meta.default_lang
  }]
}
"""

from __future__ import annotations

from typing import Any, Dict, List, Tuple

from ..errors import VersionError
from ..model import ImageResult, TextLine
from .base import require, require_type

VERSION = "3.0"
SCHEMA_ID = 3


def detect(doc: Dict[str, Any]) -> bool:
    meta = doc.get("meta")
    return isinstance(meta, dict) and meta.get("schema") == SCHEMA_ID


def load(doc: Dict[str, Any]) -> ImageResult:
    meta = require_type(require(doc, "meta", ""), dict, "meta")
    image = require_type(require(meta, "image", "meta"), dict, "meta.image")
    image_id = require_type(require(image, "id", "meta.image"), str, "meta.image.id")
    width = require_type(
        require(image, "width", "meta.image"), int, "meta.image.width"
    )
    height = require_type(
        require(image, "height", "meta.image"), int, "meta.image.height"
    )
    default_lang = require_type(
        require(meta, "default_lang", "meta"), str, "meta.default_lang"
    )
    raw_items = require_type(require(doc, "items", ""), list, "items")

    lines: List[TextLine] = []
    for i, raw in enumerate(raw_items):
        path = f"items[{i}]"
        raw = require_type(raw, dict, path)
        geometry = require_type(require(raw, "geometry", path), dict, f"{path}.geometry")
        points = require_type(
            require(geometry, "points_px", f"{path}.geometry"),
            list,
            f"{path}.geometry.points_px",
        )
        if len(points) != 4 or not all(
            isinstance(pt, list) and len(pt) == 2 and
            all(isinstance(v, int) for v in pt)
            for pt in points
        ):
            raise VersionError(
                f"{path}.geometry.points_px must be 4 integer [x, y] pairs",
                field_path=f"{path}.geometry.points_px",
            )
        quad = tuple((pt[0], pt[1]) for pt in points)
        text = require_type(require(raw, "text", path), str, f"{path}.text")

        char_conf = None
        if "confidence" in raw:
            confidence = require_type(
                raw["confidence"], dict, f"{path}.confidence"
            )
            per_char = require_type(
                require(confidence, "per_char", f"{path}.confidence"),
                list,
                f"{path}.confidence.per_char",
            )
            if len(per_char) != len(text):
                raise VersionError(
                    f"{path}.confidence.per_char must be one float per character",
                    field_path=f"{path}.confidence.per_char",
                )
            char_conf = tuple(float(c) for c in per_char)

        reading = require_type(require(raw, "reading", path), dict, f"{path}.reading")
        index = require_type(
            require(reading, "index", f"{path}.reading"), int, f"{path}.reading.index"
        )
        require_type(
            require(reading, "direction", f"{path}.reading"),
            str,
            f"{path}.reading.direction",
        )
        lang = default_lang
        if "lang" in raw:
            lang = require_type(raw["lang"], str, f"{path}.lang")
        lines.append(
            TextLine(
                line_id=i,
                quad=quad,
                text=text,
                char_conf=char_conf,
                lang=lang,
                order_index=index,
            )
        )
    return ImageResult(image_id, width, height, tuple(lines), VERSION)


def dump(result: ImageResult) -> Tuple[Dict[str, Any], list]:
    ordered = result.ordered_lines()
    default_lang = ordered[0].lang if ordered else "und"
    items = []
    for pos, ln in enumerate(ordered):
        item = {
            "geometry": {"points_px": [[x, y] for x, y in ln.quad]},
            "text": ln.text,
            "reading": {"index": pos, "direction": "ltr"},
        }
        if ln.char_conf is not None:
            item["confidence"] = {"per_char": list(ln.char_conf)}
        if ln.lang != default_lang:
            item["lang"] = ln.lang
        items.append(item)
    doc = {
        "meta": {
            "schema": SCHEMA_ID,
            "image": {
                "id": result.image_id,
                "width": result.width,
                "height": result.height,
            },
            "default_lang": default_lang,
        },
        "items": items,
    }
    return doc, []
