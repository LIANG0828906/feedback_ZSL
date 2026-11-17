from __future__ import annotations

import hashlib
import json
import os
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .analysis import (
    DetectionResult,
    align_channel,
    compensate_reflections,
    detect_channel,
    estimate_reflections,
    mono_channel,
)
from .dsp import master_bus
from .errors import RenderError
from .manifest import ALGORITHM_VERSION, Project, load_project
from .wavio import pcm_bytes, wav_header


SNAPSHOT_VERSION = 1
COMPENSATION_ATTENUATION_DB = -20.0


@dataclass(frozen=True)
class RenderOptions:
    output_dir: Path
    state_dir: Path | None = None
    block_size: int | None = None
    block_delay_seconds: float = 0.0
    stop_requested: Callable[[], bool] | None = None
    progress: Callable[[dict[str, Any]], None] | None = None

    @property
    def resolved_state_dir(self) -> Path:
        return self.state_dir or (self.output_dir / ".render_state")


@dataclass(frozen=True)
class RenderResult:
    status: str
    report: dict[str, Any]
    snapshot_path: Path
    bus_path: Path
    recovery_report_path: Path | None = None


def _sha256_bytes(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def _sha256_file(path: Path, length: int | None = None) -> str:
    digest = hashlib.sha256()
    remaining = length
    with path.open("rb") as handle:
        while True:
            amount = 1024 * 1024
            if remaining is not None:
                if remaining <= 0:
                    break
                amount = min(amount, remaining)
            block = handle.read(amount)
            if not block:
                break
            digest.update(block)
            if remaining is not None:
                remaining -= len(block)
    return digest.hexdigest()


def _atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    try:
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        pass


def _atomic_write_json(path: Path, value: dict[str, Any]) -> None:
    payload = (
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    ).encode("utf-8")
    _atomic_write_bytes(path, payload)


def _json_bytes(value: dict[str, Any]) -> bytes:
    return (
        json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        + "\n"
    ).encode("utf-8")


def _discard_state(state_dir: Path) -> None:
    for name in ("snapshot.json", "state.npz", "state.json"):
        path = state_dir / name
        try:
            path.unlink()
        except FileNotFoundError:
            pass


def _discard_reports(output_dir: Path) -> None:
    for name in ("render_report.json", "recovery_report.json"):
        try:
            (output_dir / name).unlink()
        except FileNotFoundError:
            pass


def _load_snapshot(state_dir: Path) -> dict[str, Any] | None:
    path = state_dir / "snapshot.json"
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (FileNotFoundError, OSError, UnicodeDecodeError, json.JSONDecodeError):
        return None
    if not isinstance(value, dict) or value.get("snapshot_version") != SNAPSHOT_VERSION:
        return None
    return value


def _validate_resume_state(snapshot: dict[str, Any] | None, state_dir: Path) -> bool:
    if snapshot is None:
        return False
    try:
        state_path = state_dir / str(snapshot["state_file"])
        analysis_path = state_dir / str(snapshot["analysis_file"])
        if _sha256_file(state_path) != snapshot["state_sha256"]:
            return False
        if _sha256_file(analysis_path) != snapshot["analysis_sha256"]:
            return False
    except (KeyError, OSError):
        return False
    return True


def _load_state(
    state_dir: Path,
) -> tuple[np.ndarray, dict[str, Any]]:
    with np.load(state_dir / "state.npz") as payload:
        pcm = np.asarray(payload["pcm"], dtype=np.int32)
    analysis = json.loads((state_dir / "state.json").read_text(encoding="utf-8"))
    return pcm, analysis


def _pan_gains(pan: float) -> tuple[float, float]:
    angle = (float(pan) + 1.0) * np.pi / 4.0
    return float(np.cos(angle)), float(np.sin(angle))


def _db(value: float, floor: float = 1e-15) -> float:
    return float(20.0 * np.log10(max(float(value), floor)))


def _build_parameters(
    project: Project,
    detections: dict[str, DetectionResult],
    reflection_estimates: dict[str, Any],
    compensation_enabled: dict[str, bool],
    bus_frames: int,
    block_size: int,
) -> dict[str, Any]:
    return {
        "algorithm_version": ALGORITHM_VERSION,
        "manifest_digest": project.digest,
        "sample_rate": project.sample_rate,
        "bit_depth": project.bit_depth,
        "bus_frames": bus_frames,
        "block_size": block_size,
        "alignment_shifts": {
            name: result.alignment_shift for name, result in detections.items()
        },
        "detection_status": {
            name: result.status for name, result in detections.items()
        },
        "reflection_delays_samples": {
            name: list(estimate.delays_samples)
            for name, estimate in reflection_estimates.items()
        },
        "reflection_gains_db": {
            name: list(estimate.gains_db)
            for name, estimate in reflection_estimates.items()
        },
        "compensation_enabled": compensation_enabled,
        "compensation_attenuation_db": COMPENSATION_ATTENUATION_DB,
        "mix": {
            spec.channel: {
                "instrument": spec.instrument,
                "weight": spec.mix_weight,
                "pan": spec.pan,
            }
            for spec in project.all_specs
        },
        "master": {
            "target_lufs": project.target_lufs,
            "true_peak_db": project.true_peak_db,
        },
    }


def _build_render_state(
    project: Project,
    block_size: int,
) -> tuple[np.ndarray, dict[str, Any], dict[str, Any]]:
    sample_rate = project.sample_rate
    ambient = mono_channel(project.wav_data[project.ambient.channel].samples)
    bus_frames = max(wav.info.frames for wav in project.wav_data.values())
    bus = np.zeros((bus_frames, 2), dtype=np.float64)
    detections: dict[str, DetectionResult] = {}
    estimates: dict[str, Any] = {}
    compensation_enabled: dict[str, bool] = {}
    channel_reports: list[dict[str, Any]] = []
    warnings: list[dict[str, Any]] = []

    for spec in project.all_specs:
        signal = mono_channel(project.wav_data[spec.channel].samples)
        if spec.channel == project.ambient.channel:
            detection = DetectionResult(
                channel=spec.channel,
                status="NOT_APPLICABLE",
                detected_samples=(),
                reference_samples=(),
                alignment_shift=0,
                residual_errors=(),
                message="ambient reference is not transient-aligned",
            )
            aligned = signal.copy()
        else:
            detection = detect_channel(spec, signal, sample_rate)
            aligned = align_channel(signal, detection.alignment_shift)
            if not detection.ok:
                warnings.append(
                    {
                        "channel": spec.channel,
                        "code": "DETECTION_FAILED",
                        "message": detection.message or "transient detection failed",
                    }
                )
        detections[spec.channel] = detection

        estimate = estimate_reflections(
            spec, aligned, ambient, project.reflection, sample_rate
        )
        estimates[spec.channel] = estimate
        enabled = bool(spec.early_reflections and spec.reflection_window is not None)
        compensation_enabled[spec.channel] = enabled
        if enabled:
            compensation = compensate_reflections(
                spec,
                aligned,
                sample_rate,
                attenuation_db=COMPENSATION_ATTENUATION_DB,
            )
            processed_signal = compensation.signal
            if (
                compensation.reduction_db < 12.0
                or compensation.peak_loss_db > 1.0
                or not compensation.outside_unchanged
            ):
                warnings.append(
                    {
                        "channel": spec.channel,
                        "code": "REFLECTION_COMPENSATION_FAILED",
                        "message": (
                            "window reduction or transient peak preservation "
                            "did not meet the acceptance threshold"
                        ),
                    }
                )
        else:
            compensation = compensate_reflections(spec, aligned, sample_rate)
            processed_signal = aligned
        left_gain, right_gain = _pan_gains(spec.pan)
        left_gain *= spec.mix_weight
        right_gain *= spec.mix_weight
        contribution = np.zeros((bus_frames, 2), dtype=np.float64)
        frames = min(processed_signal.size, bus_frames)
        contribution[:frames, 0] = processed_signal[:frames] * left_gain
        contribution[:frames, 1] = processed_signal[:frames] * right_gain
        bus += contribution
        peak = float(np.max(np.abs(contribution), initial=0.0))
        channel_report: dict[str, Any] = {
            "channel": spec.channel,
            "instrument": spec.instrument,
            "status": detection.status,
            "detected_onsets_samples": list(detection.detected_samples),
            "reference_samples": list(detection.reference_samples),
            "alignment_shift_samples": detection.alignment_shift,
            "post_alignment_errors_samples": list(detection.residual_errors),
            "max_alignment_error_samples": (
                max((abs(value) for value in detection.residual_errors), default=0)
            ),
            "reflection": {
                "compensation_enabled": enabled,
                "ambient_floor_dbfs": estimate.ambient_floor_dbfs,
                "declared_delays_samples": list(estimate.declared_delays_samples),
                "estimated_delays_samples": list(estimate.delays_samples),
                "estimated_delays_ms": list(estimate.delays_ms),
                "estimated_gains_db": list(estimate.gains_db),
                "estimation_confidence": list(estimate.confidence),
                "window_energy_reduction_db": compensation.reduction_db,
                "transient_peak_loss_db": compensation.peak_loss_db,
                "outside_window_unchanged": compensation.outside_unchanged,
            },
            "mix": {
                "weight": spec.mix_weight,
                "pan": spec.pan,
                "left_gain": left_gain,
                "right_gain": right_gain,
            },
            "peak_dbfs": _db(peak),
            "energy": float(np.sum(contribution * contribution)),
        }
        channel_reports.append(channel_report)

    bus_energy = float(np.sum(bus * bus))
    contribution_energy = sum(
        float(channel_report["energy"]) for channel_report in channel_reports
    )
    for channel_report in channel_reports:
        channel_energy = float(channel_report.pop("energy"))
        channel_report["contribution_ratio"] = (
            channel_energy / contribution_energy if contribution_energy > 0.0 else 0.0
        )
        channel_report["bus_energy_ratio"] = (
            channel_energy / bus_energy if bus_energy > 0.0 else 0.0
        )

    master = master_bus(
        bus,
        project.sample_rate,
        project.bit_depth,
        project.target_lufs,
        project.true_peak_db,
    )
    if not master.compliant:
        warnings.append(
            {
                "channel": "master",
                "code": "MASTER_COMPLIANCE_FAILED",
                "message": (
                    "integrated loudness deviation or true-peak ceiling "
                    "is outside the requested tolerance"
                ),
            }
        )
    parameters = _build_parameters(
        project,
        detections,
        estimates,
        compensation_enabled,
        bus_frames,
        block_size,
    )
    analysis = {
        "manifest_digest": project.digest,
        "manifest_summary": project.summary(),
        "warnings": warnings,
        "channels": channel_reports,
        "master": master.as_dict(),
        "bus_frames": bus_frames,
    }
    return master.pcm, analysis, parameters


def _write_state(
    state_dir: Path,
    pcm: np.ndarray,
    analysis: dict[str, Any],
) -> tuple[str, str]:
    state_path = state_dir / "state.npz"
    temporary = state_dir / "state.npz.tmp"
    with temporary.open("wb") as handle:
        np.savez(handle, pcm=pcm)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, state_path)
    analysis_payload = _json_bytes(analysis)
    _atomic_write_bytes(state_dir / "state.json", analysis_payload)
    return _sha256_file(state_path), _sha256_bytes(analysis_payload)


def _snapshot_payload(
    project: Project,
    parameters: dict[str, Any],
    total_frames: int,
    block_size: int,
    completed_blocks: int,
    status: str,
    state_sha256: str,
    analysis_sha256: str,
    output_prefix_sha256: str,
) -> dict[str, Any]:
    total_blocks = (total_frames + block_size - 1) // block_size
    return {
        "snapshot_version": SNAPSHOT_VERSION,
        "status": status,
        "manifest_digest": project.digest,
        "manifest_summary": {
            "sample_rate": project.sample_rate,
            "bit_depth": project.bit_depth,
            "seed": project.seed,
            "target_lufs": project.target_lufs,
            "true_peak_db": project.true_peak_db,
        },
        "parameters": parameters,
        "total_samples": total_frames,
        "sample_rate": project.sample_rate,
        "bit_depth": project.bit_depth,
        "block_size": block_size,
        "total_blocks": total_blocks,
        "completed_blocks": completed_blocks,
        "output_file": "bus.wav",
        "output_prefix_sha256": output_prefix_sha256,
        "state_file": "state.npz",
        "state_sha256": state_sha256,
        "analysis_file": "state.json",
        "analysis_sha256": analysis_sha256,
    }


def _check_output_prefix(
    output_path: Path,
    completed_blocks: int,
    block_size: int,
    total_frames: int,
    project: Project,
    snapshot_hash: str,
) -> bool:
    total_blocks = (total_frames + block_size - 1) // block_size
    if completed_blocks < 0 or completed_blocks > total_blocks:
        return False
    frames_done = min(total_frames, completed_blocks * block_size)
    header_size = len(
        wav_header(
            frames=total_frames,
            sample_rate=project.sample_rate,
            bit_depth=project.bit_depth,
            channels=2,
        )
    )
    expected_size = header_size + frames_done * 2 * (project.bit_depth // 8)
    try:
        if output_path.stat().st_size < expected_size:
            return False
    except OSError:
        return False
    return _sha256_file(output_path, expected_size) == snapshot_hash


def _recovery_report(
    project: Project,
    snapshot: dict[str, Any],
    snapshot_path: Path,
) -> dict[str, Any]:
    return {
        "status": "interrupted",
        "exit_code": 0,
        "message": "current block committed; resume from snapshot",
        "manifest_digest": project.digest,
        "completed_blocks": snapshot["completed_blocks"],
        "total_blocks": snapshot["total_blocks"],
        "total_samples": snapshot["total_samples"],
        "snapshot_path": str(snapshot_path),
        "alignment_shifts": snapshot["parameters"]["alignment_shifts"],
        "reflection_delays_samples": snapshot["parameters"][
            "reflection_delays_samples"
        ],
        "reflection_gains_db": snapshot["parameters"]["reflection_gains_db"],
    }


def _build_final_report(
    project: Project,
    analysis: dict[str, Any],
    snapshot: dict[str, Any],
    output_path: Path,
    snapshot_path: Path,
) -> dict[str, Any]:
    return {
        "status": "complete",
        "manifest_digest": project.digest,
        "manifest_summary": project.summary(),
        "warnings": analysis["warnings"],
        "channels": analysis["channels"],
        "master": analysis["master"],
        "block_render": {
            "completed_blocks": snapshot["completed_blocks"],
            "total_blocks": snapshot["total_blocks"],
            "block_size": snapshot["block_size"],
            "total_samples": snapshot["total_samples"],
        },
        "outputs": {
            "bus_wav": str(output_path),
            "bus_sha256": _sha256_file(output_path),
            "snapshot": str(snapshot_path),
        },
    }


def render_project(
    manifest: str | Path | Project,
    output_dir: str | Path,
    *,
    block_size: int | None = None,
    block_delay_seconds: float = 0.0,
    stop_requested: Callable[[], bool] | None = None,
    progress: Callable[[dict[str, Any]], None] | None = None,
) -> RenderResult:
    project = manifest if isinstance(manifest, Project) else load_project(manifest)
    output_root = Path(output_dir).resolve()
    output_root.mkdir(parents=True, exist_ok=True)
    _discard_reports(output_root)
    options = RenderOptions(
        output_dir=output_root,
        block_size=block_size,
        block_delay_seconds=block_delay_seconds,
        stop_requested=stop_requested,
        progress=progress,
    )
    state_dir = options.resolved_state_dir
    state_dir.mkdir(parents=True, exist_ok=True)
    selected_block_size = int(block_size or project.block_size)
    if selected_block_size < 1:
        raise RenderError("block_size must be positive")
    total_frames = max(wav.info.frames for wav in project.wav_data.values())
    total_blocks = (total_frames + selected_block_size - 1) // selected_block_size
    output_path = output_root / "bus.wav"
    snapshot_path = state_dir / "snapshot.json"

    snapshot = _load_snapshot(state_dir)
    resume_valid = (
        snapshot is not None
        and snapshot.get("manifest_digest") == project.digest
        and snapshot.get("total_samples") == total_frames
        and snapshot.get("block_size") == selected_block_size
        and _validate_resume_state(snapshot, state_dir)
    )

    if resume_valid:
        pcm, analysis = _load_state(state_dir)
        analysis_params = _build_parameters_from_analysis(
            project, analysis, total_frames, selected_block_size
        )
        if snapshot.get("parameters") != analysis_params:
            resume_valid = False
        else:
            parameters = analysis_params

    if not resume_valid:
        _discard_state(state_dir)
        pcm, analysis, parameters = _build_render_state(project, selected_block_size)
        state_sha256, analysis_sha256 = _write_state(state_dir, pcm, analysis)
        header = wav_header(
            frames=total_frames,
            sample_rate=project.sample_rate,
            bit_depth=project.bit_depth,
            channels=2,
        )
        _atomic_write_bytes(output_path, header)
        snapshot = _snapshot_payload(
            project,
            parameters,
            total_frames,
            selected_block_size,
            0,
            "rendering",
            state_sha256,
            analysis_sha256,
            _sha256_bytes(header),
        )
        _atomic_write_json(snapshot_path, snapshot)
    else:
        assert snapshot is not None
        state_sha256 = str(snapshot["state_sha256"])
        analysis_sha256 = str(snapshot["analysis_sha256"])

    completed_blocks = int(snapshot["completed_blocks"])
    if not _check_output_prefix(
        output_path,
        completed_blocks,
        selected_block_size,
        total_frames,
        project,
        str(snapshot["output_prefix_sha256"]),
    ):
        completed_blocks = 0
        header = wav_header(
            frames=total_frames,
            sample_rate=project.sample_rate,
            bit_depth=project.bit_depth,
            channels=2,
        )
        _atomic_write_bytes(output_path, header)
        snapshot = _snapshot_payload(
            project,
            parameters,
            total_frames,
            selected_block_size,
            0,
            "rendering",
            state_sha256,
            analysis_sha256,
            _sha256_bytes(header),
        )
        _atomic_write_json(snapshot_path, snapshot)

    header = wav_header(
        frames=total_frames,
        sample_rate=project.sample_rate,
        bit_depth=project.bit_depth,
        channels=2,
    )
    header_size = len(header)
    bytes_per_frame = 2 * (project.bit_depth // 8)
    interrupted = False
    with output_path.open("r+b") as output_handle:
        committed_frames = min(total_frames, completed_blocks * selected_block_size)
        output_handle.seek(header_size + committed_frames * bytes_per_frame)
        output_handle.truncate()
        for block_index in range(completed_blocks, total_blocks):
            start = block_index * selected_block_size
            end = min(start + selected_block_size, total_frames)
            block = pcm_bytes(pcm[start:end], project.bit_depth)
            output_handle.write(block)
            output_handle.flush()
            os.fsync(output_handle.fileno())
            completed_blocks = block_index + 1
            done = completed_blocks == total_blocks
            prefix_hash = _sha256_file(
                output_path,
                header_size + min(total_frames, completed_blocks * selected_block_size)
                * bytes_per_frame,
            )
            snapshot = _snapshot_payload(
                project,
                parameters,
                total_frames,
                selected_block_size,
                completed_blocks,
                "complete" if done else "rendering",
                state_sha256,
                analysis_sha256,
                prefix_hash,
            )
            _atomic_write_json(snapshot_path, snapshot)
            if progress is not None:
                progress(
                    {
                        "completed_blocks": completed_blocks,
                        "total_blocks": total_blocks,
                        "status": snapshot["status"],
                    }
                )
            if options.block_delay_seconds > 0.0:
                time.sleep(options.block_delay_seconds)
            if stop_requested is not None and stop_requested() and not done:
                interrupted = True
                break

    if interrupted:
        recovery = _recovery_report(project, snapshot, snapshot_path)
        recovery_path = output_root / "recovery_report.json"
        _atomic_write_json(recovery_path, recovery)
        return RenderResult(
            status="interrupted",
            report=recovery,
            snapshot_path=snapshot_path,
            bus_path=output_path,
            recovery_report_path=recovery_path,
        )

    report = _build_final_report(project, analysis, snapshot, output_path, snapshot_path)
    _atomic_write_json(output_root / "render_report.json", report)
    return RenderResult(
        status="complete",
        report=report,
        snapshot_path=snapshot_path,
        bus_path=output_path,
    )


def _build_parameters_from_analysis(
    project: Project,
    analysis: dict[str, Any],
    total_frames: int,
    block_size: int,
) -> dict[str, Any]:
    return {
        "algorithm_version": ALGORITHM_VERSION,
        "manifest_digest": project.digest,
        "sample_rate": project.sample_rate,
        "bit_depth": project.bit_depth,
        "bus_frames": total_frames,
        "block_size": block_size,
        "alignment_shifts": {
            item["channel"]: item["alignment_shift_samples"]
            for item in analysis["channels"]
        },
        "detection_status": {
            item["channel"]: item["status"] for item in analysis["channels"]
        },
        "reflection_delays_samples": {
            item["channel"]: item["reflection"]["estimated_delays_samples"]
            for item in analysis["channels"]
        },
        "reflection_gains_db": {
            item["channel"]: item["reflection"]["estimated_gains_db"]
            for item in analysis["channels"]
        },
        "compensation_enabled": {
            item["channel"]: item["reflection"]["compensation_enabled"]
            for item in analysis["channels"]
        },
        "compensation_attenuation_db": COMPENSATION_ATTENUATION_DB,
        "mix": {
            item["channel"]: {
                "instrument": item["instrument"],
                "weight": item["mix"]["weight"],
                "pan": item["mix"]["pan"],
            }
            for item in analysis["channels"]
        },
        "master": {
            "target_lufs": project.target_lufs,
            "true_peak_db": project.true_peak_db,
        },
    }
