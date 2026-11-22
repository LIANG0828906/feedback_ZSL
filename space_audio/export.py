from __future__ import annotations

import csv
import json
import wave
from pathlib import Path
import numpy as np

from .pipeline import RunResult


def write_json_report(result: RunResult, path: str | Path, sample=None) -> Path:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(
        json.dumps(
            result.to_report(sample),
            ensure_ascii=False,
            indent=2,
            sort_keys=False,
            allow_nan=False,
        )
        + "\n",
        encoding="utf-8",
    )
    return target


def write_frame_csv(result: RunResult, path: str | Path) -> Path:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(
            [
                "frame_index",
                "channel",
                "direct_delay_samples",
                "direct_delay_s",
                "direct_energy",
                "early_reflection_energy",
                "reverb_energy",
                "drr",
                "tdoa_samples",
                "tdoa_s",
                "peak_quantization_error_s",
                "energy_truncation_error_relative",
                "tdoa_error_s",
                "convolution_interpolation_error_relative",
                "peak_quantization_bound_s",
                "energy_truncation_bound_relative",
                "tdoa_bound_s",
                "convolution_interpolation_bound_relative",
                "cumulative_peak_quantization_bound_s",
                "cumulative_energy_truncation_bound_relative",
                "cumulative_tdoa_bound_s",
                "cumulative_convolution_interpolation_bound_relative",
            ]
        )
        for frame in result.frames:
            for channel in frame.channels:
                writer.writerow(
                    [
                        frame.frame_index,
                        channel.channel_id,
                        f"{channel.direct_delay_samples:.12g}",
                        f"{channel.direct_delay_s:.12g}",
                        f"{channel.direct_energy:.12g}",
                        f"{channel.early_reflection_energy:.12g}",
                        f"{channel.reverb_energy:.12g}",
                        f"{channel.drr:.12g}",
                        f"{channel.tdoa_samples:.12g}",
                        f"{channel.tdoa_s:.12g}",
                        f"{channel.observed_errors.peak_quantization_s:.12g}",
                        f"{channel.observed_errors.energy_truncation_relative:.12g}",
                        f"{channel.observed_errors.tdoa_s:.12g}",
                        f"{channel.observed_errors.convolution_interpolation_relative:.12g}",
                        f"{frame.bounds.peak_quantization_s:.12g}",
                        f"{frame.bounds.energy_truncation_relative:.12g}",
                        f"{frame.bounds.tdoa_s:.12g}",
                        f"{frame.bounds.convolution_interpolation_relative:.12g}",
                        f"{frame.cumulative_bounds.peak_quantization_s:.12g}",
                        f"{frame.cumulative_bounds.energy_truncation_relative:.12g}",
                        f"{frame.cumulative_bounds.tdoa_s:.12g}",
                        f"{frame.cumulative_bounds.convolution_interpolation_relative:.12g}",
                    ]
                )
    return target


def write_wav(
    output: np.ndarray,
    sample_rate: int,
    path: str | Path,
) -> Path:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    channels = int(output.shape[0])
    if channels < 1:
        raise ValueError("output must contain at least one channel")
    peak = float(np.max(np.abs(output))) if output.size else 0.0
    scale = 1.0 if peak <= 1.0 else 1.0 / peak
    scaled = np.clip(output * scale, -1.0, 1.0)
    pcm = np.round(scaled.T * 32767.0).astype("<i2")
    with wave.open(str(target), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(2)
        handle.setframerate(int(sample_rate))
        handle.writeframes(pcm.tobytes(order="C"))
    return target


def write_output_npz(result: RunResult, path: str | Path) -> Path:
    if result.output is None:
        raise ValueError("result has no complete render")
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    payload = {"output": result.output}
    if result.render is not None:
        payload["reconstructed_ir"] = result.render.reconstructed_ir
    np.savez_compressed(target, **payload)
    return target


def export_bundle(
    result: RunResult,
    output_dir: str | Path,
    *,
    sample_rate: int,
    sample=None,
    prefix: str = "run",
) -> dict[str, str]:
    if not result.report_complete or result.output is None:
        raise ValueError("partial or failed run cannot produce a complete export")
    output_dir = Path(output_dir)
    exported = {
        "report_json": str(
            write_json_report(result, output_dir / f"{prefix}_report.json", sample)
        ),
        "frames_csv": str(
            write_frame_csv(result, output_dir / f"{prefix}_frames.csv")
        ),
        "output_wav": str(
            write_wav(result.output, sample_rate, output_dir / f"{prefix}_render.wav")
        ),
        "output_npz": str(
            write_output_npz(result, output_dir / f"{prefix}_render.npz")
        ),
    }
    return exported
