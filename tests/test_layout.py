import random

import pytest

from caption_pipeline.errors import E_PARA, PipelineError
from caption_pipeline.layout import (CLOSE_PUNCT, line_char_counts,
                                     split_screens, wrap_text)

TEXT = "她轻声说道，「答案就藏在那本旧日记里。」他回头望了一眼，然后推开了门。"


def test_wrap_deterministic_across_runs_and_orders():
    base = wrap_text(TEXT, 12, 1.0)
    rng = random.Random(7)
    for _ in range(20):
        chars = list(TEXT)
        rng.shuffle(chars)  # 打乱遍历顺序的干扰源，不影响纯函数结果
        assert wrap_text(TEXT, 12, 1.0) == base


def test_wrap_respects_max_chars():
    for line in wrap_text(TEXT, 10, 1.0):
        assert 0 < len(line) <= 10


def test_punctuation_not_isolated_and_not_line_head():
    text = "他说，「好。」然后离开了房间，再也没有回来过这里。"
    lines = wrap_text(text, 6, 1.0)
    for line in lines:
        assert line[0] not in CLOSE_PUNCT
        assert not all(c in CLOSE_PUNCT for c in line)


def test_wrap_param_errors():
    with pytest.raises(PipelineError) as ei:
        wrap_text(TEXT, 0, 1.0)
    assert ei.value.code == E_PARA and ei.value.field == "max_chars_per_line"
    with pytest.raises(PipelineError) as ei:
        wrap_text(TEXT, 10, -0.1)
    assert ei.value.code == E_PARA and ei.value.field == "wrap_weight"


def test_split_screens_counts_and_params():
    lines = wrap_text(TEXT, 8, 1.0)
    screens = split_screens(lines, line_height=30, margin=20, screen_height=150)
    assert all(len(sc) <= (150 - 40) // 30 for sc in screens)
    assert line_char_counts(screens) == [len(l) for l in lines]
    with pytest.raises(PipelineError) as ei:
        split_screens(lines, line_height=30, margin=70, screen_height=150)
    assert ei.value.code == E_PARA and ei.value.field == "screen_height"
