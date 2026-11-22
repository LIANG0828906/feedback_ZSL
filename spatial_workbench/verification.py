"""End-to-end three-path and fault-injection verification."""

from __future__ import annotations

import json
import shutil
from pathlib import Path
from typing import Any

import numpy as np

from .compare import compare_render_results, compare_three_paths, require_equal
from .errors import ErrorCode, WorkbenchError
from .io import load_sample
from .models import AcousticSample, AnalysisConfig
from .pipeline import PipelineOutcome, ReconstructionPipeline
from .samplegen import SampleBundle, generate_sample_bundle
from .state import FrameState, FrameStateMachine


def _outcome_error_code(outcome: PipelineOutcome) -> str | None:
    return outcome.error.code.value if outcome.error is not None else None


def _make_missing_metadata_sample(source: AcousticSample, path: Path) -> Path:
    np.savez_compressed(
        path,
        ir=source.ir,
        dry=source.dry,
    )
    return path


def _make_corrupt_sample(path: Path) -> Path:
    path.write_bytes(b"not-an-npz\x00\x01")
    return path


def _fault_report(
    bundle: SampleBundle,
    work_dir: Path,
    config: AnalysisConfig,
) -> dict[str, Any]:
    report: dict[str, Any] = {}
    try:
        load_sample(bundle.missing_channel)
        missing_code = None
    except WorkbenchError as exc:
        missing_code = exc.code.value
    report["channel_missing"] = {
        "expected": "E_CHAN",
        "actual": missing_code,
        "ok": missing_code == "E_CHAN",
    }

    samples = {
        "time_reversal": bundle.time_reversed,
        "gain_overflow": bundle.gain_overflow,
        "bad_frame": bundle.bad_frame,
    }
    expected = {
        "time_reversal": "E_ORDER",
        "gain_overflow": "E_CLIP",
        "bad_frame": "E_FRAME",
    }
    outcomes: dict[str, PipelineOutcome] = {}
    for name, path in samples.items():
        sample = load_sample(path)
        checkpoint = work_dir / f"{name}.checkpoint.json"
        outcome = ReconstructionPipeline(sample, config, checkpoint).run_realtime()
        outcomes[name] = outcome
        report[name] = {
            "expected": expected[name],
            "actual": _outcome_error_code(outcome),
            "ok": _outcome_error_code(outcome) == expected[name],
            "state_counts": outcome.state.counts() if outcome.state else {},
        }

    bad_outcome = outcomes["bad_frame"]
    retry_sample = load_sample(bundle.bad_frame).copy_with(bad_frames=())
    retry_checkpoint = work_dir / "bad_frame.checkpoint.json"
    retry = ReconstructionPipeline(retry_sample, config, retry_checkpoint).run_recovery()
    report["bad_frame_retry"] = {
        "pre_retry_committed": (
            bad_outcome.state.counts().get("committed", 0)
            if bad_outcome.state is not None
            else None
        ),
        "pre_retry_failed": (
            bad_outcome.state.counts().get("failed", 0)
            if bad_outcome.state is not None
            else None
        ),
        "retry_complete": retry.complete,
        "retry_error": _outcome_error_code(retry),
        "ok": retry.complete,
    }
    return report


def _state_report() -> dict[str, Any]:
    machine = FrameStateMachine(1)
    original = machine.state(0)
    try:
        machine.transition(0, FrameState.COMMITTED)
        illegal_code = None
    except WorkbenchError as exc:
        illegal_code = exc.code.value
    state_after = machine.state(0)
    return {
        "initial": original.value,
        "illegal_target": FrameState.COMMITTED.value,
        "actual_error": illegal_code,
        "state_after_illegal_transition": state_after.value,
        "ok": illegal_code == "E_STATE" and state_after == original,
    }


def run_full_verification(
    sample_path: str | Path,
    work_dir: str | Path,
    *,
    config: AnalysisConfig | None = None,
    bundle: SampleBundle | None = None,
    recover_after: int | None = None,
) -> dict[str, Any]:
    directory = Path(work_dir)
    directory.mkdir(parents=True, exist_ok=True)
    cfg = config or AnalysisConfig()
    sample = load_sample(sample_path)
    if bundle is None:
        bundle = generate_sample_bundle(Path(sample_path).parent, seed=42_042)

    realtime_path = directory / "realtime.checkpoint.json"
    recovery_path = directory / "recovery.checkpoint.json"
    realtime = ReconstructionPipeline(sample, cfg, realtime_path).run_realtime()
    if not realtime.complete:
        report = {
            "ok": False,
            "stage": "realtime",
            "outcome": realtime.to_dict(include_frames=False),
        }
        (directory / "verification_report.json").write_text(
            json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        return report
    frame_count = realtime.scheduled_frames
    split = recover_after
    if split is None:
        split = max(1, min(frame_count - 1, frame_count // 3))
    partial = ReconstructionPipeline(sample, cfg, recovery_path).run_realtime(
        stop_after=split
    )
    if partial.error is not None:
        raise WorkbenchError(
            ErrorCode.E_DIFF,
            "partial realtime path failed",
            stage="verification",
            details=partial.error.to_dict(),
        )
    recovered = ReconstructionPipeline(sample, cfg, recovery_path).run_recovery()
    replayed = ReconstructionPipeline(sample, cfg, realtime_path).run_replay()
    if recovered.error is not None:
        raise recovered.error
    if replayed.error is not None:
        raise replayed.error
    assert realtime.result is not None
    assert recovered.result is not None
    assert replayed.result is not None
    require_equal(
        realtime.result,
        recovered.result,
        reference_path="realtime",
        candidate_path="recovery",
    )
    require_equal(
        realtime.result,
        replayed.result,
        reference_path="realtime",
        candidate_path="replay",
    )
    three_way = compare_three_paths(
        realtime.result,
        recovered.result,
        replayed.result,
    )

    # Input-hash mismatch must discard the entire snapshot and recompute all.
    mismatch_checkpoint = directory / "input_mismatch.checkpoint.json"
    shutil.copy2(realtime_path, mismatch_checkpoint)
    changed_dry = np.array(sample.dry, copy=True)
    changed_dry[-1] += 1.0e-6
    changed_sample = sample.copy_with(dry=changed_dry)
    mismatch = ReconstructionPipeline(
        changed_sample,
        cfg,
        mismatch_checkpoint,
    ).run_recovery()

    # Algorithm-version mismatch follows the same all-or-nothing rule.
    algorithm_checkpoint = directory / "algorithm_mismatch.checkpoint.json"
    shutil.copy2(realtime_path, algorithm_checkpoint)
    algorithm_config = cfg.replace(algorithm_version="spatial-recon-v1-verification")
    algorithm_mismatch = ReconstructionPipeline(
        sample,
        algorithm_config,
        algorithm_checkpoint,
    ).run_recovery()

    # First-difference localization is checked independently.
    mutated = np.array(realtime.result.output, copy=True)
    mutated[0, 123] += 1.0e-9
    from .models import RenderResult

    mutated_result = RenderResult(
        output=mutated,
        channels=realtime.result.channels,
        sample_rate=realtime.result.sample_rate,
        frames=realtime.result.frames,
        render_stats=realtime.result.render_stats,
        path="mutated",
    )
    first_diff = compare_render_results(realtime.result, mutated_result)

    meta_path = _make_missing_metadata_sample(sample, directory / "missing_metadata.npz")
    decode_path = _make_corrupt_sample(directory / "corrupt.npz")
    error_checks: dict[str, Any] = {}
    for name, path, expected_code in (
        ("missing_metadata", meta_path, "E_META"),
        ("corrupt_sample", decode_path, "E_DECODE"),
    ):
        try:
            load_sample(path)
            actual = None
        except WorkbenchError as exc:
            actual = exc.code.value
        error_checks[name] = {
            "expected": expected_code,
            "actual": actual,
            "ok": actual == expected_code,
        }

    fault_report = _fault_report(bundle, directory, cfg)
    state_report = _state_report()
    output_match = bool(
        np.array_equal(realtime.result.output, recovered.result.output)
        and np.array_equal(realtime.result.output, replayed.result.output)
    )
    params_match = bool(
        [frame.fingerprint() for frame in realtime.result.frames]
        == [frame.fingerprint() for frame in recovered.result.frames]
        == [frame.fingerprint() for frame in replayed.result.frames]
    )
    stats_match = bool(
        realtime.result.stats_dict()
        == recovered.result.stats_dict()
        == replayed.result.stats_dict()
    )
    checks = {
        "three_way_equal": three_way.equal,
        "output_sample_exact": output_match,
        "parameters_exact": params_match,
        "energy_statistics_exact": stats_match,
        "first_diff_localized": (
            not first_diff.equal
            and first_diff.first_sample == 123
            and first_diff.first_frame is not None
        ),
        "input_hash_invalidates_whole_snapshot": bool(
            mismatch.checkpoint_invalidated and mismatch.complete
        ),
        "algorithm_version_invalidates_whole_snapshot": bool(
            algorithm_mismatch.checkpoint_invalidated and algorithm_mismatch.complete
        ),
        "state_machine_illegal_transition": state_report["ok"],
        "metadata_error": error_checks["missing_metadata"]["ok"],
        "decode_error": error_checks["corrupt_sample"]["ok"],
        "fault_matrix": all(
            value.get("ok", False)
            for key, value in fault_report.items()
            if isinstance(value, dict)
        ),
    }
    report = {
        "ok": all(checks.values()),
        "sample": str(Path(sample_path)),
        "frames": frame_count,
        "sample_count": int(realtime.result.output.shape[1]),
        "channels": list(realtime.result.channels),
        "checkpoint": {
            "realtime_generation": realtime.checkpoint_generation,
            "recovery_generation": recovered.checkpoint_generation,
            "replay_generation": replayed.checkpoint_generation,
            "recovery_split_frame": split,
        },
        "hashes": {
            "realtime": realtime.result.output_hash,
            "recovered": recovered.result.output_hash,
            "replayed": replayed.result.output_hash,
        },
        "truth": realtime.truth,
        "three_way": three_way.to_dict(),
        "first_difference_probe": first_diff.to_dict(),
        "input_hash_mismatch": {
            "invalidated": mismatch.checkpoint_invalidated,
            "complete": mismatch.complete,
            "error": _outcome_error_code(mismatch),
        },
        "algorithm_mismatch": {
            "invalidated": algorithm_mismatch.checkpoint_invalidated,
            "complete": algorithm_mismatch.complete,
            "error": _outcome_error_code(algorithm_mismatch),
        },
        "state_machine": state_report,
        "metadata_decode_errors": error_checks,
        "faults": fault_report,
        "checks": checks,
    }
    (directory / "verification_report.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return report

