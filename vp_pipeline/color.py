"""Deterministic transfer-function and alpha-convention conversions."""

from __future__ import annotations

import numpy as np


def srgb_to_linear(values: np.ndarray | float) -> np.ndarray:
    array = np.asarray(values, dtype=np.float64)
    clipped = np.clip(array, 0.0, 1.0)
    return np.where(
        clipped <= 0.04045,
        clipped / 12.92,
        np.power((clipped + 0.055) / 1.055, 2.4),
    )


def linear_to_srgb(values: np.ndarray | float) -> np.ndarray:
    array = np.asarray(values, dtype=np.float64)
    clipped = np.clip(array, 0.0, 1.0)
    return np.where(
        clipped <= 0.0031308,
        clipped * 12.92,
        1.055 * np.power(clipped, 1.0 / 2.4) - 0.055,
    )


def quantize_rgba(values: np.ndarray) -> np.ndarray:
    clipped = np.clip(np.asarray(values, dtype=np.float64), 0.0, 1.0)
    return np.floor(clipped * 255.0 + 0.5).astype(np.uint8)


def straight_to_premultiplied_linear(
    straight_rgb: tuple[float, float, float],
    alpha: float,
) -> tuple[float, float, float]:
    return tuple(float(np.clip(channel * alpha, 0.0, 1.0)) for channel in straight_rgb)


def premultiplied_linear_to_straight(
    premultiplied_rgb: tuple[float, float, float],
    alpha: float,
) -> tuple[float, float, float]:
    if alpha <= 0.0:
        return (0.0, 0.0, 0.0)
    return tuple(
        float(np.clip(channel / alpha, 0.0, 1.0)) for channel in premultiplied_rgb
    )


def encoded_to_linear_rgba(
    encoded_rgb: tuple[float, float, float],
    alpha: float,
    transfer: str,
    alpha_mode: str,
) -> tuple[float, float, float]:
    rgb = np.asarray(encoded_rgb, dtype=np.float64)
    if transfer == "srgb":
        linear = srgb_to_linear(rgb)
    elif transfer == "linear":
        linear = np.clip(rgb, 0.0, 1.0)
    else:
        raise ValueError(f"unsupported transfer: {transfer}")
    premultiplied = tuple(float(value) for value in linear)
    if alpha_mode == "straight":
        return premultiplied
    if alpha_mode == "premultiplied":
        return premultiplied_linear_to_straight(premultiplied, alpha)
    raise ValueError(f"unsupported alpha mode: {alpha_mode}")


def linear_straight_to_encoded(
    straight_rgb: tuple[float, float, float],
    alpha: float,
    transfer: str,
    alpha_mode: str,
) -> tuple[float, float, float]:
    rgb = np.asarray(straight_rgb, dtype=np.float64)
    if alpha_mode == "premultiplied":
        rgb = np.clip(rgb * alpha, 0.0, 1.0)
    elif alpha_mode != "straight":
        raise ValueError(f"unsupported alpha mode: {alpha_mode}")
    if transfer == "srgb":
        encoded = linear_to_srgb(rgb)
    elif transfer == "linear":
        encoded = np.clip(rgb, 0.0, 1.0)
    else:
        raise ValueError(f"unsupported transfer: {transfer}")
    return tuple(float(value) for value in encoded)
