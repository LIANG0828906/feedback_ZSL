from __future__ import annotations

from pathlib import Path

import pytest

import glyph_workbench.session as session_module
from glyph_workbench.compare import assert_results_equal
from glyph_workbench.errors import WorkbenchError
from glyph_workbench.models import load_document
from glyph_workbench.params import RenderParams
from glyph_workbench.session import EditSession


def _params() -> RenderParams:
    return RenderParams(
        pixels_per_unit=0.25,
        tolerance=0.35,
        grid=8,
        max_workers=2,
    )


def test_uncommitted_edits_do_not_affect_export_and_dependencies_reuse(
    document, tmp_path: Path
) -> None:
    model_path = tmp_path / "model.json"
    session = EditSession(document, "B", _params(), tmp_path / "persist", model_path)
    session.commit()
    committed_export = session.export_result()
    committed_hash = session.committed_glyph.content_hash()

    segment = session.working_glyph.contours[2].segments[0]
    session.set_control_point(
        2,
        0,
        "p1",
        (segment.p1[0] + 7, segment.p1[1] + 3),
    )
    assert session.working_glyph.content_hash() != committed_hash
    assert session.committed_glyph.content_hash() == committed_hash
    assert_results_equal(committed_export, session.export_result())
    assert model_path.exists()

    info = session.commit()
    assert info.success
    assert info.affected_contours == [2]
    assert info.reused_contours == [0, 1]
    assert info.result is not None and info.result.complete
    assert session.dirty_contours == set()
    assert load_document(model_path).find_glyph("B").content_hash() == session.working_glyph.content_hash()


def test_batch_commit_rolls_back_model_and_working_copy_on_write_failure(
    document, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    model_path = tmp_path / "model.json"
    session = EditSession(document, "A", _params(), tmp_path / "persist", model_path)
    session.commit()
    before_text = model_path.read_text(encoding="utf-8")
    before_hash = session.committed_glyph.content_hash()

    def fail_write(*args, **kwargs):
        raise WorkbenchError("E_IO", "simulated atomic write failure")

    monkeypatch.setattr(session_module, "save_document_atomic", fail_write)
    segment = session.working_glyph.contours[0].segments[1]
    session.set_control_point(
        0,
        1,
        "p1",
        (segment.p1[0] + 11, segment.p1[1] - 4),
    )
    session.set_parameters(
        RenderParams(
            pixels_per_unit=0.3,
            tolerance=0.3,
            grid=8,
            max_workers=2,
        )
    )
    with pytest.raises(WorkbenchError) as caught:
        session.commit()
    assert caught.value.code == "E_IO"
    assert session.committed_glyph.content_hash() == before_hash
    assert session.working_glyph.content_hash() == before_hash
    assert session.working_parameters == session.committed_parameters
    assert session.dirty_contours == set()
    assert model_path.read_text(encoding="utf-8") == before_text


def test_split_delete_and_drag_preserve_contour_continuity(document, tmp_path: Path) -> None:
    session = EditSession(
        document, "A", _params(), tmp_path / "persist", tmp_path / "model.json"
    )
    session.split_segment(0, 1, t=0.5)
    contour = session.working_glyph.contours[0]
    for index in range(1, len(contour.segments)):
        assert contour.segments[index].p0 == contour.segments[index - 1].endpoint
    assert contour.segments[-1].endpoint == contour.start

    session.delete_segment(0, 2)
    contour = session.working_glyph.contours[0]
    assert contour.segments[0].p0 == contour.start
    for index in range(1, len(contour.segments)):
        assert contour.segments[index].p0 == contour.segments[index - 1].endpoint
    assert contour.segments[-1].endpoint == contour.start

    target = contour.segments[3]
    session.set_control_point(
        0,
        3,
        "p1",
        (target.p1[0] + 12, target.p1[1] + 9),
    )
    contour = session.working_glyph.contours[0]
    for index in range(1, len(contour.segments)):
        assert contour.segments[index].p0 == contour.segments[index - 1].endpoint
    assert contour.segments[-1].endpoint == contour.start

