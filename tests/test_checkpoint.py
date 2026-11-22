from __future__ import annotations

import json
from pathlib import Path

import pytest

from glyph_workbench.compare import assert_results_equal
from glyph_workbench.engine import CrashInjected, process_full, process_glyph
from glyph_workbench.params import RenderParams


def _params(**overrides) -> RenderParams:
    values = {
        "pixels_per_unit": 0.25,
        "tolerance": 0.35,
        "grid": 8,
        "max_workers": 3,
    }
    values.update(overrides)
    return RenderParams(**values)


def test_durable_crash_recovery_and_commit_prefix(document, tmp_path: Path) -> None:
    glyph = document.find_glyph("B")
    params = _params()
    persist = tmp_path / "persist"

    def crash_after_two(index: int, result) -> None:
        if index == 1:
            raise CrashInjected("simulated hard stop")

    with pytest.raises(CrashInjected):
        process_glyph(
            glyph,
            params,
            persist_root=persist,
            on_commit=crash_after_two,
        )
    manifests = list(persist.glob("checkpoints/B/*/manifest.json"))
    assert len(manifests) == 1
    manifest = json.loads(manifests[0].read_text(encoding="utf-8"))
    assert manifest["committed_count"] == 2
    assert manifest["commit_sequence"] == [0, 1]
    assert manifest["segments"][2]["committed"] is False
    assert manifest["states"]["segments"][2]["state"] != "COMMITTED"

    recovered = process_glyph(glyph, params, persist_root=persist)
    assert recovered.complete
    assert recovered.loaded_from_checkpoint == 2
    assert recovered.commit_sequence == [0, 1, 2]
    full = process_full(glyph, params)
    assert_results_equal(full, recovered)


def test_parameter_or_content_mismatch_invalidates_whole_snapshot(
    document, tmp_path: Path
) -> None:
    glyph = document.find_glyph("O")
    params = _params()
    persist = tmp_path / "persist"
    first = process_glyph(glyph, params, persist_root=persist)
    assert first.complete

    changed_params = _params(tolerance=0.2)
    after_params = process_glyph(glyph, changed_params, persist_root=persist)
    assert after_params.complete
    assert after_params.checkpoint_invalidated is True
    assert after_params.loaded_from_checkpoint == 0
    assert_results_equal(process_full(glyph, changed_params), after_params)

    changed_glyph = glyph.copy()
    changed_glyph.contours[1].segments[0].p1 = (
        changed_glyph.contours[1].segments[0].p1[0] + 5,
        changed_glyph.contours[1].segments[0].p1[1] + 3,
    )
    after_content = process_glyph(
        changed_glyph, changed_params, persist_root=persist
    )
    assert after_content.complete
    assert after_content.checkpoint_invalidated is True
    assert after_content.loaded_from_checkpoint == 0
    assert_results_equal(
        process_full(changed_glyph, changed_params), after_content
    )

