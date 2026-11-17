from __future__ import annotations

import hashlib
import io
import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .audio_io import atomic_write_bytes, encode_wav_bytes, read_wav_mono
from .errors import MasterError, StructuredIssue
from .loudness import (
    finalize_loudness,
    integrated_loudness,
    sample_peak_dbfs,
    true_peak_dbtp,
)
from .manifest import (
    GROUP_DISPLAY,
    INSTRUMENT_DISPLAY,
    ChannelSpec,
    Manifest,
)
from .spatial import (
    PanParameters,
    energy_percentages,
    pan_parameters,
    render_binaural,
    source_angles,
)


SNAPSHOT_VERSION = 1


@dataclass
class InterruptFlag:
    requested: bool = False


@dataclass
class AlignmentRecord:
    spec: ChannelSpec
    source_hash: str
    onset_sample: int
    shift_samples: int
    aligned_offset_samples: int
    aligned_duration_samples: int
    source_samples: int
    waveform: np.ndarray
    padded: np.ndarray


@dataclass
class PreparedRender:
    manifest: Manifest
    alignment: list[AlignmentRecord]
    pans: list[PanParameters]
    elevations: list[float]
    binaural: list[tuple[np.ndarray, np.ndarray]]
    total_samples: int
    total_blocks: int


@dataclass
class ProgressState:
    work_dir: Path
    block_samples: int
    total_samples: int
    total_blocks: int
    completed_blocks: int = 0
    blocks: list[dict[str, Any]] = field(default_factory=list)
    mismatch_code: str | None = None
    resumed_blocks: int = 0
    rendered_blocks: int = 0

    @property
    def snapshot_path(self) -> Path:
        return self.work_dir / "snapshot.json"

    @property
    def blocks_dir(self) -> Path:
        return self.work_dir / "blocks"


def detect_onset(samples: np.ndarray) -> int:
    values = np.asarray(samples, dtype=np.float64)
    nonzero = np.flatnonzero(np.abs(values) > 1e-12)
    if nonzero.size == 0:
        raise ValueError("channel is silent")
    return int(nonzero[0])


def _resize(samples: np.ndarray, length: int) -> np.ndarray:
    if samples.size == length:
        return samples.copy()
    if samples.size > length:
        return samples[:length].copy()
    result = np.zeros(length, dtype=np.float64)
    result[: samples.size] = samples
    return result


def prepare_render(manifest: Manifest) -> PreparedRender:
    alignment: list[AlignmentRecord] = []
    pans: list[PanParameters] = []
    elevations: list[float] = []
    raw_aligned: list[np.ndarray] = []

    for spec in manifest.channels:
        info, source, source_hash = read_wav_mono(Path(spec.path))
        try:
            onset = detect_onset(source)
        except ValueError as exc:
            raise MasterError(
                StructuredIssue(
                    "E_AUDIO_READ",
                    spec.channel_id,
                    str(exc),
                    instrument=spec.instrument,
                    details={"path": spec.path_report},
                )
            ) from exc

        shift = spec.start_offset_samples - onset
        if shift >= 0:
            aligned = np.concatenate((np.zeros(shift, dtype=np.float64), source))
        else:
            aligned = source[-shift:].copy()
        if spec.end_offset_samples is not None:
            aligned = _resize(aligned, spec.end_offset_samples)
        if aligned.size == 0:
            raise MasterError(
                StructuredIssue(
                    "E_OFFSET",
                    spec.channel_id,
                    "channel has zero length after alignment",
                    instrument=spec.instrument,
                )
            )

        actual_onset = detect_onset(aligned)
        if abs(actual_onset - spec.start_offset_samples) > 1:
            raise MasterError(
                StructuredIssue(
                    "E_OFFSET",
                    spec.channel_id,
                    "declared start offset cannot be reached without changing relative timing",
                    instrument=spec.instrument,
                    details={
                        "declared": spec.start_offset_samples,
                        "actual": actual_onset,
                        "source_onset": onset,
                    },
                )
            )
        alignment.append(
            AlignmentRecord(
                spec=spec,
                source_hash=source_hash,
                onset_sample=onset,
                shift_samples=shift,
                aligned_offset_samples=actual_onset,
                aligned_duration_samples=int(aligned.size),
                source_samples=int(source.size),
                waveform=aligned,
                padded=aligned,
            )
        )
        azimuth, elevation = source_angles(spec.anchor, manifest.listener)
        pans.append(pan_parameters(azimuth, manifest.sample_rate))
        elevations.append(elevation)
        raw_aligned.append(aligned)

    total_samples = max(int(values.size) for values in raw_aligned)
    binaural: list[tuple[np.ndarray, np.ndarray]] = []
    for record, pan in zip(alignment, pans):
        padded = np.zeros(total_samples, dtype=np.float64)
        padded[: record.waveform.size] = record.waveform
        record.padded = padded
        binaural.append(render_binaural(padded, pan))

    total_blocks = (total_samples + manifest.block_samples - 1) // manifest.block_samples
    return PreparedRender(
        manifest=manifest,
        alignment=alignment,
        pans=pans,
        elevations=elevations,
        binaural=binaural,
        total_samples=total_samples,
        total_blocks=total_blocks,
    )


def _alignment_parameters(prepared: PreparedRender) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    for record, elevation, pan in zip(
        prepared.alignment, prepared.elevations, prepared.pans
    ):
        spec = record.spec
        result.append(
            {
                "channel": spec.channel_id,
                "instrument": spec.instrument,
                "instrument_key": spec.instrument_key,
                "source_sha256": record.source_hash,
                "source_onset_sample": record.onset_sample,
                "alignment_shift_samples": record.shift_samples,
                "declared_start_offset_samples": spec.start_offset_samples,
                "declared_end_offset_samples": spec.end_offset_samples,
                "aligned_offset_samples": record.aligned_offset_samples,
                "aligned_duration_samples": record.aligned_duration_samples,
                "anchor": {
                    "x": spec.anchor[0],
                    "y": spec.anchor[1],
                    "z": spec.anchor[2],
                },
                "azimuth_deg": pan.azimuth_deg,
                "elevation_deg": elevation,
                "pan": pan.pan,
                "left_gain": pan.left_gain,
                "right_gain": pan.right_gain,
                "ild_db": pan.ild_db,
                "itd_seconds": pan.itd_seconds,
                "left_delay_samples": pan.left_delay_samples,
                "right_delay_samples": pan.right_delay_samples,
                "max_ild_step_db": pan.max_ild_step_db,
            }
        )
    return result


def _canonical_json(payload: Any) -> bytes:
    return json.dumps(
        payload,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def _parameters(prepared: PreparedRender) -> tuple[list[dict[str, Any]], str]:
    values = _alignment_parameters(prepared)
    return values, hashlib.sha256(_canonical_json(values)).hexdigest()


def _remove_progress(work_dir: Path) -> None:
    snapshot = work_dir / "snapshot.json"
    if snapshot.exists():
        snapshot.unlink()
    blocks_dir = work_dir / "blocks"
    if blocks_dir.exists():
        for path in sorted(blocks_dir.glob("block_*.npy")):
            path.unlink()
    for path in sorted(work_dir.glob(".*.tmp")):
        path.unlink()


def _load_snapshot(work_dir: Path) -> dict[str, Any] | None:
    path = work_dir / "snapshot.json"
    if not path.exists():
        return None
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError(f"unreadable snapshot: {exc}") from exc
    if not isinstance(value, dict):
        raise ValueError("snapshot root is not an object")
    return value


def _snapshot_state(
    manifest: Manifest,
    block_samples: int,
    total_samples: int,
    total_blocks: int,
    completed_blocks: int,
    blocks: list[dict[str, Any]],
    parameters: list[dict[str, Any]],
    parameters_digest: str,
) -> dict[str, Any]:
    return {
        "snapshot_version": SNAPSHOT_VERSION,
        "manifest_sha256": manifest.digest,
        "parameters_digest": parameters_digest,
        "parameters": parameters,
        "block_samples": block_samples,
        "total_samples": total_samples,
        "total_blocks": total_blocks,
        "completed_blocks": completed_blocks,
        "blocks": blocks,
    }


def _write_snapshot(
    state: ProgressState,
    manifest: Manifest,
    parameters: list[dict[str, Any]],
    parameters_digest: str,
) -> None:
    payload = _snapshot_state(
        manifest=manifest,
        block_samples=state.block_samples,
        total_samples=state.total_samples,
        total_blocks=state.total_blocks,
        completed_blocks=state.completed_blocks,
        blocks=state.blocks,
        parameters=parameters,
        parameters_digest=parameters_digest,
    )
    encoded = json.dumps(
        payload, ensure_ascii=False, indent=2, allow_nan=False
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(state.snapshot_path, encoded)


def _validate_snapshot_blocks(state: ProgressState, snapshot: dict[str, Any]) -> None:
    blocks = snapshot.get("blocks")
    if not isinstance(blocks, list) or len(blocks) != state.completed_blocks:
        raise ValueError("snapshot block ledger is inconsistent")
    for index, entry in enumerate(blocks):
        if not isinstance(entry, dict) or entry.get("index") != index:
            raise ValueError("snapshot block indices are inconsistent")
        filename = entry.get("file")
        if not isinstance(filename, str) or Path(filename).name != filename:
            raise ValueError("snapshot block filename is invalid")
        path = state.blocks_dir / filename
        if not path.exists():
            raise ValueError("snapshot block file is missing")
        payload = path.read_bytes()
        digest = hashlib.sha256(payload).hexdigest()
        if digest != entry.get("sha256"):
            raise ValueError("snapshot block digest mismatch")
        array = np.load(io.BytesIO(payload), allow_pickle=False)
        if array.ndim != 2 or array.shape[1] != 2:
            raise ValueError("snapshot block has the wrong shape")
        expected_samples = min(
            state.block_samples, state.total_samples - index * state.block_samples
        )
        if array.shape[0] != expected_samples or array.dtype != np.float64:
            raise ValueError("snapshot block length or dtype is inconsistent")


def open_progress(
    work_dir: Path,
    prepared: PreparedRender,
    block_samples: int,
    *,
    fresh: bool = False,
) -> ProgressState:
    work_dir.mkdir(parents=True, exist_ok=True)
    state = ProgressState(
        work_dir=work_dir,
        block_samples=block_samples,
        total_samples=prepared.total_samples,
        total_blocks=prepared.total_blocks,
    )
    if fresh:
        _remove_progress(work_dir)
        return state

    try:
        snapshot = _load_snapshot(work_dir)
    except ValueError:
        _remove_progress(work_dir)
        state.mismatch_code = "E_RESUME_CORRUPT"
        return state
    if snapshot is None:
        return state

    _, parameters_digest = _parameters(prepared)
    mismatch = (
        snapshot.get("snapshot_version") != SNAPSHOT_VERSION
        or snapshot.get("manifest_sha256") != prepared.manifest.digest
        or snapshot.get("parameters_digest") != parameters_digest
        or snapshot.get("block_samples") != block_samples
        or snapshot.get("total_samples") != prepared.total_samples
        or snapshot.get("total_blocks") != prepared.total_blocks
    )
    if mismatch:
        _remove_progress(work_dir)
        state.mismatch_code = "E_RESUME_MISMATCH"
        return state

    completed = snapshot.get("completed_blocks")
    if not isinstance(completed, int) or isinstance(completed, bool):
        _remove_progress(work_dir)
        state.mismatch_code = "E_RESUME_CORRUPT"
        return state
    if completed < 0 or completed > prepared.total_blocks:
        _remove_progress(work_dir)
        state.mismatch_code = "E_RESUME_CORRUPT"
        return state
    state.completed_blocks = completed
    state.resumed_blocks = completed
    state.blocks = list(snapshot.get("blocks", []))
    try:
        _validate_snapshot_blocks(state, snapshot)
    except (OSError, ValueError, KeyError):
        _remove_progress(work_dir)
        state.completed_blocks = 0
        state.resumed_blocks = 0
        state.blocks = []
        state.mismatch_code = "E_RESUME_CORRUPT"
    return state


def _block_filename(index: int) -> str:
    return f"block_{index:08d}.npy"


def _encode_block(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, np.asarray(array, dtype=np.float64), allow_pickle=False)
    return buffer.getvalue()


def _block_mix(prepared: PreparedRender, start: int, stop: int) -> np.ndarray:
    output = np.zeros((stop - start, 2), dtype=np.float64)
    for record, (left, right) in zip(prepared.alignment, prepared.binaural):
        spec = record.spec
        gain = float(spec.downmix_weight) * 10.0 ** (
            prepared.manifest.group_gains_db[spec.group] / 20.0
        )
        output[:, 0] += left[start:stop] * gain
        output[:, 1] += right[start:stop] * gain
    return output


def _load_blocks(state: ProgressState) -> list[np.ndarray]:
    arrays: list[np.ndarray] = []
    for entry in state.blocks:
        path = state.blocks_dir / entry["file"]
        payload = path.read_bytes()
        if hashlib.sha256(payload).hexdigest() != entry["sha256"]:
            raise ValueError("block digest changed after snapshot validation")
        arrays.append(np.load(io.BytesIO(payload), allow_pickle=False))
    return arrays


def render_blocks(
    prepared: PreparedRender,
    state: ProgressState,
    parameters: list[dict[str, Any]],
    parameters_digest: str,
    interrupt: InterruptFlag | None = None,
) -> tuple[list[np.ndarray], bool]:
    arrays = _load_blocks(state)
    if len(arrays) != state.completed_blocks:
        raise RuntimeError("snapshot block ledger is inconsistent")
    state.blocks_dir.mkdir(parents=True, exist_ok=True)
    interrupted = False

    for index in range(state.completed_blocks, state.total_blocks):
        start = index * state.block_samples
        stop = min(prepared.total_samples, start + state.block_samples)
        mix = _block_mix(prepared, start, stop)
        payload = _encode_block(mix)
        filename = _block_filename(index)
        block_path = state.blocks_dir / filename
        atomic_write_bytes(block_path, payload)
        state.blocks.append(
            {
                "index": index,
                "file": filename,
                "samples": int(mix.shape[0]),
                "sha256": hashlib.sha256(payload).hexdigest(),
            }
        )
        state.completed_blocks = index + 1
        state.rendered_blocks += 1
        arrays.append(mix)
        _write_snapshot(state, prepared.manifest, parameters, parameters_digest)
        if interrupt is not None and interrupt.requested:
            interrupted = True
            break
    return arrays, interrupted


def _group_contribution_reports(
    prepared: PreparedRender,
) -> list[dict[str, Any]]:
    energies = {name: 0.0 for name in GROUP_DISPLAY}
    for record, (left, right) in zip(prepared.alignment, prepared.binaural):
        spec = record.spec
        gain = float(spec.downmix_weight) * 10.0 ** (
            prepared.manifest.group_gains_db[spec.group] / 20.0
        )
        energies[spec.group] += float(np.sum((left * gain) ** 2 + (right * gain) ** 2))
    total = sum(energies.values())
    result: list[dict[str, Any]] = []
    for group in ("vocal", "instrument", "ambience"):
        contribution = 100.0 * energies[group] / total if total > 0 else 0.0
        result.append(
            {
                "group": group,
                "name": GROUP_DISPLAY[group],
                "gain_db": prepared.manifest.group_gains_db[group],
                "contribution_percent": contribution,
            }
        )
    return result


def _track_reports(prepared: PreparedRender) -> list[dict[str, Any]]:
    reports: list[dict[str, Any]] = []
    for record, pan, elevation, (left, right) in zip(
        prepared.alignment,
        prepared.pans,
        prepared.elevations,
        prepared.binaural,
    ):
        spec = record.spec
        left_percent, right_percent = energy_percentages(left, right)
        left_peak, right_peak, _ = sample_peak_dbfs(np.column_stack((left, right)))
        stability = abs(pan.max_ild_step_db)
        reports.append(
            {
                "channel": spec.channel_id,
                "instrument": spec.instrument,
                "instrument_key": spec.instrument_key,
                "group": spec.group,
                "source_path": spec.path_report,
                "source_sha256": record.source_hash,
                "source_samples": record.source_samples,
                "detected_onset_sample": record.onset_sample,
                "alignment_shift_samples": record.shift_samples,
                "aligned_offset_samples": record.aligned_offset_samples,
                "declared_start_offset_samples": spec.start_offset_samples,
                "declared_end_offset_samples": spec.end_offset_samples,
                "aligned_duration_samples": record.aligned_duration_samples,
                "aligned_duration_seconds": (
                    record.aligned_duration_samples / prepared.manifest.sample_rate
                ),
                "anchor": {
                    "x": spec.anchor[0],
                    "y": spec.anchor[1],
                    "z": spec.anchor[2],
                },
                "azimuth_deg": pan.azimuth_deg,
                "elevation_deg": elevation,
                "downmix_weight": spec.downmix_weight,
                "pan": pan.pan,
                "left_gain": pan.left_gain,
                "right_gain": pan.right_gain,
                "itd_seconds": pan.itd_seconds,
                "left_delay_samples": pan.left_delay_samples,
                "right_delay_samples": pan.right_delay_samples,
                "left_energy_percent": left_percent,
                "right_energy_percent": right_percent,
                "interaural_level_difference_db": pan.ild_db,
                "max_interaural_difference_db": abs(pan.ild_db),
                "max_ild_step_db": stability,
                "spatial_stability_threshold_db": (
                    prepared.manifest.spatial_stability_threshold_db
                ),
                "spatially_stable": (
                    stability <= prepared.manifest.spatial_stability_threshold_db
                ),
                "left_peak_dbfs": left_peak,
                "right_peak_dbfs": right_peak,
            }
        )
    return reports


def _base_report(
    prepared: PreparedRender,
    state: ProgressState,
    status: str,
) -> dict[str, Any]:
    manifest = prepared.manifest
    business_codes: list[str] = []
    if state.mismatch_code:
        business_codes.append(state.mismatch_code)
    if status == "interrupted":
        business_codes.append("E_INTERRUPTED")
    return {
        "report_version": 1,
        "status": status,
        "exit_code": 0 if status in ("complete", "interrupted") else 1,
        "business_codes": business_codes,
        "manifest_sha256": manifest.digest,
        "seed": manifest.seed,
        "sample_rate": manifest.sample_rate,
        "bit_depth": manifest.bit_depth,
        "target_loudness_lufs": manifest.target_loudness_lufs,
        "true_peak_limit_dbtp": manifest.true_peak_limit_dbtp,
        "spatial_stability_threshold_db": manifest.spatial_stability_threshold_db,
        "tracks": _track_reports(prepared),
        "groups": _group_contribution_reports(prepared),
        "progress": {
            "completed_blocks": state.completed_blocks,
            "total_blocks": state.total_blocks,
            "block_samples": state.block_samples,
            "total_samples": state.total_samples,
            "resumed_blocks": state.resumed_blocks,
            "rendered_blocks": state.rendered_blocks,
        },
        "resume": {
            "manifest_sha256": manifest.digest,
            "mismatch_code": state.mismatch_code,
            "resumed_from_snapshot": state.resumed_blocks > 0,
        },
        "master": None,
        "loudness": None,
    }


def build_interrupted_report(
    prepared: PreparedRender,
    state: ProgressState,
) -> dict[str, Any]:
    return _base_report(prepared, state, "interrupted")


def _quantized_master(
    candidate: np.ndarray,
    manifest: Manifest,
) -> tuple[bytes, np.ndarray]:
    payload, decoded = encode_wav_bytes(
        candidate, manifest.sample_rate, manifest.bit_depth
    )
    limit = manifest.true_peak_limit_dbtp
    for _ in range(4):
        peak = true_peak_dbtp(decoded)
        if peak <= limit:
            break
        adjustment = 10.0 ** ((limit - 0.02 - peak) / 20.0)
        candidate = candidate * adjustment
        payload, decoded = encode_wav_bytes(
            candidate, manifest.sample_rate, manifest.bit_depth
        )
    return payload, decoded


def finalize_render(
    prepared: PreparedRender,
    state: ProgressState,
    arrays: list[np.ndarray],
    output_path: Path,
) -> dict[str, Any]:
    manifest = prepared.manifest
    if len(arrays) != state.total_blocks:
        raise RuntimeError("cannot finalize an incomplete block set")
    if arrays:
        mix = np.concatenate(arrays, axis=0)
    else:
        mix = np.zeros((0, 2), dtype=np.float64)
    if mix.shape != (prepared.total_samples, 2):
        raise RuntimeError("assembled master has the wrong shape")

    try:
        mastered, loudness_result = finalize_loudness(
            mix,
            manifest.sample_rate,
            manifest.target_loudness_lufs,
            manifest.true_peak_limit_dbtp,
        )
    except ValueError as exc:
        if "silent" in str(exc):
            raise MasterError(
                StructuredIssue(
                    "E_SILENT_MASTER",
                    "MASTER",
                    "the mixed master contains no measurable program loudness",
                )
            ) from exc
        raise

    payload, decoded = _quantized_master(mastered, manifest)
    final_loudness = integrated_loudness(decoded, manifest.sample_rate)
    final_peak = true_peak_dbtp(decoded)
    deviation = final_loudness - manifest.target_loudness_lufs
    if abs(deviation) > 0.5 or final_peak > manifest.true_peak_limit_dbtp:
        raise MasterError(
            StructuredIssue(
                "E_LOUDNESS_UNACHIEVABLE",
                "MASTER",
                "target loudness and true-peak ceiling cannot both be met",
                details={
                    "integrated_lufs": final_loudness,
                    "deviation_lu": deviation,
                    "true_peak_dbtp": final_peak,
                },
            )
        )

    from .audio_io import atomic_write_bytes

    atomic_write_bytes(output_path, payload)
    digest = hashlib.sha256(payload).hexdigest()
    left_peak, right_peak, max_peak = sample_peak_dbfs(decoded)
    limiter = loudness_result["limiter"]
    report = _base_report(prepared, state, "complete")
    report["master"] = {
        "file_name": output_path.name,
        "sample_rate": manifest.sample_rate,
        "channels": 2,
        "bit_depth": manifest.bit_depth,
        "total_samples": int(decoded.shape[0]),
        "duration_seconds": decoded.shape[0] / manifest.sample_rate,
        "bytes": len(payload),
        "sha256": digest,
        "peak_dbfs": {
            "left": left_peak,
            "right": right_peak,
            "maximum": max_peak,
        },
    }
    report["loudness"] = {
        "integrated_lufs": final_loudness,
        "target_lufs": manifest.target_loudness_lufs,
        "deviation_lu": deviation,
        "true_peak_dbtp": final_peak,
        "true_peak_limit_dbtp": manifest.true_peak_limit_dbtp,
        "within_loudness_tolerance": abs(deviation) <= 0.5,
        "below_true_peak_limit": final_peak <= manifest.true_peak_limit_dbtp,
        "limiter": {
            "applied": bool(limiter.applied),
            "triggered_samples": int(limiter.triggered_samples),
            "passes": int(limiter.passes),
            "gain_min": float(limiter.gain_min),
            "lookahead_samples": int(limiter.lookahead_samples),
            "sample_ceiling_dbfs": float(limiter.sample_ceiling_dbfs),
            "delay_compensated": True,
        },
    }
    return report


def write_report(path: Path, report: dict[str, Any]) -> None:
    def jsonable(value: Any) -> Any:
        if isinstance(value, np.generic):
            return value.item()
        if isinstance(value, dict):
            return {str(key): jsonable(item) for key, item in value.items()}
        if isinstance(value, (list, tuple)):
            return [jsonable(item) for item in value]
        return value

    payload = json.dumps(
        jsonable(report), ensure_ascii=False, indent=2, allow_nan=False
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(path, payload)


def run_render(
    manifest: Manifest,
    output_path: Path,
    report_path: Path,
    work_dir: Path,
    *,
    block_samples: int | None = None,
    fresh: bool = False,
    interrupt: InterruptFlag | None = None,
    event: Callable[[dict[str, Any]], None] | None = None,
) -> str:
    prepared = prepare_render(manifest)
    block = block_samples if block_samples is not None else manifest.block_samples
    if block <= 0:
        raise MasterError(
            StructuredIssue(
                "E_MANIFEST_SCHEMA",
                "MASTER",
                "render block size must be positive",
            )
        )
    total_blocks = (prepared.total_samples + block - 1) // block
    prepared.total_blocks = total_blocks
    state = open_progress(work_dir, prepared, block, fresh=fresh)
    if state.mismatch_code and event is not None:
        event(
            {
                "ok": True,
                "event": "resume_reset",
                "code": state.mismatch_code,
                "message": "discarded previous progress and restarted from block zero",
            }
        )
    parameters, parameters_digest = _parameters(prepared)
    arrays, interrupted = render_blocks(
        prepared, state, parameters, parameters_digest, interrupt
    )
    if interrupted:
        report = build_interrupted_report(prepared, state)
        write_report(report_path, report)
        return "interrupted"
    report = finalize_render(prepared, state, arrays, output_path)
    write_report(report_path, report)
    return "complete"
