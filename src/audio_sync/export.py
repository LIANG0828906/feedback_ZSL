from __future__ import annotations

import hashlib
import json
import os
import tempfile
import wave
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .engine import AlignmentResult
from .errors import E_PARA, SyncError


def _jsonable(value: Any) -> Any:
    if isinstance(value, np.ndarray):
        return value.tolist()
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, dict):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    return value


def _atomic_replace_bytes(path: Path, payload: bytes) -> str:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except Exception:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise
    return hashlib.sha256(payload).hexdigest()


def _layout_mapping(
    result: AlignmentResult,
    layout_name: str | None,
    channels: Iterable[str] | None,
    speakers: Iterable[str] | None,
) -> tuple[str, list[str], list[str]]:
    default_channels = list(result.layout["channels"])
    default_speakers = list(result.layout["speakers"])
    name = layout_name or str(result.layout["name"])
    output_channels = list(channels) if channels is not None else default_channels
    output_speakers = list(speakers) if speakers is not None else default_speakers
    if sorted(output_channels) != sorted(result.channels.keys()):
        raise SyncError(
            E_PARA,
            "layout must map every aligned channel exactly once",
            field="layout",
            details={"layout_channels": output_channels, "aligned_channels": list(result.channels)},
        )
    if len(output_speakers) != len(output_channels):
        raise SyncError(
            E_PARA,
            "layout speaker list length must equal channel count",
            field="layout.speakers",
        )
    return name, output_channels, output_speakers


def _interleaved(result: AlignmentResult, order: list[str], dtype: str) -> np.ndarray:
    stacked = np.column_stack([np.asarray(result.channels[channel], dtype=dtype) for channel in order])
    return np.ascontiguousarray(stacked, dtype=dtype)


def _write_wav_atomic(path: Path, samples: np.ndarray, sample_rate: int) -> str:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    os.close(fd)
    try:
        scaled = np.clip(samples, -1.0, 1.0)
        pcm = np.rint(scaled * 2_147_483_647.0).astype("<i4")
        with wave.open(temporary, "wb") as handle:
            handle.setnchannels(pcm.shape[1])
            handle.setsampwidth(4)
            handle.setframerate(sample_rate)
            handle.writeframes(pcm.tobytes(order="C"))
        with open(temporary, "rb") as handle:
            digest = hashlib.sha256(handle.read()).hexdigest()
        os.replace(temporary, path)
        return digest
    except Exception:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def export_alignment(
    result: AlignmentResult,
    output_dir: str | Path,
    *,
    formats: tuple[str, ...] = ("npy", "wav"),
    layout_name: str | None = None,
    channels: Iterable[str] | None = None,
    speakers: Iterable[str] | None = None,
    stem: str = "aligned_multichannel",
) -> dict[str, Any]:
    if not result.complete:
        raise SyncError(E_PARA, "cannot export an incomplete alignment result", field="complete")
    allowed = {"npy", "wav"}
    unknown = set(formats) - allowed
    if unknown:
        raise SyncError(E_PARA, f"unsupported export format(s): {sorted(unknown)}", field="formats")
    output_dir = Path(output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    name, order, speaker_positions = _layout_mapping(result, layout_name, channels, speakers)
    stacked = _interleaved(result, order, result.config.output_dtype)
    files: dict[str, Any] = {}

    if "npy" in formats:
        npy_path = output_dir / f"{stem}.npy"
        import io

        buffer = io.BytesIO()
        np.save(buffer, stacked, allow_pickle=False)
        digest = _atomic_replace_bytes(npy_path, buffer.getvalue())
        files["npy"] = {
            "path": str(npy_path),
            "sha256": digest,
            "shape": list(stacked.shape),
            "dtype": str(stacked.dtype),
        }
    if "wav" in formats:
        wav_path = output_dir / f"{stem}.wav"
        digest = _write_wav_atomic(wav_path, stacked, result.sample_rate)
        files["wav"] = {
            "path": str(wav_path),
            "sha256": digest,
            "channels": int(stacked.shape[1]),
            "sample_width_bytes": 4,
            "sample_rate": result.sample_rate,
        }

    slot_map = [
        {"slot": index, "channel_id": channel, "speaker": speaker_positions[index]}
        for index, channel in enumerate(order)
    ]
    sidecar = {
        "schema_version": 1,
        "session_id": result.session_id,
        "sample_rate": result.sample_rate,
        "layout": {
            "name": name,
            "channel_count": len(order),
            "channels": order,
            "speakers": speaker_positions,
            "slot_map": slot_map,
        },
        "files": files,
        "low_confidence_channels": result.low_confidence_channels,
        "low_confidence_policy": "flagged_and_reported_never_silently_accepted",
        "channel_reports": result.reports,
    }
    sidecar_path = output_dir / f"{stem}.layout.json"
    sidecar_bytes = (json.dumps(_jsonable(sidecar), indent=2, sort_keys=True) + "\n").encode("utf-8")
    sidecar_hash = _atomic_replace_bytes(sidecar_path, sidecar_bytes)

    report = {
        **result.report_dict(),
        "export": {
            "output_dir": str(output_dir),
            "layout_sidecar": {"path": str(sidecar_path), "sha256": sidecar_hash},
            "files": files,
            "slot_map": slot_map,
            "low_confidence_channels": result.low_confidence_channels,
        },
    }
    report_path = output_dir / "error_report.json"
    report_bytes = (json.dumps(_jsonable(report), indent=2, sort_keys=True) + "\n").encode("utf-8")
    report_hash = _atomic_replace_bytes(report_path, report_bytes)
    return {
        "output_dir": str(output_dir),
        "files": files,
        "layout": sidecar["layout"],
        "error_report": {"path": str(report_path), "sha256": report_hash},
        "low_confidence_channels": result.low_confidence_channels,
    }

