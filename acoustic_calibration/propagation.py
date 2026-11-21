"""Per-node error propagation and conservative upper-bound attribution."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Callable, Sequence

import numpy as np

from .models import (
    BandError,
    BandSpec,
    ChannelAlignment,
    ChannelComputation,
    ErrorNode,
    FilterDesign,
    SmoothingResult,
    SpectralResult,
)

_EPS = np.finfo(np.float64).eps
_QUANT_STEP = 1.0 / 32768.0


@dataclass(frozen=True)
class PropagationReport:
    channel_errors: tuple[tuple[BandError, ...], ...]
    alerts: tuple[dict[str, Any], ...]

    def errors_for(self, channel_index: int) -> tuple[BandError, ...]:
        return self.channel_errors[channel_index]


def _db_from_relative(value: float) -> float:
    return float(20.0 * math.log10(1.0 + max(0.0, value)))


def compute_channel_errors(
    alignment: ChannelAlignment,
    spectral: SpectralResult,
    smoothing: SmoothingResult,
    filter_design: FilterDesign,
    threshold_db: float,
    affected_band_indices: Sequence[int] | None = None,
    previous: Sequence[BandError] | None = None,
    checkpoint: Callable[[int], None] | None = None,
) -> tuple[BandError, ...]:
    weights = np.asarray(smoothing.weights, dtype=np.float64)
    band_count = len(spectral.bands)
    quant_rel = _QUANT_STEP / math.sqrt(12.0) / max(abs(float(alignment.direct_peak)), 1e-12)
    quant_rel = min(quant_rel, 10.0)

    window_rel = np.asarray([band.coherent_gain_rel for band in spectral.bands], dtype=np.float64)
    segment_rel = 2.0 * np.asarray([band.segment_std_rel for band in spectral.bands], dtype=np.float64)
    rounding_rel = np.asarray([band.rounding_rel for band in spectral.bands], dtype=np.float64)
    upstream = quant_rel + window_rel + segment_rel + rounding_rel

    results: list[BandError] = []
    affected = set(range(band_count)) if affected_band_indices is None else {int(index) for index in affected_band_indices}
    previous_by_band = {error.band_index: error for error in (previous or ())}
    for band_index, band in enumerate(spectral.bands):
        if checkpoint is not None:
            checkpoint(band_index)
        if band_index not in affected and band_index in previous_by_band:
            results.append(previous_by_band[band_index])
            continue
        coupling = float(np.dot(weights[band_index], upstream))
        smoothing_rel = max(0.0, coupling - float(upstream[band_index]))
        filter_rel = max(0.0, float(filter_design.truncation_rel))
        contributions = (
            ("quantization", quant_rel),
            ("window_coherent_gain", float(window_rel[band_index])),
            ("segment_variance", float(segment_rel[band_index] + rounding_rel[band_index])),
            ("smoothing_coupling", smoothing_rel),
            ("filter_truncation", filter_rel),
        )

        cumulative = 0.0
        nodes: list[ErrorNode] = []
        for name, contribution in contributions:
            before = cumulative
            cumulative = min(10.0, cumulative + max(0.0, float(contribution)))
            amplification = (1.0 + cumulative) / max(1.0 + before, _EPS)
            nodes.append(
                ErrorNode(
                    name=name,
                    relative_error=float(contribution),
                    cumulative_upper_db=_db_from_relative(cumulative),
                    amplification=float(amplification),
                )
            )
        upper_db = _db_from_relative(cumulative)
        dominant = max(nodes, key=lambda node: node.relative_error).name
        crossing = next((node.name for node in nodes if node.cumulative_upper_db > threshold_db), dominant)
        results.append(
            BandError(
                channel_index=alignment.channel_index,
                band_index=band.index,
                upper_db=float(upper_db),
                threshold_crossed=bool(upper_db > threshold_db),
                dominant_node=dominant,
                attribution_node=crossing,
                nodes=tuple(nodes),
            )
        )
    return tuple(results)


def build_error_report(
    channel_names: Sequence[str],
    channels: Sequence[ChannelComputation],
    bands: Sequence[BandSpec],
    threshold_db: float,
) -> PropagationReport:
    """Build an alert report from already computed per-channel errors."""

    alerts: list[dict[str, Any]] = []
    for channel_name, channel in zip(channel_names, channels, strict=True):
        for error in channel.errors:
            if not error.threshold_crossed:
                continue
            band = bands[error.band_index]
            attributed = next(node for node in error.nodes if node.name == error.attribution_node)
            alerts.append(
                {
                    "channel_index": error.channel_index,
                    "channel_name": channel_name,
                    "band_index": error.band_index,
                    "center_hz": band.center_hz,
                    "node": error.attribution_node,
                    "dominant_node": error.dominant_node,
                    "upper_db": error.upper_db,
                    "threshold_db": threshold_db,
                    "node_relative_error": attributed.relative_error,
                    "node_amplification": attributed.amplification,
                    "node_cumulative_upper_db": attributed.cumulative_upper_db,
                }
            )
    alerts.sort(key=lambda item: (item["channel_index"], item["band_index"]))
    return PropagationReport(
        channel_errors=tuple(channel.errors for channel in channels),
        alerts=tuple(alerts),
    )
