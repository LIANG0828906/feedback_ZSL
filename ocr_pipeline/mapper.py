"""Mapping validation between detection/recognition and the internal model.

Checks per image: reading-order indices continuous and unique, quads
non-degenerate, per-char confidence length consistent. Failures raise
E_MAP with the image id and line id. Only mapped intervals may commit.
"""

from __future__ import annotations

from typing import Sequence

from .errors import MapError
from .model import TextLine


def _is_degenerate(quad) -> bool:
    if len(set(quad)) < 3:
        return True
    xs = [p[0] for p in quad]
    ys = [p[1] for p in quad]
    return (max(xs) - min(xs)) == 0 or (max(ys) - min(ys)) == 0


def validate_lines(image_id: str, lines: Sequence[TextLine]) -> None:
    indices = [ln.order_index for ln in lines]
    if sorted(indices) != list(range(len(lines))):
        bad = next(
            (ln.line_id for ln in lines
             if indices.count(ln.order_index) > 1),
            lines[0].line_id if lines else -1,
        )
        raise MapError(
            "reading-order indices not continuous or duplicated",
            image_id=image_id,
            line_id=bad,
        )
    for ln in lines:
        if _is_degenerate(ln.quad):
            raise MapError(
                "degenerate quad", image_id=image_id, line_id=ln.line_id
            )
        if ln.char_conf is not None and len(ln.char_conf) != ln.char_count:
            raise MapError(
                "char_conf length does not match char count",
                image_id=image_id,
                line_id=ln.line_id,
            )
