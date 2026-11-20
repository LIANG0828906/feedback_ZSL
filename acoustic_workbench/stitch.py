from __future__ import annotations

from typing import Any

import numpy as np

from .errors import E_STITCH_FAIL, fail
from .model import Scene


def stitch_multichannel_response(
    early_segment: np.ndarray,
    late_response: np.ndarray,
    scene: Scene,
    listener_id: str,
) -> np.ndarray:
    channels = len(scene.channel_layout)
    if early_segment.ndim != 2 or early_segment.shape[0] != channels:
        fail(
            E_STITCH_FAIL,
            "early segment does not preserve the 5.1 channel layout",
            listener_id=listener_id,
            shape=list(early_segment.shape),
        )
    if late_response.ndim != 2 or late_response.shape[0] != channels:
        fail(
            E_STITCH_FAIL,
            "late response does not preserve the 5.1 channel layout",
            listener_id=listener_id,
            shape=list(late_response.shape),
        )
    if not np.all(np.isfinite(early_segment)) or not np.all(
        np.isfinite(late_response)
    ):
        fail(E_STITCH_FAIL, "cannot stitch non-finite channel data")
    total = max(early_segment.shape[1], late_response.shape[1])
    output = np.zeros((channels, total), dtype=np.float64)
    output[:, : early_segment.shape[1]] += early_segment
    output[:, : late_response.shape[1]] += late_response
    if output.shape[0] != 6:
        fail(E_STITCH_FAIL, "stitched response is not 5.1")
    if len({int(output.shape[1])}) != 1:
        fail(E_STITCH_FAIL, "channel lengths diverged during stitching")
    if not np.all(np.isfinite(output)):
        fail(E_STITCH_FAIL, "stitched response contains non-finite samples")
    return np.ascontiguousarray(output)


def compute_field_coefficients(
    response: np.ndarray,
    scene: Scene,
    decay: dict[str, Any],
    listener_id: str,
    early_events: list[dict[str, Any]],
) -> dict[str, Any]:
    if response.ndim != 2 or response.shape[0] != 6:
        fail(E_STITCH_FAIL, "field coefficients require a 5.1 response")
    if not np.all(np.isfinite(response)):
        fail(E_STITCH_FAIL, "field coefficients contain non-finite response data")
    channels: dict[str, Any] = {}
    for index, channel_name in enumerate(scene.channel_layout):
        rms = float(np.sqrt(np.mean(response[index] ** 2)))
        peak = float(np.max(np.abs(response[index])))
        curves = decay["channels"][channel_name]
        direct = float(np.max(np.abs(response[index]))) if response.size else 0.0
        channels[channel_name] = {
            "rms": rms,
            "peak": peak,
            "direct_peak": direct,
            "t60_s": [float(curve["t60_s"]) for curve in curves],
            "band_curve_valid": [bool(curve["valid"]) for curve in curves],
        }
    return {
        "listener_id": listener_id,
        "channel_layout": list(scene.channel_layout),
        "band_centers_hz": list(decay["band_centers_hz"]),
        "channels": channels,
        "early_event_count": len(early_events),
        "response_samples": int(response.shape[1]),
        "response_rms": float(np.sqrt(np.mean(response**2))),
    }

