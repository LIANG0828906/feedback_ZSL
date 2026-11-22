import pytest

from ocr_pipeline.errors import MapError
from ocr_pipeline.mapper import validate_lines
from ocr_pipeline.model import TextLine

QUAD = ((10, 10), (50, 10), (50, 30), (10, 30))


def _line(line_id, order, quad=QUAD, text="AB", conf=(0.9, 0.8)):
    return TextLine(line_id, quad, text, conf, "eng", order)


def test_valid_lines_pass():
    validate_lines("img", [_line(0, 0), _line(1, 1), _line(2, 2)])


def test_duplicate_reading_order_raises_e_map():
    lines = [_line(0, 0), _line(1, 1), _line(2, 1)]
    with pytest.raises(MapError) as exc:
        validate_lines("img_9", lines)
    assert exc.value.code == "E_MAP"
    assert exc.value.image_id == "img_9"


def test_noncontinuous_reading_order_raises_e_map():
    lines = [_line(0, 0), _line(1, 2)]
    with pytest.raises(MapError):
        validate_lines("img_9", lines)


def test_degenerate_quad_raises_e_map_with_line_id():
    flat = ((5, 5), (5, 5), (5, 5), (5, 5))
    lines = [_line(0, 0), _line(4, 1, quad=flat)]
    with pytest.raises(MapError) as exc:
        validate_lines("img_9", lines)
    assert exc.value.line_id == 4


def test_conf_length_mismatch_raises_e_map():
    lines = [_line(0, 0, conf=(0.9,))]
    with pytest.raises(MapError):
        validate_lines("img_9", lines)
