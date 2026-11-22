import pytest

from subpipeline.errors import E_PARA, PipelineError
from subpipeline.model import PresentationParams
from subpipeline.wrapping import (
    layout_text, split_screens, tokenize, verify_screens_preserve_text,
    wrap_text,
)


PARAMS = PresentationParams(
    line_height_px=30, max_chars_per_line=6, vertical_margin_px=30,
    screen_height_px=120)


def test_tokenize_cjk_and_ascii():
    assert tokenize("警报AB12，") == ("警", "报", "AB12", "，")


def test_wrap_respects_width_and_no_orphan_punct():
    lines = wrap_text("警报响起，舱门关闭。", PARAMS)
    assert all(len(line) <= 6 for line in lines)
    assert all(line[0] not in "，。、；：？！" for line in lines)
    assert "".join(lines) == "警报响起，舱门关闭。"


def test_wrap_is_deterministic():
    text = "我们必须在能源耗尽之前抵达控制中心。"
    assert wrap_text(text, PARAMS) == wrap_text(text, PARAMS)


def test_screens_keep_character_order():
    text = "信号中断了，最后坐标停留在废弃观测站附近，等待救援。"
    screens = layout_text(text, PARAMS)
    assert len(screens) >= 1
    assert verify_screens_preserve_text(text, screens)
    for screen in screens:
        assert screen.line_count <= PARAMS.max_lines_per_screen


def test_zero_width_raises_epara():
    params = PresentationParams(
        line_height_px=30, max_chars_per_line=0, vertical_margin_px=10,
        screen_height_px=120)
    with pytest.raises(PipelineError) as exc:
        layout_text("任何", params)
    assert exc.value.code == E_PARA


def test_long_ascii_word_raises_epara():
    params = PresentationParams(
        line_height_px=30, max_chars_per_line=2, vertical_margin_px=10,
        screen_height_px=120)
    with pytest.raises(PipelineError) as exc:
        layout_text("WARNING", params)
    assert exc.value.code == E_PARA


def test_split_screens_balanced():
    lines = tuple("一二三四五六七八九十")
    screens = split_screens(lines, PARAMS)
    assert sum(s.line_count for s in screens) == 10
    assert all(s.line_count <= PARAMS.max_lines_per_screen for s in screens)
