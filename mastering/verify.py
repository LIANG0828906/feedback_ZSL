from __future__ import annotations

import math
from pathlib import Path

from .limiter import true_peak_dbtp
from .loudness import integrated_loudness
from .manifest import load_manifest
from .wavio import inspect_wav, read_wav


def verify_master(
    manifest_path: str | Path,
    master_path: str | Path,
) -> dict:
    project = load_manifest(manifest_path)
    master = Path(master_path)
    info = inspect_wav(master)
    samples, read_info = read_wav(master)
    if read_info != info:
        raise RuntimeError("WAV header changed between inspect and read")

    expected_frames = project.output_frames
    expected_data_bytes = (
        expected_frames * info.channels * (info.bits_per_sample // 8)
    )
    header_ok = (
        info.sample_rate == project.output.sample_rate
        and info.channels == project.output.channels
        and info.bits_per_sample == project.output.bits_per_sample
        and info.frames == expected_frames
        and info.data_bytes == expected_data_bytes
    )
    true_peak = true_peak_dbtp(samples)
    loudness = integrated_loudness(samples, info.sample_rate)
    true_peak_ok = not math.isfinite(true_peak) or (
        true_peak <= project.output.true_peak_limit_dbtp + 1e-12
    )
    return {
        "header": {
            "bits_per_sample": info.bits_per_sample,
            "channels": info.channels,
            "data_bytes": info.data_bytes,
            "frames": info.frames,
            "sample_rate": info.sample_rate,
            "self_consistent": header_ok,
        },
        "integrated_loudness_lufs": (
            float(loudness) if math.isfinite(loudness) else None
        ),
        "master": str(master),
        "status": "ok" if header_ok and true_peak_ok else "error",
        "true_peak_dbtp": (
            float(true_peak) if math.isfinite(true_peak) else None
        ),
        "true_peak_limit_dbtp": project.output.true_peak_limit_dbtp,
        "true_peak_ok": true_peak_ok,
    }
