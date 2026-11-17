"""Deterministic reverb impulse responses, block convolution and binaural panner."""

from __future__ import annotations

import math

import numpy as np

from .rng import DeterministicRng, seed_mix

SPEED_OF_SOUND = 343.0
ITD_MAX_S = 0.7e-3
ILD_MAX_DB = 8.0


def build_impulse_response(rt60: float, sample_rate: int,
                           seed: int, layer_id: str) -> np.ndarray:
    """Build a deterministic exponentially-decaying noise impulse response.

    The IR length covers the rt60 tail (floor 0.25 s) and is normalised to
    unit energy; the dry/wet power split is applied at render time.
    """
    length = max(int(0.25 * sample_rate), int(round(rt60 * sample_rate)))
    rng = DeterministicRng(seed_mix(int(seed), "reverb", layer_id))
    noise = rng.noise(length)
    time = np.arange(length, dtype=np.float64) / sample_rate
    if rt60 <= 0:
        decay = np.zeros(length, dtype=np.float64)
    else:
        decay = np.power(10.0, -3.0 * time / rt60)
    # A few deterministic early reflections keep the tail from sounding flat.
    ir = noise * decay
    for reflection_ms, level in ((11.0, 0.9), (23.0, 0.6), (37.0, 0.4), (51.0, 0.28)):
        position = int(reflection_ms * sample_rate / 1000.0)
        if position < length:
            ir[position] += level * max(decay[position], 1e-9)
    energy = float(np.dot(ir, ir)) + 1e-12
    # Divide by RMS so the resulting impulse has unit energy. The dry/wet
    # power split is applied at render time.
    return ir / math.sqrt(energy)


class FftConvolver:
    """Overlap-save convolver that carries its own state across blocks."""

    def __init__(self, impulse: np.ndarray, block_samples: int) -> None:
        self.block_samples = int(block_samples)
        self.impulse = np.asarray(impulse, dtype=np.float64).copy()
        self.n_fft = 1
        while self.n_fft < self.block_samples + self.impulse.size - 1:
            self.n_fft *= 2
        self.spectrum = np.fft.rfft(self.impulse, self.n_fft)
        self.history = np.zeros(self.n_fft - self.block_samples, dtype=np.float64)

    def process(self, block: np.ndarray) -> np.ndarray:
        block = np.asarray(block, dtype=np.float64)
        if block.size != self.block_samples:
            padded = np.zeros(self.block_samples, dtype=np.float64)
            padded[: block.size] = block
            block = padded
        frame = np.concatenate((self.history, block))
        output = np.fft.irfft(np.fft.rfft(frame, self.n_fft) * self.spectrum, self.n_fft)
        result = output[-self.block_samples :].copy()
        self.history = frame[-self.history.size :].copy() if self.history.size else self.history
        return result[: block.shape[0]] if result.shape[0] == self.block_samples else result

    def state_dict(self) -> dict:
        return {"history": self.history.copy(), "impulse": self.impulse.copy()}

    def load_state(self, state: dict) -> None:
        history = np.asarray(state["history"], dtype=np.float64)
        impulse = np.asarray(state["impulse"], dtype=np.float64)
        if history.shape != self.history.shape or impulse.shape != self.impulse.shape:
            raise ValueError("convolver state shape mismatch")
        self.history = history.copy()
        self.impulse = impulse.copy()
        self.spectrum = np.fft.rfft(self.impulse, self.n_fft)


def binaural_params(azimuth: float, elevation: float) -> tuple[float, float, float, float]:
    """Analytical, reproducible HRTF substitute.

    Returns (itd_seconds, ild_db, gain_left, gain_right).
    ITD follows the classic Woodworth spherical-head formula; ILD uses a
    smooth sin(azimuth) law that stays differentiable across all angles.
    """
    head_radius = 0.0875
    itd = (head_radius / SPEED_OF_SOUND) * (azimuth + math.sin(azimuth)) * max(math.cos(elevation), 0.0)
    itd = float(np.clip(itd, -ITD_MAX_S, ITD_MAX_S))
    ild = ILD_MAX_DB * math.sin(azimuth) * max(math.cos(elevation), 0.0)
    gain_left = float(10.0 ** ((-ild / 2.0) / 20.0))
    gain_right = float(10.0 ** ((ild / 2.0) / 20.0))
    return itd, float(ild), gain_left, gain_right


def fractional_delay(signal: np.ndarray, delay_samples: float) -> np.ndarray:
    """Linear-interpolation fractional delay for a finite signal block."""
    if signal.size == 0:
        return signal.copy()
    whole = int(math.floor(delay_samples))
    frac = delay_samples - whole
    length = signal.size + whole + 2
    padded = np.zeros(length, dtype=np.float64)
    padded[whole : whole + signal.size] = signal
    shifted = padded[:-1] * (1.0 - frac) + padded[1:] * frac
    return shifted[: signal.size]


class BinauralPanner:
    """Per-sample ITD/ILD panner with persisted histories for resume."""

    def __init__(self, sample_rate: int, max_delay_s: float = ITD_MAX_S) -> None:
        self.sample_rate = int(sample_rate)
        self.max_delay = int(math.ceil(max_delay_s * sample_rate)) + 2
        # One extra slot absorbs the +1 interpolation lookup at full delay.
        self.left_history = np.zeros(self.max_delay + 1, dtype=np.float64)
        self.right_history = np.zeros(self.max_delay + 1, dtype=np.float64)

    def render(self, signal: np.ndarray, itd: np.ndarray, gain_left: np.ndarray,
               gain_right: np.ndarray) -> np.ndarray:
        """Render a mono block into an (n, 2) stereo block."""
        n = signal.size
        buffer_size = self.max_delay + 1 + n
        padded_left = np.zeros(buffer_size, dtype=np.float64)
        padded_right = np.zeros(buffer_size, dtype=np.float64)
        padded_left[: self.max_delay + 1] = self.left_history
        padded_right[: self.max_delay + 1] = self.right_history
        padded_left[self.max_delay + 1 : self.max_delay + 1 + n] = signal
        padded_right[self.max_delay + 1 : self.max_delay + 1 + n] = signal
        indices = np.arange(n)
        # Positive ITD means the source is on the left: the right ear arrives late.
        delay_left_samples = np.maximum(-itd * self.sample_rate, 0.0)
        delay_right_samples = np.maximum(itd * self.sample_rate, 0.0)

        def ear(padded: np.ndarray, delay: np.ndarray, gain: np.ndarray) -> np.ndarray:
            base = indices + 1
            whole = np.floor(delay).astype(np.int64)
            frac = delay - whole
            early = padded[base + whole]
            late = padded[base + whole + 1]
            return (early * (1.0 - frac) + late * frac) * gain

        left = ear(padded_left, delay_left_samples, gain_left)
        right = ear(padded_right, delay_right_samples, gain_right)
        self.left_history = padded_left[n : n + self.max_delay + 1].copy()
        self.right_history = padded_right[n : n + self.max_delay + 1].copy()
        return np.stack((left, right), axis=1)

    def state_dict(self) -> dict:
        return {
            "left_history": self.left_history.copy(),
            "right_history": self.right_history.copy(),
        }

    def load_state(self, state: dict) -> None:
        left = np.asarray(state["left_history"], dtype=np.float64)
        right = np.asarray(state["right_history"], dtype=np.float64)
        if left.shape != self.left_history.shape or right.shape != self.right_history.shape:
            raise ValueError("panner history shape mismatch")
        self.left_history = left.copy()
        self.right_history = right.copy()

