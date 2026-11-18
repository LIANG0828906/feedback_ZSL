from __future__ import annotations

import hashlib
import json
import math
import os
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np
import soundfile as sf

from .binaural import (
    BinauralState,
    interpolation_report,
    prepare_binaural_state,
    rate_report,
    render_source_block,
    tracking_report,
)
from .errors import (
    EXIT_RENDER,
    EXIT_RESUME_DIVERGENCE,
    EXIT_RESUME_MISMATCH,
    BusinessError,
)
from .leakage import LeakageEstimate, suppress_all_actors
from .loudness import (
    MasterResult,
    integrated_loudness_lufs,
    master_bus,
    true_peak_dbtp,
)
from .project import Project, load_project, project_digest, read_track
from .snapshot import SnapshotProgress, SnapshotStore


@dataclass
class PreparedRender:
    project: Project
    tracks: dict[str, np.ndarray]
    leakage: dict[str, LeakageEstimate]
    leakage_reports: list[dict[str, Any]]
    leakage_digest: str
    binaural: BinauralState
    project_digest: str
    total_frames: int
    total_blocks: int

    def render_block(self, start: int, end: int) -> tuple[np.ndarray, list[float]]:
        mix = np.zeros((end - start, 2), dtype=np.float64)
        energy: list[float] = []
        for track in self.project.mixed_tracks:
            if track.source_type == "actor":
                source = self.leakage[track.name].suppressed
            else:
                source = self.tracks[track.name]
            contribution = render_source_block(
                source,
                self.binaural.parameters[track.name],
                start,
                end,
                self.project.sample_rate,
                self.project.downmix[track.name],
            )
            mix += contribution
            energy.append(float(np.sum(contribution * contribution, dtype=np.float64)))
        return mix, energy


def _leakage_identity(estimates: dict[str, LeakageEstimate]) -> str:
    payload = []
    for name in sorted(estimates):
        estimate = estimates[name]
        payload.append(
            {
                "channel": name,
                "delay_samples": int(estimate.delay_samples),
                "gain_hex": float(estimate.gain_linear).hex(),
                "pause_samples": round(estimate.pause_seconds, 12),
            }
        )
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def prepare_render(project: Project) -> PreparedRender:
    tracks = {track.name: read_track(track) for track in project.tracks}
    total_frames = max(track.size for track in tracks.values())
    leakage = suppress_all_actors(project, tracks)
    binaural = prepare_binaural_state(project, total_frames)
    leakage_reports = [leakage[track.name].report() for track in project.tracks if track.name in leakage]
    total_blocks = int(math.ceil(total_frames / project.block_frames))
    return PreparedRender(
        project=project,
        tracks=tracks,
        leakage=leakage,
        leakage_reports=leakage_reports,
        leakage_digest=_leakage_identity(leakage),
        binaural=binaural,
        project_digest=project_digest(project),
        total_frames=total_frames,
        total_blocks=total_blocks,
    )


def _first_differences(
    expected: np.ndarray,
    actual: np.ndarray,
    block_index: int,
    limit: int = 12,
) -> list[dict[str, Any]]:
    differences: list[dict[str, Any]] = []
    channel_names = ("left", "right")
    for channel in range(min(2, expected.shape[1], actual.shape[1])):
        mismatch = np.flatnonzero(expected[:, channel] != actual[:, channel])
        for sample in mismatch[:limit]:
            differences.append(
                {
                    "block": int(block_index),
                    "channel": channel_names[channel],
                    "sample": int(sample),
                    "expected": float(expected[sample, channel]),
                    "actual": float(actual[sample, channel]),
                }
            )
            if len(differences) >= limit:
                return differences
    if not differences:
        differences.append(
            {
                "block": int(block_index),
                "channel": "left",
                "sample": 0,
                "reason": "shape",
                "expected_shape": list(expected.shape),
                "actual_shape": list(actual.shape),
            }
        )
    return differences


def _verify_committed_prefix(
    prepared: PreparedRender,
    progress: SnapshotProgress,
    store: SnapshotStore,
) -> tuple[np.ndarray, list[float]]:
    stored_blocks = store.read_blocks(progress)
    recomputed_energy = [0.0] * len(prepared.project.mixed_tracks)
    for index, stored in enumerate(stored_blocks):
        start = index * prepared.project.block_frames
        end = min(prepared.total_frames, start + prepared.project.block_frames)
        expected, energy = prepared.render_block(start, end)
        if expected.shape != stored.shape or not np.array_equal(expected, stored):
            differences = _first_differences(expected, stored, index)
            raise BusinessError(
                "E_RESUME_DIVERGENCE",
                "master",
                "committed snapshot blocks differ from deterministic re-render",
                EXIT_RESUME_DIVERGENCE,
                {"differences": differences},
            )
        for channel_index, value in enumerate(energy):
            recomputed_energy[channel_index] += value
    for index, (expected, actual) in enumerate(
        zip(progress.contribution_energy, recomputed_energy)
    ):
        if expected != actual:
            raise BusinessError(
                "E_RESUME_DIVERGENCE",
                f"contribution_{index}",
                "committed snapshot contribution energy differs from re-render",
                EXIT_RESUME_DIVERGENCE,
                {
                    "differences": [
                        {
                            "block": max(0, progress.completed_blocks - 1),
                            "channel": f"contribution_{index}",
                            "expected": expected,
                            "actual": actual,
                        }
                    ]
                },
            )
    if stored_blocks:
        partial = np.concatenate(stored_blocks, axis=0)
    else:
        partial = np.empty((0, 2), dtype=np.float64)
    return partial, recomputed_energy


def _atomic_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    encoded = (
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n"
    ).encode("utf-8")
    with temp.open("wb") as handle:
        handle.write(encoded)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temp, path)


def _master_bit_depth(project: Project) -> int:
    render = project.raw.get("render", {})
    if not isinstance(render, dict):
        render = {}
    bits = render.get("master_bit_depth", project.raw.get("master_bit_depth", 24))
    try:
        bits = int(bits)
    except (TypeError, ValueError):
        bits = 24
    return bits if bits in {16, 24, 32} else 24


def _subtype_from_project(project: Project) -> str:
    return {16: "PCM_16", 24: "PCM_24", 32: "PCM_32"}[_master_bit_depth(project)]


def _write_master_atomic(path: Path, data: np.ndarray, sample_rate: int, subtype: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.stem}.tmp-{os.getpid()}{path.suffix}")
    sf.write(
        str(temp),
        data,
        sample_rate,
        subtype=subtype,
        format="WAV",
        endian="LITTLE",
    )
    os.replace(temp, path)


def _quantized_metrics(path: Path, sample_rate: int) -> tuple[float, float, float]:
    data, actual_rate = sf.read(str(path), dtype="float64", always_2d=True)
    if actual_rate != sample_rate:
        raise BusinessError(
            "E_MASTER_SAMPLE_RATE",
            "master",
            "written master sample rate changed unexpectedly",
            EXIT_RENDER,
        )
    loudness = integrated_loudness_lufs(data, sample_rate)
    peak = true_peak_dbtp(data, sample_rate)
    peak_dbfs = float(20.0 * np.log10(np.max(np.abs(data)) + 1.0e-30))
    return loudness, peak, peak_dbfs


def _safe_relative(path: Path, root: Path) -> str:
    try:
        return path.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return path.name


def build_report(
    prepared: PreparedRender,
    master: MasterResult,
    master_path: Path,
    report_root: Path,
    master_bytes_sha256: str,
    quantized_loudness: float,
    quantized_peak: float,
    quantized_peak_dbfs: float,
    contribution_energy: list[float],
    resumed_blocks: int,
    total_blocks: int,
    snapshot_verified: bool,
) -> dict[str, Any]:
    project = prepared.project
    injected = set(project.leakage_injection_channels)
    for estimate in prepared.leakage.values():
        if estimate.channel in injected and estimate.reduction_db < 12.0:
            raise BusinessError(
                "E_LEAKAGE_REDUCTION",
                estimate.channel,
                f"leakage-band reduction {estimate.reduction_db:.3f} dB is below 12 dB",
                EXIT_RENDER,
            )
        if estimate.direct_peak_loss_db > 1.0:
            raise BusinessError(
                "E_DIRECT_PEAK_LOSS",
                estimate.channel,
                f"direct peak loss {estimate.direct_peak_loss_db:.3f} dB exceeds 1 dB",
                EXIT_RENDER,
            )
    checks, maximum_error = interpolation_report(project, prepared.binaural)
    rate = rate_report(project, prepared.binaural)
    if rate["max_ild_rate_db_per_s"] > project.max_ild_rate_db_per_s + 1.0e-7:
        raise BusinessError(
            "E_ILD_RATE_LIMIT",
            "head_tracking",
            "rendered ILD rate exceeds the declared limit",
            EXIT_RENDER,
        )
    if rate["max_itd_rate_us_per_s"] > project.max_itd_rate_us_per_s + 1.0e-5:
        raise BusinessError(
            "E_ITD_RATE_LIMIT",
            "head_tracking",
            "rendered ITD rate exceeds the declared limit",
            EXIT_RENDER,
        )
    expected_checks = [check for check in checks if check["expected_azimuth_deg"] is not None]
    if expected_checks and maximum_error > project.azimuth_tolerance_deg + 1.0e-7:
        raise BusinessError(
            "E_INTERPOLATION_TOLERANCE",
            "head_tracking",
            f"maximum azimuth error {maximum_error:.6f} deg exceeds "
            f"{project.azimuth_tolerance_deg:.6f} deg",
            EXIT_RENDER,
            {"max_azimuth_error_deg": maximum_error},
        )
    total_energy = float(sum(contribution_energy))
    contributions = []
    for track, energy in zip(project.mixed_tracks, contribution_energy):
        contributions.append(
            {
                "channel": track.name,
                "source_type": track.source_type,
                "energy": float(energy),
                "contribution_ratio": (
                    round(float(energy / total_energy), 9) if total_energy > 0 else 0.0
                ),
            }
        )
    master_display = _safe_relative(master_path, report_root)
    return {
        "status": "ok",
        "operation": "render",
        "project_id": project.project_id,
        "project_digest": prepared.project_digest,
        "sample_rate": project.sample_rate,
        "frames": int(prepared.total_frames),
        "master": {
            "path": master_display,
            "sha256": master_bytes_sha256,
            "format": "WAV",
            "bit_depth": _master_bit_depth(project),
            "subtype": _subtype_from_project(project),
            "frames": int(prepared.total_frames),
            "channels": 2,
            "integrated_loudness_lufs": round(float(quantized_loudness), 6),
            "target_loudness_lufs": project.target_loudness_lufs,
            "loudness_deviation_lu": round(
                float(quantized_loudness - project.target_loudness_lufs), 6
            ),
            "true_peak_dbtp": round(float(quantized_peak), 6),
            "true_peak_limit_dbtp": project.true_peak_dbtp,
            "sample_peak_dbfs": round(float(quantized_peak_dbfs), 6),
            "limiter": {
                "applied": master.limiter_applied,
                "triggered_samples": int(master.limiter_triggered_samples),
                "cumulative_triggered_samples": int(
                    master.limiter_cumulative_triggered_samples
                ),
                "passes": int(master.limiter_passes),
                "lookahead_samples": int(master.limiter_lookahead_samples),
                "latency_compensated": True,
                "maximum_reduction_db": round(
                    float(master.maximum_limiter_reduction_db), 6
                ),
            },
        },
        "leakage": prepared.leakage_reports,
        "binaural": {
            "head_model": {
                "kind": "analytic_woodworth_ild_itd",
                "head_radius_m": project.head_radius_m,
                "speed_of_sound_m_s": project.speed_of_sound_m_s,
                "max_ild_db": project.max_ild_db,
                "orientation_convention": project.orientation_convention,
            },
            "tracking": tracking_report(project, prepared.binaural),
            "rate": rate,
            "interpolation": {
                "checks": checks,
                "max_azimuth_error_deg": round(float(maximum_error), 9),
                "tolerance_deg": project.azimuth_tolerance_deg,
            },
        },
        "mix": {
            "bus_frames": int(prepared.total_frames),
            "bus_peak_dbfs": round(float(quantized_peak_dbfs), 6),
            "bus_true_peak_dbtp": round(float(quantized_peak), 6),
            "contributions": contributions,
        },
        "resume": {
            "resumed_blocks": int(resumed_blocks),
            "total_blocks": int(total_blocks),
            "snapshot_verified": bool(snapshot_verified),
            "block_frames": project.block_frames,
        },
    }


@dataclass
class RenderOutcome:
    status: str
    report: dict[str, Any]
    master_path: Path | None = None
    recovery_path: Path | None = None


def _recovery_report(
    prepared: PreparedRender,
    progress: SnapshotProgress | None,
    resumed_blocks: int,
) -> dict[str, Any]:
    completed = progress.completed_blocks if progress is not None else 0
    return {
        "status": "interrupted",
        "operation": "render",
        "project_id": prepared.project.project_id,
        "project_digest": prepared.project_digest,
        "completed_blocks": int(completed),
        "total_blocks": int(prepared.total_blocks),
        "next_block": int(completed),
        "block_frames": prepared.project.block_frames,
        "snapshot_verified": bool(resumed_blocks > 0),
        "leakage": prepared.leakage_reports,
        "pan_summary": prepared.binaural.summary,
        "resume": {
            "state": "committed snapshot",
            "resumed_blocks": int(resumed_blocks),
        },
    }


def execute_render(
    project_path: str | Path,
    master_path: str | Path,
    report_path: str | Path,
    state_dir: str | Path,
    *,
    fresh: bool = False,
    interrupt_requested: Callable[[], bool] | None = None,
    block_delay_s: float = 0.0,
) -> RenderOutcome:
    project = load_project(project_path, validate_audio=True)
    prepared = prepare_render(project)
    master_path = Path(master_path)
    report_path = Path(report_path)
    recovery_path = report_path.with_name(f"{report_path.stem}.recovery.json")
    try:
        recovery_path.unlink()
    except FileNotFoundError:
        pass
    state_dir = Path(state_dir)
    store = SnapshotStore(state_dir)
    if fresh:
        store.clear()

    progress = store.load()
    resumed_blocks = 0
    partial = np.empty((0, 2), dtype=np.float64)
    contribution_energy = [0.0] * len(project.mixed_tracks)
    previous_blocks: list[dict[str, Any]] = []
    if progress is not None:
        if (
            progress.project_digest != prepared.project_digest
            or progress.leakage_digest != prepared.leakage_digest
            or progress.pan_digest != prepared.binaural.parameter_digest
            or progress.total_frames != prepared.total_frames
            or progress.block_frames != project.block_frames
            or progress.total_blocks != prepared.total_blocks
            or len(progress.contribution_energy) != len(project.mixed_tracks)
            or progress.leakage_estimates != prepared.leakage_reports
            or progress.pan_summary != prepared.binaural.summary
        ):
            store.clear()
            raise BusinessError(
                "E_RESUME_MISMATCH",
                "snapshot",
                "snapshot project, leakage, or pan parameters do not match current inputs",
                EXIT_RESUME_MISMATCH,
                {
                    "expected_project_digest": prepared.project_digest,
                    "snapshot_project_digest": progress.project_digest,
                    "expected_leakage_digest": prepared.leakage_digest,
                    "snapshot_leakage_digest": progress.leakage_digest,
                    "expected_pan_digest": prepared.binaural.parameter_digest,
                    "snapshot_pan_digest": progress.pan_digest,
                },
            )
        partial, contribution_energy = _verify_committed_prefix(
            prepared, progress, store
        )
        resumed_blocks = progress.completed_blocks
        previous_blocks = list(progress.blocks)

    def interrupted() -> bool:
        return bool(interrupt_requested and interrupt_requested())

    if interrupted() and previous_blocks:
        report = _recovery_report(prepared, progress, resumed_blocks)
        _atomic_write_json(recovery_path, report)
        return RenderOutcome(
            status="interrupted",
            report=report,
            recovery_path=recovery_path,
        )

    current_progress: SnapshotProgress | None = progress
    start_block = len(previous_blocks)
    for block_index in range(start_block, prepared.total_blocks):
        if interrupted():
            break
        start = block_index * project.block_frames
        end = min(prepared.total_frames, start + project.block_frames)
        block, energy = prepared.render_block(start, end)
        for index, value in enumerate(energy):
            contribution_energy[index] += value
        current_progress = store.commit_block(
            block_index=block_index,
            block=block,
            total_blocks=prepared.total_blocks,
            total_frames=prepared.total_frames,
            block_frames=project.block_frames,
            project_digest=prepared.project_digest,
            leakage_digest=prepared.leakage_digest,
            pan_digest=prepared.binaural.parameter_digest,
            contribution_energy=contribution_energy,
            leakage_estimates=prepared.leakage_reports,
            pan_summary=prepared.binaural.summary,
            previous_blocks=previous_blocks,
        )
        previous_blocks = list(current_progress.blocks)
        if block_delay_s > 0:
            time.sleep(block_delay_s)

    if interrupted() or (
        current_progress is not None
        and current_progress.completed_blocks < prepared.total_blocks
    ):
        report = _recovery_report(prepared, current_progress, resumed_blocks)
        _atomic_write_json(recovery_path, report)
        return RenderOutcome(
            status="interrupted",
            report=report,
            recovery_path=recovery_path,
        )

    if current_progress is None:
        raise BusinessError("E_EMPTY_RENDER", "master", "no blocks were rendered", EXIT_RENDER)
    if previous_blocks:
        stored = store.read_blocks(current_progress)
        mix = np.concatenate(stored, axis=0)
    else:
        mix = partial
    if mix.shape[0] != prepared.total_frames:
        raise BusinessError(
            "E_BUS_LENGTH",
            "master",
            "rendered bus length does not equal the longest input channel",
            EXIT_RENDER,
        )

    master = master_bus(
        mix,
        project.sample_rate,
        project.target_loudness_lufs,
        project.true_peak_dbtp,
        project.limiter_lookahead_ms,
        project.limiter_release_ms,
    )
    _write_master_atomic(
        master_path,
        master.signal,
        project.sample_rate,
        _subtype_from_project(project),
    )
    quantized_loudness, quantized_peak, quantized_peak_dbfs = _quantized_metrics(
        master_path, project.sample_rate
    )
    if abs(quantized_loudness - project.target_loudness_lufs) > 0.5 + 1.0e-7:
        raise BusinessError(
            "E_LOUDNESS_TARGET_UNREACHABLE",
            "master",
            "quantized master loudness is outside the 0.5 LU tolerance",
            EXIT_RENDER,
        )
    if quantized_peak > project.true_peak_dbtp + 1.0e-7:
        raise BusinessError(
            "E_TRUE_PEAK_EXCEEDED",
            "master",
            "quantized master exceeds the true-peak limit",
            EXIT_RENDER,
        )
    if interrupted():
        report = _recovery_report(prepared, current_progress, resumed_blocks)
        _atomic_write_json(recovery_path, report)
        return RenderOutcome(
            status="interrupted",
            report=report,
            recovery_path=recovery_path,
        )
    master_hash = hashlib.sha256(master_path.read_bytes()).hexdigest()
    report = build_report(
        prepared,
        master,
        master_path,
        report_path.resolve().parent,
        master_hash,
        quantized_loudness,
        quantized_peak,
        quantized_peak_dbfs,
        contribution_energy,
        resumed_blocks,
        prepared.total_blocks,
        snapshot_verified=resumed_blocks > 0,
    )
    _atomic_write_json(report_path, report)
    if interrupted():
        recovery = _recovery_report(prepared, current_progress, resumed_blocks)
        _atomic_write_json(recovery_path, recovery)
        return RenderOutcome(
            status="interrupted",
            report=recovery,
            recovery_path=recovery_path,
        )
    return RenderOutcome(
        status="ok",
        report=report,
        master_path=master_path,
    )

