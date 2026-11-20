from __future__ import annotations

import math

import numpy as np

from .bands import Band


def smoothing_weights(
    bands: tuple[Band, ...], index: int, smoothing_octaves: float
) -> tuple[np.ndarray, np.ndarray]:
    centers = np.asarray([band.center_hz for band in bands], dtype=np.float64)
    sigma = smoothing_octaves / 2.3548200450309493
    distances = np.abs(np.log2(centers / centers[index]))
    weights = np.exp(-0.5 * (distances / sigma) ** 2)
    retained = weights >= max(1e-4 * float(np.max(weights)), 1e-12)
    retained[index] = True
    indices = np.flatnonzero(retained)
    selected = weights[indices]
    selected /= selected.sum()
    return indices, selected


def smooth_band(
    index: int,
    bands: tuple[Band, ...],
    analysis: dict[str, object],
    smoothing_octaves: float,
    segmentation_safety: float,
) -> dict[str, object]:
    indices, weights = smoothing_weights(bands, index, smoothing_octaves)
    power = np.asarray(analysis["power"], dtype=np.float64)
    q = np.asarray(analysis["quantization_power_bound"], dtype=np.float64)
    seg = np.asarray(analysis["segmentation_power_bound"], dtype=np.float64)
    smoothed_power = max(float(np.dot(weights, power[indices])), np.finfo(np.float64).tiny)
    propagated_q = float(np.dot(weights, q[indices]))
    propagated_seg = float(np.dot(weights, seg[indices]))
    coupling = 0.0
    for local_index, weight in zip(indices, weights):
        if int(local_index) != index:
            coupling += float(weight) * float(q[local_index] + seg[local_index])
    coupling_sources: list[dict[str, object]] = []
    for local_index, weight in zip(indices, weights):
        if int(local_index) != index:
            coupling_sources.append(
                {
                    "band": bands[int(local_index)].label,
                    "weight": float(weight),
                    "quantization_power_bound": float(q[local_index]),
                    "segmentation_power_bound": float(seg[local_index]),
                }
            )
    return {
        "smoothed_power": smoothed_power,
        "smoothed_db": 10.0 * math.log10(smoothed_power),
        "propagated_quantization_bound": propagated_q,
        "propagated_segmentation_bound": propagated_seg,
        "smoothing_coupling_bound": float(coupling),
        "weight_indices": indices.astype(np.int64),
        "weights": weights.astype(np.float64),
        "max_neighbor_weight": float(
            np.max(np.delete(weights, np.flatnonzero(indices == index)))
            if indices.size > 1
            else 0.0
        ),
        "coupling_sources": coupling_sources,
    }


def weights_signature(indices: np.ndarray, weights: np.ndarray) -> list[dict[str, float | int]]:
    return [
        {"index": int(index), "weight": float(weight)}
        for index, weight in zip(indices, weights)
    ]

