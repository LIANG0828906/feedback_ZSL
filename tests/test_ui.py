from __future__ import annotations

import time


def _wait_until_idle(app, window, timeout=20.0):
    deadline = time.monotonic() + timeout
    while window._busy and time.monotonic() < deadline:
        app.processEvents()
        time.sleep(0.01)
    app.processEvents()
    assert not window._busy


def test_offscreen_window_runs_incremental_and_renders_panels(
    qt_app, sample_bundle, tmp_path
):
    from filter_studio.ui.main import MainWindow, _configure_application

    _configure_application(qt_app)
    sample_dir, _ = sample_bundle
    window = MainWindow(
        sample_dir,
        tmp_path / "ui-cache",
        auto_start=False,
    )
    window.show()
    qt_app.processEvents()
    assert window.sample_combo.count() == 4
    assert window.session is not None
    assert window.session.image is not None

    window._start_pipeline(force_full=False)
    _wait_until_idle(qt_app, window)
    assert window._last_run is not None
    assert window._last_run.status == "complete"
    assert len(window.session.cache) == 3
    assert window.cache_table.rowCount() == 3
    assert window._last_cached_output() is not None

    window.param_widgets["local_contrast"]["blend"].setValue(0.52)
    window._start_pipeline(force_full=False)
    _wait_until_idle(qt_app, window)
    assert window._last_run.status == "complete"
    assert window._last_run.diagnostics.recompute_count == 1
    assert window._last_run.diagnostics.cache_hit_count == 2
    assert "增量" in window.diagnostics_view.toPlainText() or "incremental" in window.diagnostics_view.toPlainText()

    window._start_compare()
    _wait_until_idle(qt_app, window)
    assert window._last_comparison is not None
    assert window._last_comparison.equal is True
    window.close()
    qt_app.processEvents()

