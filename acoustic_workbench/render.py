"""Reconstruction-parameter driven multichannel convolution rendering."""

from __future__ import annotations

import math
from dataclasses import dataclass, field, replace
from typing import Any, Iterable, Mapping

import numpy as np
from scipy import signal

from .codes import E_CHAN, E_META, E_PARA
from .errors import error
from .models import FrameEstimate, RenderConfig, RenderResult, RenderStats, Sample, stable_seed


@dataclass
class BlockContribution:
    block_index: int
    start: int
    channels: dict[str, np.ndarray]


@dataclass
class FrameContribution:
    frame_index: int
    blocks: list[BlockContribution] = field(default_factory=list)
    per_channel_energy: dict[str, float] = field(default_factory=dict)

    @property
    def buffer_bytes(self) -> int:
        return sum(
            array.nbytes for block in self.blocks for array in block.channels.values()
        )


def validate_render_config(config: RenderConfig) -> None:
    if (
        not isinstance(config.convolution_window, int)
        or not 16 <= config.convolution_window <= 65536
    ):
        raise error(
            E_PARA,
            "convolution_window must be an integer in [16, 65536]",
            stage="render.config",
            field="convolution_window",
            context={"value": config.convolution_window},
        )
    if (
        not isinstance(config.render_block_size, int)
        or not 16 <= config.render_block_size <= 1_048_576
    ):
        raise error(
            E_PARA,
            "render_block_size must be an integer in [16, 1048576]",
            stage="render.config",
            field="render_block_size",
            context={"value": config.render_block_size},
        )
    seen_outputs: set[str] = set()
    for output_id, input_id in config.mapping():
        if not output_id or not input_id:
            raise error(
                E_PARA,
                "channel_map entries must contain non-empty ids",
                stage="render.config",
                field="channel_map",
            )
        if output_id in seen_outputs:
            raise error(
                E_PARA,
                "channel_map output ids must be unique",
                stage="render.config",
                field="channel_map",
                context={"output": output_id},
            )
        seen_outputs.add(output_id)


def resolve_channel_mapping(
    sample: Sample,
    render_config: RenderConfig,
) -> tuple[tuple[str, str], ...]:
    validate_render_config(render_config)
    if sample.layout is None:
        raise error(
            E_META,
            "sample layout is required to render",
            stage="render.channel_map",
            field="layout",
        )
    available = set(sample.layout.channel_ids)
    mapping = render_config.mapping()
    if not mapping:
        return tuple((channel_id, channel_id) for channel_id in sample.layout.channel_ids)
    for output_id, input_id in mapping:
        if input_id not in available:
            raise error(
                E_CHAN,
                "channel map references a missing input channel",
                stage="render.channel_map",
                field="channel_map",
                channel=input_id,
                context={"output_id": output_id, "available": sorted(available)},
            )
    return tuple(mapping)


class ConvolutionRenderer:
    def __init__(self, algorithm_version: str = "acoustic-workbench/1.0.0") -> None:
        self.algorithm_version = algorithm_version

    def render(
        self,
        sample: Sample,
        frame_estimates: Iterable[FrameEstimate],
        config: RenderConfig,
    ) -> RenderResult:
        records = sorted(frame_estimates, key=lambda item: item.frame_index)
        mapping = resolve_channel_mapping(sample, config)
        dry = np.asarray(sample.dry, dtype=np.float64)
        if dry.ndim != 1 or dry.size == 0:
            raise error(
                E_META,
                "dry signal must be a non-empty mono vector",
                stage="render.input",
                field="dry",
            )
        if not records:
            raise error(
                E_PARA,
                "at least one frame estimate is required for rendering",
                stage="render.input",
                field="frame_estimates",
            )
        frame_indices = [record.frame_index for record in records]
        if len(frame_indices) != len(set(frame_indices)):
            raise error(
                E_PARA,
                "frame estimates must have unique frame indices",
                stage="render.input",
                field="frame_estimates",
            )
        expected_indices = list(range(len(records)))
        if frame_indices != expected_indices:
            raise error(
                E_PARA,
                "frame estimates must be contiguous and start at frame 0",
                stage="render.input",
                field="frame_estimates",
                context={"frame_indices": frame_indices},
            )

        plan = make_block_plan(dry.size, len(records), config.render_block_size)
        contributions = [
            self.render_frame_contribution(
                sample,
                record,
                config,
                mapping=mapping,
                block_indices=plan.frame_blocks[record.frame_index],
            )
            for record in records
        ]
        return self.assemble(sample, contributions, config, mapping=mapping)

    def prep_plan(
        self,
        sample: Sample,
        config: RenderConfig,
        total_frames: int,
    ) -> dict[int, tuple[int, ...]]:
        resolve_channel_mapping(sample, config)
        dry_length = int(np.asarray(sample.dry).size)
        plan = make_block_plan(dry_length, total_frames, config.render_block_size)
        return {frame: tuple(blocks) for frame, blocks in plan.frame_blocks.items()}

    def render_frame_contribution(
        self,
        sample: Sample,
        record: FrameEstimate,
        config: RenderConfig,
        *,
        mapping: tuple[tuple[str, str], ...] | None = None,
        block_indices: Iterable[int] | None = None,
    ) -> FrameContribution:
        mapping = mapping or resolve_channel_mapping(sample, config)
        dry = np.asarray(sample.dry, dtype=np.float64)
        block_size = config.render_block_size
        block_count = (dry.size + block_size - 1) // block_size
        if block_indices is None:
            block_indices = range(block_count)
        kernels = {
            input_id: _synthesize_kernel(
                record=record,
                channel_index=record.channel_ids.index(input_id),
                length=config.convolution_window,
                sample_rate=sample.sample_rate or 48_000,
            )
            for _, input_id in mapping
        }
        result = FrameContribution(
            frame_index=record.frame_index,
            per_channel_energy={output_id: 0.0 for output_id, _ in mapping},
        )
        for block_index in block_indices:
            block_start = block_index * block_size
            block_end = min(dry.size, block_start + block_size)
            dry_block = dry[block_start:block_end]
            block_channels: dict[str, np.ndarray] = {}
            for output_id, input_id in mapping:
                convolved = signal.fftconvolve(dry_block, kernels[input_id], mode="full")
                target_end = min(dry.size, block_start + convolved.size)
                contribution = np.ascontiguousarray(convolved[: target_end - block_start])
                block_channels[output_id] = contribution
                result.per_channel_energy[output_id] += float(
                    np.sum(contribution * contribution)
                )
            result.blocks.append(
                BlockContribution(
                    block_index=block_index,
                    start=block_start,
                    channels=block_channels,
                )
            )
        return result

    def assemble(
        self,
        sample: Sample,
        contributions: Iterable[FrameContribution],
        config: RenderConfig,
        *,
        mapping: tuple[tuple[str, str], ...] | None = None,
    ) -> RenderResult:
        mapping = mapping or resolve_channel_mapping(sample, config)
        dry_length = int(np.asarray(sample.dry).size)
        output = np.zeros((len(mapping), dry_length), dtype=np.float64)
        per_frame_energy: dict[int, dict[str, float]] = {}
        frame_blocks: dict[int, list[int]] = {}
        block_items: list[tuple[int, int, BlockContribution]] = []
        for contribution in contributions:
            per_frame_energy[contribution.frame_index] = dict(
                contribution.per_channel_energy
            )
            frame_blocks[contribution.frame_index] = [
                block.block_index for block in contribution.blocks
            ]
            for block in contribution.blocks:
                block_items.append(
                    (block.block_index, contribution.frame_index, block)
                )
        block_items.sort(key=lambda item: (item[0], item[1]))
        output_index = {output_id: index for index, (output_id, _) in enumerate(mapping)}
        for _, _, block in block_items:
            for output_id, values in block.channels.items():
                channel_index = output_index[output_id]
                end = min(output.shape[1], block.start + values.size)
                output[channel_index, block.start:end] += values[: end - block.start]
        if output.shape[1] != dry_length:
            raise error(
                E_META,
                "renderer produced a sample count different from the dry signal",
                stage="render.length_check",
                context={"expected": dry_length, "actual": int(output.shape[1])},
            )
        stats = RenderStats(
            sample_count=int(output.shape[1]),
            output_channels=tuple(output_id for output_id, _ in mapping),
            peak=float(np.max(np.abs(output))) if output.size else 0.0,
            rms=float(np.sqrt(np.mean(output * output))) if output.size else 0.0,
            per_channel_rms=tuple(
                float(np.sqrt(np.mean(channel * channel))) if channel.size else 0.0
                for channel in output
            ),
            per_frame_energy=per_frame_energy,
        )
        return RenderResult(
            output=output,
            stats=stats,
            frame_blocks={frame: tuple(blocks) for frame, blocks in frame_blocks.items()},
        )

    def render_from_persisted(
        self,
        sample: Sample,
        persisted_records: Iterable[Mapping[str, Any] | FrameEstimate],
        config: RenderConfig,
    ) -> RenderResult:
        estimates = [
            item if isinstance(item, FrameEstimate) else FrameEstimate.from_dict(dict(item))
            for item in persisted_records
        ]
        return self.render(sample, estimates, config)


def _synthesize_kernel(
    *,
    record: FrameEstimate,
    channel_index: int,
    length: int,
    sample_rate: int,
) -> np.ndarray:
    kernel = np.zeros(length, dtype=np.float64)
    local_delay = float(record.delay_in_frame[channel_index])
    onset = int(round(local_delay))
    if onset < 0:
        onset = 0
    if onset >= length:
        onset = length - 1
    kernel[onset] += 1.0

    direct_energy = max(float(record.direct_energy[channel_index]), 1e-18)
    early_ratio = min(2.0, max(0.0, float(record.early_energy[channel_index]) / direct_energy))
    early_amplitude = math.sqrt(early_ratio)
    early_offsets = (
        int(round(0.004 * sample_rate)),
        int(round(0.008 * sample_rate)),
        int(round(0.013 * sample_rate)),
        int(round(0.018 * sample_rate)),
    )
    early_weights = (0.48, 0.28, 0.16, 0.08)
    for tap_index, (offset, weight) in enumerate(zip(early_offsets, early_weights, strict=True)):
        position = onset + offset
        if position >= length:
            break
        sign = -1.0 if tap_index % 2 else 1.0
        kernel[position] += sign * early_amplitude * weight

    tail_start = min(length, onset + int(round(0.021 * sample_rate)))
    tail_length = max(0, length - tail_start)
    if tail_length:
        rng = np.random.default_rng(
            stable_seed(record.frame_index, channel_index, record.algorithm_version, "tail")
        )
        tail = rng.normal(0.0, 1.0, size=tail_length)
        tail *= np.exp(-np.arange(tail_length, dtype=np.float64) / max(1.0, tail_length / 2.5))
        tail_rms = float(np.sqrt(np.mean(tail * tail)))
        if tail_rms > 0.0:
            drr_limit = min(24.0, max(-6.0, float(record.drr_db[channel_index])))
            tail_amplitude = (10.0 ** (-drr_limit / 20.0)) / math.sqrt(max(1.0, tail_length / 64.0))
            tail *= tail_amplitude / tail_rms
        kernel[tail_start:] += tail
    return kernel


def render_config_with_mapping(
    base: RenderConfig,
    mapping: Iterable[tuple[str, str]],
) -> RenderConfig:
    return replace(base, channel_map=tuple(mapping))


@dataclass(frozen=True)
class BlockPlan:
    block_frame: tuple[int, ...]
    frame_blocks: dict[int, list[int]]


def make_block_plan(
    dry_length: int,
    total_frames: int,
    block_size: int,
) -> BlockPlan:
    block_count = (dry_length + block_size - 1) // block_size
    frame_blocks: dict[int, list[int]] = {index: [] for index in range(total_frames)}
    block_frame: list[int] = []
    for block_index in range(block_count):
        frame_index = min(
            total_frames - 1,
            (block_index * total_frames) // max(1, block_count),
        )
        block_frame.append(frame_index)
        frame_blocks[frame_index].append(block_index)
    return BlockPlan(block_frame=tuple(block_frame), frame_blocks=frame_blocks)
