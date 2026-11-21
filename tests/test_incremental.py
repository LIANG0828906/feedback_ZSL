from __future__ import annotations

import threading
from dataclasses import replace

import mcsync.incremental as incremental_module
from mcsync.incremental import IncrementalSyncEngine


def test_mode_change_reuses_markers_segments_and_fits(
    synthetic, pipeline_config
):
    engine = IncrementalSyncEngine(synthetic.audio, pipeline_config)
    update = engine.update_parameters(mode="windowed_sinc")
    report = update.reuse_report
    assert not report["recomputed_marker_channels"]
    assert not report["recomputed_segments"]
    assert not report["recomputed_fits"]
    assert report["recomputed_concat_chunks"]
    assert update.snapshot.revision == 2


def test_single_channel_fit_change_only_rebuilds_that_channel(
    synthetic, pipeline_config
):
    engine = IncrementalSyncEngine(synthetic.audio, pipeline_config)
    fit = engine.snapshot().report.fit_for(1)
    changed = replace(
        fit,
        initial_offset_samples=fit.initial_offset_samples + 0.25,
        drift_ppm=fit.drift_ppm + 0.2,
        slope=fit.slope + 0.2e-6,
    )
    update = engine.update(fit_overrides={1: changed})
    report = update.reuse_report
    assert report["recomputed_fits"] == [1]
    assert report["recomputed_channels"] == [1]
    assert report["fit_parameter_comparison"] is not None
    comparison_channel = next(
        item
        for item in report["fit_parameter_comparison"]["channels"]
        if item["channel"] == 1
    )
    assert abs(comparison_channel["drift_ppm_delta"] - 0.2) < 1.0e-9
    assert comparison_channel["segments"]
    assert all(
        item["channel"] == 1
        for item in report["recomputed_concat_chunks"]
    )
    assert any(
        item["channel"] != 1 for item in report["reused_concat_chunks"]
    )


def test_channel_reduction_removes_only_dropped_channels(
    synthetic, pipeline_config
):
    engine = IncrementalSyncEngine(synthetic.audio, pipeline_config)
    update = engine.update_parameters(active_channels=(0, 1, 2, 3))
    report = update.reuse_report
    assert report["removed_channels"] == [4, 5, 6, 7]
    assert set(report["reused_channels"]) == {0, 1, 2, 3}
    assert not report["recomputed_segments"]
    assert update.snapshot.report.channel_layout.channels[-1].index == 3


def test_segment_length_change_recomputes_segments_and_chunks(
    synthetic, pipeline_config
):
    engine = IncrementalSyncEngine(synthetic.audio, pipeline_config)
    update = engine.update_parameters(segment_length_samples=24000)
    report = update.reuse_report
    assert not report["reused_segments"]
    assert report["recomputed_segments"]
    assert report["recomputed_concat_chunks"]


def test_query_returns_old_snapshot_while_replacement_is_running(
    synthetic, pipeline_config, monkeypatch
):
    engine = IncrementalSyncEngine(synthetic.audio, pipeline_config)
    started = threading.Event()
    release = threading.Event()
    original = incremental_module.resample_channel_range

    def delayed_resample(*args, **kwargs):
        started.set()
        assert release.wait(timeout=10.0)
        return original(*args, **kwargs)

    monkeypatch.setattr(
        incremental_module, "resample_channel_range", delayed_resample
    )
    outcome = {}

    def update() -> None:
        outcome["value"] = engine.update_parameters(mode="windowed_sinc")

    worker = threading.Thread(target=update)
    worker.start()
    assert started.wait(timeout=5.0)
    while_running = engine.snapshot()
    assert while_running.revision == 1
    assert while_running.config.compensation.mode == "linear"
    assert len(while_running.report.markers) == 64
    release.set()
    worker.join(timeout=15.0)
    assert not worker.is_alive()
    finished = engine.snapshot()
    assert finished.revision == 2
    assert finished.config.compensation.mode == "windowed_sinc"
    assert len(finished.report.markers) == 64

