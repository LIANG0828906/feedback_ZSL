"""Render export helpers (exact NPY plus desktop-friendly WAV/JSON)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np
from scipy.io import wavfile

from .errors import ErrorCode, WorkbenchError
from .models import RenderResult


def export_render(
    result: RenderResult,
    output_dir: str | Path,
    *,
    basename: str = "spatial_render",
) -> dict[str, str]:
    directory = Path(output_dir)
    try:
        directory.mkdir(parents=True, exist_ok=True)
        npy_path = directory / f"{basename}.npy"
        wav_path = directory / f"{basename}.wav"
        frames_path = directory / f"{basename}_frames.json"
        manifest_path = directory / f"{basename}_manifest.json"
        np.save(npy_path, result.output, allow_pickle=False)
        audio = np.ascontiguousarray(result.output.T, dtype=np.float32)
        wavfile.write(wav_path, result.sample_rate, audio)
        frame_payload = {
            "channels": list(result.channels),
            "sample_rate": result.sample_rate,
            "sample_count": int(result.output.shape[1]),
            "output_hash": result.output_hash,
            "path": result.path,
            "frames": [
                {
                    "frame_index": frame.frame_index,
                    "parameters": frame.to_dict(),
                    "energy_statistics": frame.energy_statistics(
                        result.render_stats.get(frame.frame_index)
                    ),
                    "render_statistics": (
                        result.render_stats[frame.frame_index].to_dict()
                        if frame.frame_index in result.render_stats
                        else None
                    ),
                }
                for frame in result.frames
            ],
        }
        frames_path.write_text(
            json.dumps(frame_payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        manifest = {
            "format": "spatial-render-export-v1",
            "channels": list(result.channels),
            "sample_rate": result.sample_rate,
            "sample_count": int(result.output.shape[1]),
            "output_hash": result.output_hash,
            "checkpoint_generation": result.checkpoint_generation,
            "files": {
                "npy": npy_path.name,
                "wav": wav_path.name,
                "frames": frames_path.name,
            },
        }
        manifest_path.write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        return {
            "npy": str(npy_path),
            "wav": str(wav_path),
            "frames": str(frames_path),
            "manifest": str(manifest_path),
        }
    except OSError as exc:
        raise WorkbenchError.wrap(
            ErrorCode.E_IO,
            "failed to export render",
            exc,
            stage="export",
        )

