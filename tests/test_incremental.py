import json

import pytest

from ocr_pipeline import engine
from ocr_pipeline.errors import DiffError
from ocr_pipeline.generator import gt_regions_of
from ocr_pipeline.scheduler import IncrementalSession


def _regions(dataset, image_id="sample_005"):
    gt = json.loads(
        (dataset / "ground_truth" / f"{image_id}.json").read_text(encoding="utf-8")
    )
    return gt_regions_of(gt)


def test_incremental_matches_full_after_param_adjustment(dataset):
    regions = _regions(dataset)
    session = IncrementalSession("sample_005", regions, lines_per_interval=1)
    # Commit the first interval under the original parameters.
    session.recognize_all()
    session.commit_interval(0)
    # Adjust thresholds: only uncommitted intervals are invalidated.
    invalidated = session.adjust_params(merge_threshold=4.0, conf_threshold=0.75)
    assert invalidated == [iv.interval_index for iv in session.intervals[1:]]
    session.compare_with_full()  # must not raise


def test_committed_interval_keeps_frozen_params(dataset):
    regions = _regions(dataset)
    session = IncrementalSession("sample_005", regions, lines_per_interval=1)
    session.recognize_all()
    session.commit_interval(0)
    frozen_lines = session._cache[0]
    session.adjust_params(merge_threshold=2.0, conf_threshold=0.9)
    session.recognize_all()
    assert session._cache[0] == frozen_lines
    assert session.effective_params(0) == (
        engine.DEFAULT_MERGE_THRESHOLD, engine.DEFAULT_CONF_THRESHOLD)
    assert session.effective_params(1) == (2.0, 0.9)


def test_diff_error_localizes_first_mismatch(dataset):
    regions = _regions(dataset)
    session = IncrementalSession("sample_005", regions, lines_per_interval=1)
    session.recognize_all()
    cached = session._cache[0]
    tampered_first = cached[0].__class__(
        cached[0].line_id, cached[0].quad, "TAMPERED",
        cached[0].char_conf, cached[0].lang, cached[0].order_index,
    )
    tampered = (tampered_first, *cached[1:])
    session._cache[0] = tampered
    with pytest.raises(DiffError) as exc:
        session.compare_with_full()
    assert exc.value.code == "E_DIFF"
    assert exc.value.image_id == "sample_005"
    assert exc.value.line_id == cached[0].line_id
    assert exc.value.field == "text"
