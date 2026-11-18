from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import detection_failed
from .manifest import ProjectSpec


@dataclass(frozen=True)
class ChannelAlignment:
    name: str
    raw_onsets: tuple[int, ...]
    reference_samples: tuple[int, ...]
    detected_offsets: tuple[int, ...]
    estimated_offset_samples: int
    applied_shift_samples: int
    residual_samples: tuple[int, ...]

    def as_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "raw_onsets": list(self.raw_onsets),
            "reference_samples": list(self.reference_samples),
            "detected_offsets": list(self.detected_offsets),
            "estimated_offset_samples": self.estimated_offset_samples,
            "applied_shift_samples": self.applied_shift_samples,
            "residual_samples": list(self.residual_samples),
        }

    def as_parameter(self) -> dict[str, Any]:
        return self.as_dict()


@dataclass
class AlignmentResult:
    channels: dict[str, np.ndarray]
    alignments: tuple[ChannelAlignment, ...]


def _noise_sigma(samples: np.ndarray) -> float:
    if samples.size < 8:
        return 1e-12
    differences = np.diff(np.asarray(samples, dtype=np.float64))
    median = float(np.median(differences))
    mad = float(np.median(np.abs(differences - median)))
    return max(mad / 0.6744897501960817 / math.sqrt(2.0), 1e-12)


def _shift(samples: np.ndarray, shift: int) -> np.ndarray:
    if shift == 0:
        return samples.copy()
    result = np.zeros_like(samples)
    if shift > 0:
        result[: samples.size - shift] = samples[shift:]
    else:
        result[-shift:] = samples[: samples.size + shift]
    return result


def _detect_channel_onsets(
    project: ProjectSpec,
    name: str,
    samples: np.ndarray,
) -> tuple[tuple[int, ...], tuple[int, ...]]:
    sample_rate = project.sample_rate
    reference_samples = tuple(
        int(round(value * sample_rate)) for value in project.reference_times
    )
    declared_offsets = [abs(channel.start_offset_samples) for channel in project.channels]
    search_radius = max(
        int(round(0.10 * sample_rate)),
        max(declared_offsets, default=0) + int(round(0.025 * sample_rate)),
    )
    differences = np.abs(np.diff(samples))
    sigma = _noise_sigma(samples)
    global_peak = float(np.max(differences)) if differences.size else 0.0
    threshold = max(25.0 * sigma, 0.04 * global_peak)
    if global_peak <= threshold:
        raise detection_failed(
            name,
            "no transient above the detection threshold",
            threshold=threshold,
            global_peak=global_peak,
        )

    onsets: list[int] = []
    for reference_index, reference_sample in enumerate(reference_samples):
        start = max(1, reference_sample - search_radius)
        stop = min(samples.size, reference_sample + search_radius)
        if stop - start < 2:
            raise detection_failed(
                name,
                "reference time is outside the channel",
                reference_index=reference_index,
            )
        local = np.abs(np.diff(samples[start - 1 : stop]))
        local_peak = float(np.max(local))
        if local_peak < threshold:
            raise detection_failed(
                name,
                "transient detection failed near a reference time",
                reference_index=reference_index,
                reference_sample=reference_sample,
                local_peak=local_peak,
                threshold=threshold,
            )
        crossings = np.flatnonzero(local >= threshold)
        if crossings.size == 0:
            raise detection_failed(
                name,
                "no transient crossing near a reference time",
                reference_index=reference_index,
                reference_sample=reference_sample,
            )
        onset = start + int(crossings[0])
        onsets.append(onset)
    return tuple(onsets), reference_samples


def align_channels(
    project: ProjectSpec,
    channels: dict[str, np.ndarray],
) -> AlignmentResult:
    processed: dict[str, np.ndarray] = {}
    alignments: list[ChannelAlignment] = []
    for channel in project.channels:
        samples = channels[channel.name]
        onsets, reference_samples = _detect_channel_onsets(
            project, channel.name, samples
        )
        detected_offsets = tuple(
            onset - reference
            for onset, reference in zip(onsets, reference_samples)
        )
        estimate = int(round(float(np.median(detected_offsets))))
        shift = -estimate
        aligned = _shift(samples, shift)
        residual = tuple(
            onset + shift - reference
            for onset, reference in zip(onsets, reference_samples)
        )
        if any(abs(value) > 1 for value in residual):
            raise detection_failed(
                channel.name,
                "detected transient cannot be aligned within one sample",
                estimated_offset_samples=estimate,
                residual_samples=list(residual),
            )
        processed[channel.name] = aligned
        alignments.append(
            ChannelAlignment(
                name=channel.name,
                raw_onsets=onsets,
                reference_samples=reference_samples,
                detected_offsets=detected_offsets,
                estimated_offset_samples=estimate,
                applied_shift_samples=shift,
                residual_samples=residual,
            )
        )
    return AlignmentResult(channels=processed, alignments=tuple(alignments))


def apply_alignment_parameters(
    project: ProjectSpec,
    channels: dict[str, np.ndarray],
    parameters: tuple[dict[str, Any], ...],
) -> tuple[dict[str, np.ndarray], tuple[ChannelAlignment, ...]]:
    by_name = {str(item["name"]): item for item in parameters}
    processed: dict[str, np.ndarray] = {}
    alignments: list[ChannelAlignment] = []
    for channel in project.channels:
        item = by_name.get(channel.name)
        if item is None:
            raise detection_failed(channel.name, "missing stored alignment parameter")
        shift = int(item["applied_shift_samples"])
        processed[channel.name] = _shift(channels[channel.name], shift)
        alignments.append(
            ChannelAlignment(
                name=channel.name,
                raw_onsets=tuple(int(value) for value in item["raw_onsets"]),
                reference_samples=tuple(
                    int(value) for value in item["reference_samples"]
                ),
                detected_offsets=tuple(
                    int(value) for value in item["detected_offsets"]
                ),
                estimated_offset_samples=int(item["estimated_offset_samples"]),
                applied_shift_samples=shift,
                residual_samples=tuple(
                    int(value) for value in item["residual_samples"]
                ),
            )
        )
    return processed, tuple(alignments)
