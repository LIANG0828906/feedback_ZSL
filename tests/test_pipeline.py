from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest

from spatial_workbench.compare import compare_render_results, compare_three_paths, require_equal
from spatial_workbench.errors import ErrorCode, WorkbenchError
from spatial_workbench.io import load_sample
from spatial_workbench.models import AnalysisConfig, RenderResult
from spatial_workbench.pipeline import ReconstructionPipeline
from spatial_workbench.render import RenderEngine


def test_render_length_and_three_paths_exact(normal_sample, config, tmp_path):
    realtime_path = tmp_path / "realtime.json"
    recovery_path = tmp_path / "recovery.json"
    realtime = ReconstructionPipeline(normal_sample, config, realtime_path).run_realtime()
    assert realtime.complete and realtime.error is None
    assert realtime.result.output.shape == (4, normal_sample.dry.size)
    assert realtime.state.counts()["committed"] == 32
    partial = ReconstructionPipeline(normal_sample, config, recovery_path).run_realtime(
        stop_after=7
    )
    assert partial.complete is False
    assert partial.state.counts()["committed"] == 7
    recovered = ReconstructionPipeline(normal_sample, config, recovery_path).run_recovery()
    replay = ReconstructionPipeline(normal_sample, config, realtime_path).run_replay()
    assert recovered.complete and replay.complete
    assert np.array_equal(realtime.result.output, recovered.result.output)
    assert np.array_equal(realtime.result.output, replay.result.output)
    assert [f.fingerprint() for f in realtime.result.frames] == [
        f.fingerprint() for f in recovered.result.frames
    ] == [f.fingerprint() for f in replay.result.frames]
    assert realtime.result.stats_dict() == recovered.result.stats_dict()
    assert realtime.result.stats_dict() == replay.result.stats_dict()
    assert compare_three_paths(
        realtime.result,
        recovered.result,
        replay.result,
    ).equal


def test_first_difference_localizes_sample_and_frame(normal_sample, config, tmp_path):
    outcome = ReconstructionPipeline(
        normal_sample,
        config,
        tmp_path / "checkpoint.json",
    ).run_realtime()
    mutated = np.array(outcome.result.output, copy=True)
    mutated[1, 777] += 2.0e-9
    candidate = RenderResult(
        output=mutated,
        channels=outcome.result.channels,
        sample_rate=outcome.result.sample_rate,
        frames=outcome.result.frames,
        render_stats=outcome.result.render_stats,
        path="mutated",
    )
    report = compare_render_results(outcome.result, candidate)
    assert not report.equal
    assert report.first_sample == 777
    assert report.first_frame == 777 // config.hop_length
    with pytest.raises(WorkbenchError) as caught:
        require_equal(
            outcome.result,
            candidate,
            reference_path="reference",
            candidate_path="candidate",
        )
    assert caught.value.code == ErrorCode.E_DIFF
    assert caught.value.details["first_difference"]["first_sample"] == 777


def test_bad_frame_failure_retry_does_not_pollute_others(sample_bundle, config, tmp_path):
    sample = load_sample(sample_bundle.bad_frame)
    checkpoint = tmp_path / "retry.json"
    first = ReconstructionPipeline(sample, config, checkpoint).run_realtime()
    assert first.error.code == ErrorCode.E_FRAME
    assert first.state.counts()["failed"] == 1
    assert first.state.counts()["committed"] == 3
    assert len(first.result.frames) == 3
    retry_sample = sample.copy_with(bad_frames=())
    assert retry_sample.content_hash == sample.content_hash
    retry = ReconstructionPipeline(retry_sample, config, checkpoint).run_recovery()
    assert retry.complete
    assert retry.error is None
    assert retry.state.counts()["committed"] == 32


def test_checkpoint_mismatch_forces_full_recompute(normal_sample, config, tmp_path):
    checkpoint = tmp_path / "checkpoint.json"
    first = ReconstructionPipeline(normal_sample, config, checkpoint).run_realtime()
    assert first.complete
    changed_dry = np.array(normal_sample.dry, copy=True)
    changed_dry[0] += 1.0e-7
    changed = normal_sample.copy_with(dry=changed_dry)
    recovered = ReconstructionPipeline(changed, config, checkpoint).run_recovery()
    assert recovered.checkpoint_invalidated
    assert recovered.complete
    assert recovered.state.counts()["committed"] == 32

    algorithm_config = config.replace(algorithm_version="algorithm-v2")
    algorithm = ReconstructionPipeline(
        normal_sample,
        algorithm_config,
        tmp_path / "algorithm.json",
    )
    algorithm.run_realtime()
    mismatch = ReconstructionPipeline(
        normal_sample,
        config,
        tmp_path / "algorithm.json",
    ).run_recovery()
    assert mismatch.checkpoint_invalidated
    assert mismatch.complete


def test_output_layout_mapping(normal_sample, config, tmp_path):
    stereo_config = config.replace(output_layout="stereo")
    outcome = ReconstructionPipeline(
        normal_sample,
        stereo_config,
        tmp_path / "stereo.json",
    ).run_realtime()
    assert outcome.complete
    assert outcome.result.output.shape == (2, normal_sample.dry.size)
    assert outcome.result.channels == ("FL", "FR")


def test_custom_channel_weight_mapping(normal_sample, config, tmp_path):
    weights = {
        "L": {"FL": 1.0, "RL": 0.5},
        "R": {"FR": 1.0, "RR": 0.5},
    }
    mapped = config.replace(channel_weights=weights)
    outcome = ReconstructionPipeline(
        normal_sample,
        mapped,
        tmp_path / "custom-map.json",
    ).run_realtime()
    assert outcome.complete
    assert outcome.result.channels == ("L", "R")
    assert outcome.result.output.shape == (2, normal_sample.dry.size)
    invalid = config.replace(
        channel_weights={"L": {"FL": 5.0}},
    )
    failed = ReconstructionPipeline(
        normal_sample,
        invalid,
        tmp_path / "invalid-map.json",
    ).run_realtime()
    assert failed.error.code == ErrorCode.E_PARA


def test_reconfigure_only_uncommitted_frames(normal_sample, config, tmp_path):
    checkpoint = tmp_path / "reconfigure.json"
    pipeline = ReconstructionPipeline(normal_sample, config, checkpoint)
    partial = pipeline.run_realtime(stop_after=8)
    frozen = [frame.fingerprint() for frame in partial.result.frames]
    pipeline.reconfigure_uncommitted(
        frame_length=256,
        energy_threshold=0.02,
        output_layout="quad",
    )
    recovered = pipeline.run_recovery()
    assert recovered.complete
    assert [frame.fingerprint() for frame in recovered.result.frames[:8]] == frozen
    assert all(
        frame.frame_length == 256 for frame in recovered.result.frames[8:]
    )
    assert all(
        frame.frame_length == config.frame_length
        for frame in recovered.result.frames[:8]
    )


def test_resource_limit(normal_sample, tmp_path):
    limited = AnalysisConfig(
        frame_length=512,
        hop_length=256,
        max_workers=2,
        max_buffer_bytes=8_192,
    )
    outcome = ReconstructionPipeline(
        normal_sample,
        limited,
        tmp_path / "limited.json",
    ).run_realtime()
    assert outcome.error.code == ErrorCode.E_RESOURCE
    assert outcome.state.counts()["failed"] == 32


def test_single_and_multi_worker_outputs_are_identical(normal_sample, config, tmp_path):
    sequential = ReconstructionPipeline(
        normal_sample,
        config.replace(max_workers=1),
        tmp_path / "one.json",
    ).run_realtime()
    parallel = ReconstructionPipeline(
        normal_sample,
        config.replace(max_workers=4),
        tmp_path / "four.json",
    ).run_realtime()
    assert sequential.complete and parallel.complete
    assert np.array_equal(sequential.result.output, parallel.result.output)
    assert [f.fingerprint() for f in sequential.result.frames] == [
        f.fingerprint() for f in parallel.result.frames
    ]


def test_cancel_stops_at_frame_boundary(normal_sample, config, tmp_path):
    pipeline = ReconstructionPipeline(
        normal_sample,
        config,
        tmp_path / "cancel.json",
    )
    original_estimate = pipeline._estimate_indices

    def estimate_then_cancel(state, context, schedules, indices):
        result = original_estimate(state, context, schedules, indices)
        pipeline.cancel()
        return result

    pipeline._estimate_indices = estimate_then_cancel
    outcome = pipeline.run_realtime()
    assert outcome.cancelled
    assert not outcome.complete
    assert outcome.state.counts()["committed"] == 0
    assert len(pipeline.store.records) == 0


def test_out_of_bounds_frame_parameters_return_para(normal_sample, analyzed):
    _, frames, _ = analyzed
    invalid = frames[0].clone()
    invalid.local_arrival_delays_samples[0] = invalid.frame_length + 1
    with pytest.raises(WorkbenchError) as caught:
        RenderEngine().render_contribution(invalid, normal_sample.dry)
    assert caught.value.code == ErrorCode.E_PARA

