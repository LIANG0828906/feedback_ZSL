"""Schema v1.0: pixel-int flat boxes, no per-char confidence,
language and reading order stored at the top level.

{
  "version": "1.0",
  "image": {"id": ..., "width": W, "height": H},
  "language": "eng",
  "reading_order": [<line list index in reading sequence>, ...],
  "lines": [{"box": [x1,y1,...,x4,y4], "text": ..., "num_chars": N}]
}
"""

from __future__ import annotations

from typing import Any, Dict, List, Tuple

from ..errors import VersionError, lossy
from ..model import ImageResult, TextLine
from .base import require, require_type

VERSION = "1.0"


def detect(doc: Dict[str, Any]) -> bool:
    return doc.get("version") == VERSION


def load(doc: Dict[str, Any]) -> ImageResult:
    image = require_type(require(doc, "image", ""), dict, "image")
    image_id = require_type(require(image, "id", "image"), str, "image.id")
    width = require_type(require(image, "width", "image"), int, "image.width")
    height = require_type(require(image, "height", "image"), int, "image.height")
    lang = require_type(require(doc, "language", ""), str, "language")
    order = require_type(
        require(doc, "reading_order", ""), list, "reading_order"
    )
    raw_lines = require_type(require(doc, "lines", ""), list, "lines")

    lines: List[TextLine] = []
    for i, raw in enumerate(raw_lines):
        path = f"lines[{i}]"
        raw = require_type(raw, dict, path)
        box = require_type(require(raw, "box", path), list, f"{path}.box")
        if len(box) != 8 or not all(isinstance(v, int) for v in box):
            raise VersionError(
                f"{path}.box must be 8 pixel ints", field_path=f"{path}.box"
            )
        text = require_type(require(raw, "text", path), str, f"{path}.text")
        num_chars = require_type(
            require(raw, "num_chars", path), int, f"{path}.num_chars"
        )
        if num_chars != len(text):
            raise VersionError(
                f"{path}.num_chars does not match text length",
                field_path=f"{path}.num_chars",
            )
        quad = tuple((box[2 * k], box[2 * k + 1]) for k in range(4))
        lines.append(
            TextLine(
                line_id=i,
                quad=quad,
                text=text,
                char_conf=None,
                lang=lang,
                order_index=i,
            )
        )

    if sorted(order) != list(range(len(lines))):
        raise VersionError(
            "reading_order is not a permutation of line indices",
            field_path="reading_order",
        )
    remapped = []
    for pos, line_idx in enumerate(order):
        ln = lines[line_idx]
        remapped.append(
            TextLine(ln.line_id, ln.quad, ln.text, ln.char_conf, ln.lang, pos)
        )
    return ImageResult(image_id, width, height, tuple(remapped), VERSION)


def dump(result: ImageResult) -> Tuple[Dict[str, Any], list]:
    warnings = []
    ordered = result.ordered_lines()
    top_lang = ordered[0].lang if ordered else "und"
    lines = []
    for pos, ln in enumerate(ordered):
        if ln.char_conf is not None:
            warnings.append(
                lossy(
                    f"lines[{pos}].char_confidence",
                    list(ln.char_conf),
                    "dropped:schema-v1.0-has-no-per-char-confidence",
                )
            )
        if ln.lang != top_lang:
            warnings.append(
                lossy(
                    f"lines[{pos}].language",
                    ln.lang,
                    f"collapsed-to-top-level-language:{top_lang}",
                )
            )
        flat = [coord for pt in ln.quad for coord in pt]
        lines.append({"box": flat, "text": ln.text, "num_chars": ln.char_count})
    doc = {
        "version": VERSION,
        "image": {
            "id": result.image_id,
            "width": result.width,
            "height": result.height,
        },
        "language": top_lang,
        "reading_order": list(range(len(ordered))),
        "lines": lines,
    }
    return doc, warnings
