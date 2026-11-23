from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest

from acoustic_workbench.analysis import FrameAnalyzer
from acoustic_workbench.codes import E_STATE
from acoustic_workbench.config import AnalysisConfig
from acoustic_workbench.errors import WorkbenchError
from acoustic_workbench.models import RenderConfig
from acoustic_workbench.pipeline import AcousticPipeline
from acoustic_workbench.state import FrameState, FrameStateMachine


def test_state_machine_legal_transitions_and_freeze(short_sample) -> None:
    analyzer = FrameAnalyzer()
    effective, _ = __import__(
        "acoustic_workbench.strategy", fromlist=["resolve_analysis_config"]
    ).resolve_analysis_config(short_sample, AnalysisConfig())
    estimate = analyzer.estimate_frame(short_sample, effective, 0)
    machine = FrameStateMachine(2)
    with pytest.raises(WorkbenchError) as raised:
        machine.mark_committed(0)
    assert raised.value.code == E_STATE
    assert machine.state(0) is FrameState.PENDING
    machine.mark_estimated(0, estimate)
    assert machine.state(0) is FrameState.ESTIMATED
    machine.revise_uncommitted(0)
    assert machine.state(0) is FrameState.PENDING
    machine.mark_estimated(0, estimate)
    machine.mark_committed(0)
    with pytest.raises(WorkbenchError) as raised:
        machine.mark_failed(0, {})
    assert raised.value.code == E_STATE
    assert machine.state(0) is FrameState.COMMITTED
    assert machine.progress()["committed_frames"] == 1


def test_resume_and_uncommitted_config_update(short_sample, tmp_path: Path) -> None:
    checkpoint = tmp_path / "run.sqlite3"
    render = RenderConfig(convolution_window=1024, render_block_size=512)
    initial = AnalysisConfig(
        frame_length=4096,
        hop_length=512,
        estimation_window=4096,
        mode="calibration",
        auto_tradeoff=False,
    )
    pipeline = AcousticPipeline(short_sample, initial, render, checkpoint)
    partial = pipeline.run(stop_after=3)
    assert partial.complete is False
    assert partial.committed_frames == 3
    updated = AnalysisConfig(
        frame_length=4096,
        hop_length=512,
        estimation_window=2048,
        mode="calibration",
        auto_tradeoff=False,
    )
    update = pipeline.update_uncommitted_analysis_config(updated)
    assert update["committed_frames_frozen"] == 3
    completed = pipeline.run()
    assert completed.complete is True
    assert completed.estimates[0].resolution["estimation_window"] == 4096
    assert completed.estimates[3].resolution["estimation_window"] == 2048
    with pytest.raises(WorkbenchError) as raised:
        pipeline.update_render_config(
            RenderConfig(convolution_window=512, render_block_size=512)
        )
    assert raised.value.code == E_STATE
    pipeline.close()

    resumed = AcousticPipeline(short_sample, updated, render, checkpoint)
    assert resumed.snapshot.committed_count == resumed.total_frames
    result = resumed.run()
    assert result.complete is True
    resumed.close()


def test_checkpoint_tamper_invalidates_entire_snapshot(short_sample, tmp_path: Path) -> None:
    checkpoint = tmp_path / "tamper.sqlite3"
    config = AnalysisConfig()
    render = RenderConfig(convolution_window=1024, render_block_size=512)
    pipeline = AcousticPipeline(short_sample, config, render, checkpoint)
    pipeline.run(stop_after=3)
    pipeline.close()

    connection = sqlite3.connect(checkpoint)
    connection.execute(
        "UPDATE meta SET value=? WHERE key='algorithm_version'",
        (json.dumps("tampered-version"),),
    )
    connection.commit()
    connection.close()

    restarted = AcousticPipeline(short_sample, config, render, checkpoint)
    assert restarted.snapshot.committed_count == 0
    assert restarted.state.progress()["committed_frames"] == 0
    connection = sqlite3.connect(checkpoint)
    reasons = [
        row[0]
        for row in connection.execute("SELECT reason FROM invalidation_log").fetchall()
    ]
    connection.close()
    assert any("algorithm version" in reason or "identity" in reason for reason in reasons)
    restarted.close()


def test_cancel_keeps_only_committed_prefix(short_sample, tmp_path: Path) -> None:
    checkpoint = tmp_path / "cancel.sqlite3"
    config = AnalysisConfig(max_parallelism=2)
    render = RenderConfig(convolution_window=512, render_block_size=128)
    pipeline = AcousticPipeline(short_sample, config, render, checkpoint)
    pipeline.request_cancel()
    cancelled = pipeline.run()
    assert cancelled.complete is False
    assert cancelled.cancelled is True
    assert cancelled.committed_frames == 0
    pipeline.close()

    resumed = AcousticPipeline(short_sample, config, render, checkpoint)
    completed = resumed.run()
    assert completed.complete is True
    assert completed.committed_frames == completed.total_frames
    resumed.close()
