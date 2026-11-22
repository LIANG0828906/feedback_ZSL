from __future__ import annotations

import math
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any, Sequence

import numpy as np

from .analysis import FrameResult
from .bounds import lagrange_weights
from .config import WorkbenchConfig
from .errors import ErrorCode, WorkbenchError, para_error
from .layout import validate_layout_mapping
from .model import AcousticSample


@dataclass(frozen=True)
class RenderResult:
    output: np.ndarray
    reconstructed_ir: np.ndarray
    channel_map: tuple[int, ...]
    dry_length: int
    output_channel_count: int
    conv_window_len: int
    block_size: int
    interpolation_order: int

    def to_metadata(self) -> dict[str, Any]:
        return {
            "dry_length": self.dry_length,
            "output_channels": self.output_channel_count,
            "channel_map": list(self.channel_map),
            "conv_window_len": self.conv_window_len,
            "block_size": self.block_size,
            "interpolation_order": self.interpolation_order,
            "output_samples": int(self.output.shape[1]),
        }


def _next_power_of_two(value: int) -> int:
    return 1 << max(0, int(value - 1).bit_length())


def _add_lagrange_tap(
    target: np.ndarray,
    delay_samples: float,
    amplitude: float,
    order: int,
) -> None:
    if not math.isfinite(delay_samples) or not math.isfinite(amplitude):
        return
    floor_index = int(math.floor(delay_samples))
    fraction = float(delay_samples - floor_index)
    weights, left = lagrange_weights(fraction, order)
    energy = float(np.dot(weights, weights))
    if energy > 1.0e-30:
        weights = weights * math.sqrt(abs(amplitude) / energy)
    base = floor_index + left
    for offset, weight in enumerate(weights):
        index = base + offset
        if 0 <= index < target.size:
            target[index] += float(weight)


def _late_tap_weights(count: int) -> np.ndarray:
    indices = np.arange(1, count + 1, dtype=np.float64)
    weights = np.exp(-1.7 * (indices - 1.0) / max(1.0, count - 1.0))
    return weights / float(np.sum(weights))


def reconstruct_ir(
    sample: AcousticSample,
    frame_results: Sequence[FrameResult],
) -> np.ndarray:
    if not frame_results:
        raise WorkbenchError(
            ErrorCode.STATE,
            "cannot reconstruct a sound field without committed frames",
        )
    channel_index = {channel.id: index for index, channel in enumerate(sample.layout.channels)}
    reconstructed = np.zeros_like(sample.ir, dtype=np.float64)
    for frame_result in frame_results:
        config = frame_result.config
        frame_start = frame_result.frame_index * config.analysis.hop
        late_start = frame_start + config.analysis.window_len
        late_end = frame_start + min(
            config.analysis.frame_len,
            config.analysis.hop,
        )
        for estimate in frame_result.channels:
            index = channel_index.get(estimate.channel_id)
            if index is None:
                raise WorkbenchError(
                    ErrorCode.CHAN,
                    "frame estimate references a channel outside the layout",
                    channel=estimate.channel_id,
                    frame_index=frame_result.frame_index,
                )
            target = reconstructed[index]
            _add_lagrange_tap(
                target,
                estimate.direct_delay_samples,
                estimate.direct_energy,
                config.render.interpolation_order,
            )

            early_count = 8
            early_weights = _late_tap_weights(early_count)
            early_span = max(
                1,
                config.analysis.window_len
                - config.analysis.direct_half_width
                - 1,
            )
            base = int(round(estimate.direct_delay_samples)) + config.analysis.direct_half_width
            for tap_index, weight in enumerate(early_weights):
                position = base + int(
                    early_span * float(tap_index + 1) / float(early_count + 1)
                )
                if 0 <= position < target.size:
                    sign = 1.0 if (frame_result.frame_index + index + tap_index) % 2 else -1.0
                    target[position] += sign * math.sqrt(
                        max(0.0, estimate.early_reflection_energy * float(weight))
                    )

            if late_end > late_start and estimate.reverb_energy > 0.0:
                tap_count = min(16, late_end - late_start)
                weights = _late_tap_weights(tap_count)
                for tap_index, weight in enumerate(weights):
                    position = late_start + int(
                        (tap_index + 0.5) * (late_end - late_start) / tap_count
                    )
                    if 0 <= position < target.size:
                        sign = (
                            1.0
                            if (frame_result.frame_index * 3 + index * 5 + tap_index) % 2
                            else -1.0
                        )
                        target[position] += sign * math.sqrt(
                            max(0.0, estimate.reverb_energy * float(weight))
                        )
    return reconstructed


def _ola_fft_convolve(
    dry: np.ndarray,
    kernel: np.ndarray,
    *,
    block_size: int,
    conv_window_len: int,
) -> np.ndarray:
    dry = np.asarray(dry, dtype=np.float64)
    kernel = np.asarray(kernel, dtype=np.float64)
    output = np.zeros(dry.size, dtype=np.float64)
    if dry.size == 0 or kernel.size == 0:
        return output
    for kernel_offset in range(0, kernel.size, conv_window_len):
        kernel_chunk = kernel[kernel_offset : kernel_offset + conv_window_len]
        for dry_offset in range(0, dry.size, block_size):
            dry_block = dry[dry_offset : dry_offset + block_size]
            conv_len = dry_block.size + kernel_chunk.size - 1
            nfft = _next_power_of_two(conv_len)
            transformed = np.fft.rfft(dry_block, n=nfft) * np.fft.rfft(
                kernel_chunk,
                n=nfft,
            )
            convolved = np.fft.irfft(transformed, n=nfft)[:conv_len]
            start = dry_offset + kernel_offset
            if start >= output.size:
                continue
            end = min(output.size, start + convolved.size)
            output[start:end] += convolved[: end - start]
    return output


def render_audio(
    sample: AcousticSample,
    frame_results: Sequence[FrameResult],
    config: WorkbenchConfig,
    *,
    reconstructed_ir: np.ndarray | None = None,
) -> RenderResult:
    config.validate()
    if sample.channel_count != sample.layout.count:
        raise WorkbenchError(
            ErrorCode.CHAN,
            "IR channel count does not match declared layout",
            field="ir",
            details={
                "ir_channels": sample.channel_count,
                "layout_channels": sample.layout.count,
            },
        )
    if sample.dry_length == 0:
        raise para_error("dry clip has no samples", "dry")
    mapping = validate_layout_mapping(
        sample.layout,
        config.render.channel_map,
        sample.dry.shape[0],
    )
    if reconstructed_ir is None:
        reconstructed_ir = reconstruct_ir(sample, frame_results)
    if reconstructed_ir.shape != sample.ir.shape:
        raise para_error(
            "reconstructed IR shape must match the input IR",
            "reconstructed_ir",
        )

    required_buffer = sample.dry_length + reconstructed_ir.shape[1]
    if required_buffer > config.per_thread_buffer_samples:
        raise para_error(
            "render channel exceeds the per-thread buffer limit",
            "per_thread_buffer_samples",
            details={
                "required_samples": required_buffer,
                "limit": config.per_thread_buffer_samples,
            },
        )

    output = np.zeros(
        (sample.layout.count, sample.dry_length),
        dtype=np.float64,
    )

    def render_channel(output_index: int) -> tuple[int, np.ndarray]:
        source_index = mapping[output_index]
        rendered = _ola_fft_convolve(
            sample.dry[source_index],
            reconstructed_ir[output_index],
            block_size=config.render.block_size,
            conv_window_len=config.render.conv_window_len,
        )
        return output_index, rendered

    workers = min(config.max_workers, sample.layout.count)
    if workers > 1 and sample.layout.count > 1:
        with ThreadPoolExecutor(
            max_workers=workers,
            thread_name_prefix="render-channel",
        ) as executor:
            for output_index, rendered in executor.map(
                render_channel,
                range(sample.layout.count),
            ):
                output[output_index] = rendered
    else:
        for output_index in range(sample.layout.count):
            index, rendered = render_channel(output_index)
            output[index] = rendered

    if output.shape[1] != sample.dry_length:
        raise RuntimeError("renderer violated exact dry-clip duration")
    if not np.all(np.isfinite(output)):
        raise WorkbenchError(
            ErrorCode.CLIP,
            "rendered output contains non-finite samples",
            field="output",
        )
    return RenderResult(
        output=output,
        reconstructed_ir=reconstructed_ir,
        channel_map=mapping,
        dry_length=sample.dry_length,
        output_channel_count=sample.layout.count,
        conv_window_len=config.render.conv_window_len,
        block_size=config.render.block_size,
        interpolation_order=config.render.interpolation_order,
    )
