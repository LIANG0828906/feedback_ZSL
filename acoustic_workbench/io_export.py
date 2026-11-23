"""Atomic audio/report export and lightweight playback helpers."""

from __future__ import annotations

import csv
import io
import json
import tempfile
from pathlib import Path
from typing import Any, Iterable

import numpy as np
from scipy.io import wavfile

from .codes import E_IO
from .errors import error
from .models import FrameEstimate, RenderResult


def _atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix=path.name, suffix=".tmp", delete=False) as tmp:
        tmp.write(payload)
        tmp.flush()
        tmp_path = Path(tmp.name)
    try:
        tmp_path.replace(path)
    except OSError:
        tmp_path.unlink(missing_ok=True)
        raise


def export_wav(
    path: str | Path,
    result: RenderResult,
    sample_rate: int,
    *,
    normalize: bool = True,
) -> Path:
    target = Path(path)
    output = np.asarray(result.output, dtype=np.float64)
    if output.ndim == 1:
        output = output[np.newaxis, :]
    peak = float(np.max(np.abs(output))) if output.size else 0.0
    scale = 1.0
    if normalize and peak > 0.0:
        scale = 0.95 / peak
    scaled = np.clip(output.T * scale, -1.0, 1.0)
    pcm = np.round(scaled * 32767.0).astype(np.int16)
    buffer = io.BytesIO()
    try:
        wavfile.write(buffer, int(sample_rate), pcm)
        _atomic_write_bytes(target, buffer.getvalue())
    except OSError as exc:
        raise error(
            E_IO,
            f"could not export WAV: {exc}",
            stage="export.wav",
            context={"path": str(target)},
        ) from exc
    return target


def export_json(path: str | Path, payload: Any) -> Path:
    target = Path(path)
    try:
        data = json.dumps(payload, indent=2, ensure_ascii=False, sort_keys=True).encode("utf-8")
        _atomic_write_bytes(target, data + b"\n")
    except OSError as exc:
        raise error(
            E_IO,
            f"could not export JSON: {exc}",
            stage="export.json",
            context={"path": str(target)},
        ) from exc
    return target


def export_frame_csv(
    path: str | Path,
    frame_estimates: Iterable[FrameEstimate],
    *,
    channel_ids: Iterable[str],
) -> Path:
    target = Path(path)
    channels = list(channel_ids)
    output = io.StringIO()
    writer = csv.writer(output)
    header = ["frame_index", "frame_start", "direction_deg", "direction_width_deg", "confidence"]
    for channel_id in channels:
        header.extend(
            [
                f"{channel_id}.delay_samples",
                f"{channel_id}.early_energy",
                f"{channel_id}.reverberant_energy",
                f"{channel_id}.drr_db",
            ]
        )
    writer.writerow(header)
    for frame in sorted(frame_estimates, key=lambda item: item.frame_index):
        row: list[Any] = [
            frame.frame_index,
            frame.frame_start,
            frame.direction_deg,
            frame.direction_width_deg,
            frame.confidence,
        ]
        for channel_id in channels:
            index = frame.channel_ids.index(channel_id)
            row.extend(
                [
                    frame.delay_samples[index],
                    frame.early_energy[index],
                    frame.reverberant_energy[index],
                    frame.drr_db[index],
                ]
            )
        writer.writerow(row)
    try:
        _atomic_write_bytes(target, output.getvalue().encode("utf-8"))
    except OSError as exc:
        raise error(
            E_IO,
            f"could not export frame CSV: {exc}",
            stage="export.csv",
            context={"path": str(target)},
        ) from exc
    return target


def temporary_wav(
    result: RenderResult,
    sample_rate: int,
    directory: str | Path,
) -> Path:
    return export_wav(Path(directory) / "audition.wav", result, sample_rate, normalize=False)
