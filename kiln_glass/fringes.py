"""Deterministic stress-fringe and bubble sample tables per brick.

Everything in this module is a pure function of the integer fringe seed and
the brick number.  Two calls with the same (seed, brick_id) produce
byte-identical NumPy arrays; nothing depends on triangle order or on any
unseeded/global random state.
"""

from __future__ import annotations

from dataclasses import dataclass
import numpy as np

from . import xrand


@dataclass(frozen=True)
class FringeParams:
    brick_id: int
    seed: int
    frequency_x: float
    frequency_y: float
    phase_x: float
    phase_y: float
    amplitude: float
    base_modulation: float
    hue: float
    saturation: float
    scatter_gain: float
    bubbles: np.ndarray  # (N, 3): local x, local y, radius

    def to_row(self) -> tuple:
        return (
            self.brick_id,
            self.seed,
            self.frequency_x,
            self.frequency_y,
            self.phase_x,
            self.phase_y,
            self.amplitude,
            self.base_modulation,
            self.hue,
            self.saturation,
            self.scatter_gain,
            self.bubbles.shape[0],
        )


def _hsv_to_rgb(hue: float, saturation: float, value: float) -> np.ndarray:
    h = (hue % 1.0) * 6.0
    i = int(np.floor(h))
    f = h - i
    p = value * (1.0 - saturation)
    q = value * (1.0 - saturation * f)
    t = value * (1.0 - saturation * (1.0 - f))
    i %= 6
    if i == 0:
        rgb = (value, t, p)
    elif i == 1:
        rgb = (q, value, p)
    elif i == 2:
        rgb = (p, value, t)
    elif i == 3:
        rgb = (p, q, value)
    elif i == 4:
        rgb = (t, p, value)
    else:
        rgb = (value, p, q)
    return np.array(rgb, dtype=np.float64)


def generate_fringe_params(seed: int, brick_id: int, bubble_count: int) -> FringeParams:
    """Generate the full deterministic fringe/bubble table for one brick."""
    frequency_x = float(xrand.uniform_range(seed, brick_id, xrand.CH_FREQUENCY, 0, 6.0, 13.0))
    frequency_y = float(xrand.uniform_range(seed, brick_id, xrand.CH_FREQUENCY, 1, 5.0, 11.0))
    phase_x = float(xrand.uniform_range(seed, brick_id, xrand.CH_PHASE, 0, 0.0, 2.0 * np.pi))
    phase_y = float(xrand.uniform_range(seed, brick_id, xrand.CH_PHASE, 1, 0.0, 2.0 * np.pi))
    amplitude = float(xrand.uniform_range(seed, brick_id, xrand.CH_AMPLITUDE, 0, 0.12, 0.30))
    base_modulation = float(xrand.uniform_range(seed, brick_id, xrand.CH_AMPLITUDE, 1, 0.62, 0.82))
    hue = float(xrand.uniform(seed, brick_id, xrand.CH_HUE, 0))
    saturation = float(xrand.uniform_range(seed, brick_id, xrand.CH_HUE, 1, 0.35, 0.7))
    scatter_gain = float(xrand.uniform_range(seed, brick_id, xrand.CH_SCATTER, 0, 0.10, 0.22))

    indices = np.arange(bubble_count, dtype=np.uint64)
    bx = xrand.uniform(seed, brick_id, xrand.CH_BUBBLE_X, indices)
    by = xrand.uniform(seed, brick_id, xrand.CH_BUBBLE_Y, indices)
    br = xrand.uniform_range(seed, brick_id, xrand.CH_BUBBLE_R, indices, 0.008, 0.035)
    bubbles = np.stack([bx, by, br], axis=1)

    return FringeParams(
        brick_id=brick_id,
        seed=seed,
        frequency_x=frequency_x,
        frequency_y=frequency_y,
        phase_x=phase_x,
        phase_y=phase_y,
        amplitude=amplitude,
        base_modulation=base_modulation,
        hue=hue,
        saturation=saturation,
        scatter_gain=scatter_gain,
        bubbles=bubbles,
    )


def evaluate_fringes(
    params: FringeParams,
    local_xy: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    """Evaluate fringe modulation and bubble mask at local brick coords.

    ``local_xy`` has shape (..., 2) with values in [0, 1] along the brick
    face.  Returns ``(modulation, bubble_mask)`` where modulation is in
    (0, 1] and bubble_mask is 1.0 inside a bubble, else 0.0.
    """
    xy = np.asarray(local_xy, dtype=np.float64)
    x = xy[..., 0]
    y = xy[..., 1]
    wave = (
        np.sin(params.frequency_x * x + params.phase_x)
        + 0.6 * np.sin(params.frequency_y * y + params.phase_y)
        + 0.35 * np.sin(
            (params.frequency_x + params.frequency_y) * (x + y)
            + params.phase_x * 0.5
        )
    ) / 1.95
    modulation = params.base_modulation + params.amplitude * wave

    bubble_mask = np.zeros(x.shape, dtype=np.float64)
    if params.bubbles.shape[0]:
        deltas = xy[..., None, :] - params.bubbles[None, :, :2]
        dist2 = np.sum(deltas * deltas, axis=-1)
        hit = dist2 <= (params.bubbles[:, 2] ** 2)
        bubble_mask = np.any(hit, axis=-1).astype(np.float64)
    modulation = np.where(bubble_mask > 0.0, modulation * 0.55, modulation)
    return np.clip(modulation, 0.02, 1.0), bubble_mask


def scatter_color(params: FringeParams, modulation: np.ndarray) -> np.ndarray:
    """Stress-fringe modulated internal scattering RGB in [0, 1]."""
    base = _hsv_to_rgb(params.hue, params.saturation, 1.0)
    strength = params.scatter_gain * (0.35 + 0.65 * modulation)
    return base * strength[..., None]
