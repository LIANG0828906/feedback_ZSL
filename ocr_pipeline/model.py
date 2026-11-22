"""Internal canonical result model.

All schema versions load into / export from this model. Canonical coordinate
representation is pixel-space integer four-point quads.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional, Tuple

Point = Tuple[int, int]
Quad = Tuple[Point, Point, Point, Point]

SUPPORTED_VERSIONS = ("1.0", "2.0", "3.0")


@dataclass(frozen=True)
class TextLine:
    line_id: int
    quad: Quad
    text: str
    char_conf: Optional[Tuple[float, ...]]
    lang: str
    order_index: int

    @property
    def char_count(self) -> int:
        return len(self.text)


@dataclass(frozen=True)
class ImageResult:
    image_id: str
    width: int
    height: int
    lines: Tuple[TextLine, ...]
    source_version: str

    def ordered_lines(self) -> Tuple[TextLine, ...]:
        return tuple(sorted(self.lines, key=lambda ln: ln.order_index))


@dataclass
class FieldDiff:
    line_id: int
    field: str


_IGNOREABLE = {"char_conf"}


def compare_results(
    expected: ImageResult, actual: ImageResult, *, ignore_fields=frozenset()
) -> Optional[FieldDiff]:
    """Return the first field-level difference, or None when equivalent."""
    if expected.image_id != actual.image_id:
        return FieldDiff(-1, "image_id")
    if (expected.width, expected.height) != (actual.width, actual.height):
        return FieldDiff(-1, "image_size")
    exp = expected.ordered_lines()
    act = actual.ordered_lines()
    if len(exp) != len(act):
        return FieldDiff(-1, "line_count")
    for eln, aln in zip(exp, act):
        if eln.quad != aln.quad and "quad" not in ignore_fields:
            return FieldDiff(eln.line_id, "quad")
        if eln.text != aln.text and "text" not in ignore_fields:
            return FieldDiff(eln.line_id, "text")
        if eln.char_count != aln.char_count and "char_count" not in ignore_fields:
            return FieldDiff(eln.line_id, "char_count")
        if eln.char_conf != aln.char_conf and "char_conf" not in ignore_fields:
            return FieldDiff(eln.line_id, "char_conf")
        if eln.lang != aln.lang and "lang" not in ignore_fields:
            return FieldDiff(eln.line_id, "lang")
        if eln.order_index != aln.order_index and "order_index" not in ignore_fields:
            return FieldDiff(eln.line_id, "order_index")
    return None


def results_equivalent(
    expected: ImageResult, actual: ImageResult, *, ignore_fields=frozenset()
) -> bool:
    return compare_results(expected, actual, ignore_fields=ignore_fields) is None
