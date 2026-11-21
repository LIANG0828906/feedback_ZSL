"""要求 6：跨档同片段起止样本一致性。"""

from __future__ import annotations

import pytest

from mediaaudit.errors import ErrorCode
from mediaaudit.parameters import TierParams
from mediaaudit.span import assert_spans_consistent, compare_tier_spans
from tests.test_slicing import _source


def test_consistent_when_quantization_aligned() -> None:
    source = _source(48000)
    pa = TierParams(16000, 256, 1000, 250_000)
    pb = TierParams(48000, 256, 1000, 250_000)
    assert compare_tier_spans(source, "a", 1, pa, "b", 1, pb) == []
    assert_spans_consistent(source, "a", 1, pa, "b", 1, pb)


def test_mismatch_locates_first_window_and_offsets() -> None:
    source = _source(48000)
    pa = TierParams(16000, 256, 1000, 500_000)
    pb = TierParams(48000, 256, 750, 375_000)
    mismatches = compare_tier_spans(source, "a", 1, pa, "b", 2, pb)
    assert mismatches
    assert mismatches[0].first_window_index == 0
    with pytest.raises(Exception) as exc:
        assert_spans_consistent(source, "a", 1, pa, "b", 2, pb)
    assert exc.value.code == ErrorCode.E_SPAN_MISMATCH
    assert exc.value.location[0] == "span"


def test_rollback_induced_boundary_change_flagged() -> None:
    source = _source(48000)
    aligned = TierParams(48000, 256, 1000, 250_000)
    shifted = TierParams(48000, 256, 750, 375_000)
    mismatches = compare_tier_spans(source, "a", 3, aligned, "a", 4, shifted)
    assert mismatches
    assert mismatches[0].involved_tiers == ("a", "a")
