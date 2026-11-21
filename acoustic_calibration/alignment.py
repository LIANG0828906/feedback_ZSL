"""Direct-sound detection, whole-sample alignment and gain normalization."""

from __future__ import annotations

from typing import Any

import numpy as np

from .models import AlignmentParams, AlignmentResult, ChannelAlignment
from .wavio import WavData

_MAD_TO_SIGMA = 1.0 / 0.6744897501960817


def _detect_channel(
    samples: np.ndarray,
    channel_index: int,
    channel_name: str,
    sample_rate: int,
    config: AlignmentParams,
) -> ChannelAlignment:
    absolute = np.abs(samples)
    noise_samples = max(16, int(round(sample_rate * config.threshold.noise_window_ms / 1000.0)))
    leading = absolute[: min(noise_samples, absolute.size)]
    if leading.size:
        noise_floor = float(np.median(leading) * _MAD_TO_SIGMA)
    else:
        noise_floor = 0.0

    peak = float(np.max(absolute)) if absolute.size else 0.0
    floor_linear = float(10.0 ** (config.threshold.floor_dbfs / 20.0))
    threshold = max(
        floor_linear,
        noise_floor * float(config.threshold.noise_multiplier),
        peak * float(config.threshold.peak_fraction),
    )
    candidates = np.flatnonzero(absolute > threshold)
    found = bool(candidates.size)
    if found:
        arrival = int(candidates[0])
    else:
        arrival = int(np.argmax(absolute)) if absolute.size else 0

    direct_window_end = min(samples.size, arrival + max(1, int(round(sample_rate * 0.001))))
    direct_peak = float(np.max(absolute[arrival:direct_window_end])) if arrival < samples.size else 0.0
    if direct_peak <= 0.0:
        direct_peak = float(absolute[arrival]) if absolute.size else 0.0

    return ChannelAlignment(
        channel_index=channel_index,
        channel_name=channel_name,
        arrival_sample=arrival,
        threshold=float(threshold),
        noise_floor=float(noise_floor),
        direct_peak=direct_peak,
        offset=0,
        gain=1.0,
        found=found,
    )


def detect_arrivals(wav: WavData, params: AlignmentParams | None = None) -> AlignmentResult:
    """Detect the first threshold crossing in every channel.

    The threshold is channel-adaptive: it uses a robust leading-noise estimate,
    an absolute dBFS floor and a fraction of the channel peak.
    """

    config = params or AlignmentParams()
    if config.reference_channel >= wav.channel_count:
        raise ValueError("reference_channel is outside the WAV channel range")
    detected = [
        _detect_channel(wav.channels[index], index, wav.channel_names[index], wav.sample_rate, config)
        for index in range(wav.channel_count)
    ]
    reference = detected[config.reference_channel]
    reference_peak = reference.direct_peak
    channels: list[ChannelAlignment] = []
    for item in detected:
        gain = reference_peak / item.direct_peak if item.direct_peak > 0.0 else 1.0
        channels.append(
            ChannelAlignment(
                channel_index=item.channel_index,
                channel_name=item.channel_name,
                arrival_sample=item.arrival_sample,
                threshold=item.threshold,
                noise_floor=item.noise_floor,
                direct_peak=item.direct_peak,
                offset=item.arrival_sample - reference.arrival_sample,
                gain=float(gain),
                found=item.found,
            )
        )
    return AlignmentResult(
        reference_channel=config.reference_channel,
        reference_arrival=reference.arrival_sample,
        channels=tuple(channels),
    )


def _shift_to_reference(samples: np.ndarray, offset: int) -> np.ndarray:
    """Shift a channel so its direct sound lands at the reference index."""

    if offset == 0:
        return samples.copy()
    output = np.zeros_like(samples)
    if offset > 0:
        if offset < samples.size:
            output[:-offset] = samples[offset:]
    elif -offset < samples.size:
        output[-offset:] = samples[: samples.size + offset]
    return output


def apply_alignment(wav: WavData, result: AlignmentResult) -> np.ndarray:
    aligned = np.vstack(
        [
            _shift_to_reference(wav.channels[channel.channel_index], channel.offset) * channel.gain
            for channel in result.channels
        ]
    )
    aligned.setflags(write=False)
    return aligned


def align_impulse_responses(
    wav: WavData,
    params: AlignmentParams | None = None,
) -> tuple[AlignmentResult, np.ndarray]:
    result = detect_arrivals(wav, params)
    return result, apply_alignment(wav, result)


def alignment_difference(
    old: AlignmentResult | None,
    new: AlignmentResult,
) -> dict[str, Any]:
    """Summarize arrival, reference and offset changes between two alignments."""

    previous = {item.channel_index: item for item in old.channels} if old else {}
    channels: list[dict[str, Any]] = []
    changed_channels: list[int] = []
    for item in new.channels:
        prior = previous.get(item.channel_index)
        old_arrival = None if prior is None else prior.arrival_sample
        old_offset = None if prior is None else prior.offset
        changed = prior is None or old_arrival != item.arrival_sample or old_offset != item.offset
        if changed:
            changed_channels.append(item.channel_index)
        channels.append(
            {
                "channel_index": item.channel_index,
                "channel_name": item.channel_name,
                "old_arrival_sample": old_arrival,
                "new_arrival_sample": item.arrival_sample,
                "arrival_delta": None if old_arrival is None else item.arrival_sample - old_arrival,
                "old_offset": old_offset,
                "new_offset": item.offset,
                "offset_delta": None if old_offset is None else item.offset - old_offset,
                "changed": changed,
            }
        )
    return {
        "old_reference_channel": None if old is None else old.reference_channel,
        "new_reference_channel": new.reference_channel,
        "old_reference_arrival": None if old is None else old.reference_arrival,
        "new_reference_arrival": new.reference_arrival,
        "changed_channels": changed_channels,
        "channels": channels,
    }
