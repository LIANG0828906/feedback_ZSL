"""BS.1770 loudness, true-peak limiting, voice balance, and stem mixing."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np
import soundfile as sf
from scipy.ndimage import minimum_filter1d, uniform_filter1d
from scipy.signal import lfilter, resample_poly

from .errors import EXIT_PROCESSING, fail
from .manifest import Project
from .pcm import float_to_pcm, pcm_to_float


VOICE_ORDER = ("女高", "女低", "男高", "男低")


@dataclass
class VoiceBalance:
    voice: str
    target_ratio: float
    achieved_ratio: float
    correction_gain: float
    correction_gain_db: float

    def as_dict(self) -> dict:
        return {
            "voice": self.voice,
            "target_ratio": round(self.target_ratio, 9),
            "achieved_ratio": round(self.achieved_ratio, 9),
            "absolute_deviation": round(abs(self.achieved_ratio - self.target_ratio), 9),
            "correction_gain": round(self.correction_gain, 12),
            "correction_gain_db": round(self.correction_gain_db, 9),
            "contribution_percent": round(self.achieved_ratio * 100.0, 6),
        }


@dataclass
class MasterResult:
    pcm: np.ndarray
    quantized: np.ndarray
    pcm_bytes: bytes
    stems: dict[str, np.ndarray]
    balances: list[VoiceBalance]
    integrated_loudness_lufs: float
    true_peak_dbtp: float
    sample_peak_dbfs: float
    limiter_triggered_samples: int
    limiter_active: bool
    final_gain_envelope: np.ndarray


def read_project_audio(project: Project) -> tuple[dict[str, list[np.ndarray]], int]:
    loaded: dict[str, list[np.ndarray]] = {voice: [] for voice in VOICE_ORDER}
    arrays: dict[str, np.ndarray] = {}
    total_frames = 0
    for mic in project.microphones:
        try:
            audio, sample_rate = sf.read(
                str(mic.resolved_path),
                dtype="float64",
                always_2d=True,
            )
        except (RuntimeError, OSError, ValueError):
            fail(
                "E_AUDIO_READ",
                mic.name,
                "validated WAV could not be decoded",
                voice=mic.voice,
                exit_code=EXIT_PROCESSING,
            )
        if sample_rate != project.sample_rate:
            fail(
                "E_SAMPLE_RATE_MISMATCH",
                mic.name,
                "sample rate changed after validation",
                voice=mic.voice,
                exit_code=EXIT_PROCESSING,
            )
        arrays[mic.name] = np.asarray(audio, dtype=np.float64)
        total_frames = max(total_frames, arrays[mic.name].shape[0])
    if total_frames <= 0:
        fail(
            "E_EMPTY_AUDIO",
            "<master>",
            "input contains no audio frames",
            exit_code=EXIT_PROCESSING,
        )
    for mic in project.microphones:
        audio = arrays[mic.name]
        if audio.shape[0] < total_frames:
            audio = np.pad(audio, ((0, total_frames - audio.shape[0]), (0, 0)))
        loaded[mic.voice].append(audio)
    return loaded, total_frames


def _k_weight(signal: np.ndarray) -> np.ndarray:
    """ITU-R BS.1770 K-weighting filters for 48 kHz."""
    shelf_b = np.array([1.53512485958697, -2.69169618940638, 1.19839281085285])
    shelf_a = np.array([1.0, -1.69065929318241, 0.73248077421585])
    highpass_b = np.array([1.0, -2.0, 1.0])
    highpass_a = np.array([1.0, -1.99004745483398, 0.99007225036621])
    filtered = lfilter(shelf_b, shelf_a, signal, axis=0)
    return lfilter(highpass_b, highpass_a, filtered, axis=0)


def integrated_loudness(audio: np.ndarray, sample_rate: int) -> float:
    """ITU-R BS.1770-4 integrated loudness in LUFS."""
    array = np.asarray(audio, dtype=np.float64)
    if array.ndim == 1:
        array = array[:, None]
    if array.shape[0] < int(0.4 * sample_rate):
        return float("-inf")
    filtered = _k_weight(array)
    block = int(round(0.4 * sample_rate))
    step = int(round(0.1 * sample_rate))
    starts = np.arange(0, array.shape[0] - block + 1, step)
    mean_squares = np.empty(starts.size, dtype=np.float64)
    for index, start in enumerate(starts):
        segment = filtered[start : start + block]
        channel_power = np.mean(segment**2, axis=0)
        mean_squares[index] = float(np.sum(channel_power))
    with np.errstate(divide="ignore"):
        block_loudness = -0.691 + 10.0 * np.log10(mean_squares)
    absolute = block_loudness > -70.0
    if not np.any(absolute):
        return float("-inf")
    absolute_mean = float(np.mean(mean_squares[absolute]))
    relative_threshold = -0.691 + 10.0 * math.log10(max(absolute_mean, 1e-30)) - 10.0
    gated = mean_squares[absolute & (block_loudness > relative_threshold)]
    if gated.size == 0:
        return float("-inf")
    return -0.691 + 10.0 * math.log10(float(np.mean(gated)))


def true_peak_dbtp(audio: np.ndarray, sample_rate: int) -> tuple[float, float]:
    """Return (4x true peak dBTP, sample peak dBFS)."""
    array = np.asarray(audio, dtype=np.float64)
    if array.ndim == 1:
        array = array[:, None]
    sample_peak = float(np.max(np.abs(array))) if array.size else 0.0
    oversampled = resample_poly(array, 4, 1, axis=0, padtype="line")
    oversampled_peak = float(np.max(np.abs(oversampled))) if oversampled.size else 0.0

    def to_db(value: float) -> float:
        return 20.0 * math.log10(value) if value > 0 else float("-inf")

    return to_db(oversampled_peak), to_db(sample_peak)


def _limit_true_peak(
    audio: np.ndarray,
    sample_rate: int,
    limit_dbtp: float,
) -> tuple[np.ndarray, np.ndarray, int]:
    """Look-ahead true-peak safety limiter with zero output latency.

    The gain envelope is derived from future sample peaks and applied at the
    current sample positions. No delay is introduced, so the frame count is
    unchanged.
    """
    array = np.asarray(audio, dtype=np.float64)
    ceiling = 10.0 ** ((limit_dbtp - 0.55) / 20.0)
    peak = np.max(np.abs(array), axis=1)
    required = np.minimum(1.0, ceiling / np.maximum(peak, 1e-15))
    lookahead = max(1, int(round(0.008 * sample_rate)))
    future_min = minimum_filter1d(required, size=2 * lookahead + 1, mode="nearest")
    release = max(3, int(round(0.040 * sample_rate)))
    smoothed = uniform_filter1d(future_min, size=release, mode="nearest")
    gain = np.minimum(smoothed, future_min)
    output = array * gain[:, None]
    true_peak, _ = true_peak_dbtp(output, sample_rate)
    if true_peak > limit_dbtp:
        safety = 10.0 ** ((limit_dbtp - 0.03 - true_peak) / 20.0)
        gain *= safety
        output *= safety
    triggered = int(np.count_nonzero(gain < 1.0 - 1e-9))
    return output, gain, triggered


def _pan_stem(mono: np.ndarray, pan: float) -> np.ndarray:
    angle = (float(pan) + 1.0) * math.pi / 4.0
    return np.column_stack((mono * math.cos(angle), mono * math.sin(angle)))


def _voice_energy(stem: np.ndarray) -> float:
    return float(np.sum(np.asarray(stem, dtype=np.float64) ** 2))


def _build_stems(
    buses: dict[str, np.ndarray],
    project: Project,
    correction_gains: dict[str, float],
) -> dict[str, np.ndarray]:
    stems: dict[str, np.ndarray] = {}
    for voice in VOICE_ORDER:
        mono = buses[voice] * correction_gains[voice]
        stems[voice] = _pan_stem(mono, project.voices[voice].pan)
    return stems


def _finalize_once(
    stems: dict[str, np.ndarray],
    sample_rate: int,
    target_lufs: float,
    true_peak_limit: float,
    bit_depth: int,
    target_ratios: dict[str, float],
) -> MasterResult:
    mix = sum(stems[voice] for voice in VOICE_ORDER)
    initial_loudness = integrated_loudness(mix, sample_rate)
    if not math.isfinite(initial_loudness):
        fail(
            "E_SILENT_MASTER",
            "<master>",
            "master is silent or too short for loudness measurement",
            exit_code=EXIT_PROCESSING,
        )
    pre_gain_db = target_lufs - initial_loudness
    final_gain_envelope = np.ones(mix.shape[0], dtype=np.float64)
    limiter_triggered = 0
    limiter_active = False
    output = mix
    quantized = mix
    pcm = float_to_pcm(mix, bit_depth)

    for _ in range(10):
        pre_gain = 10.0 ** (pre_gain_db / 20.0)
        output = mix * pre_gain
        limiter_gain = np.ones(output.shape[0], dtype=np.float64)
        peak_now, _ = true_peak_dbtp(output, sample_rate)
        if peak_now > true_peak_limit:
            output, limiter_gain, limiter_triggered = _limit_true_peak(
                output,
                sample_rate,
                true_peak_limit,
            )
            limiter_active = True
        final_gain_envelope = pre_gain * limiter_gain
        pcm = float_to_pcm(output, bit_depth)
        quantized = pcm_to_float(pcm, bit_depth)
        loudness_now = integrated_loudness(quantized, sample_rate)
        peak_after, _ = true_peak_dbtp(quantized, sample_rate)
        if not math.isfinite(loudness_now):
            fail(
                "E_SILENT_MASTER",
                "<master>",
                "master became silent during normalization",
                exit_code=EXIT_PROCESSING,
            )
        if abs(loudness_now - target_lufs) <= 0.03 and peak_after <= true_peak_limit:
            break
        if peak_after > true_peak_limit:
            correction = true_peak_limit - 0.03 - peak_after
            pre_gain_db += correction
        else:
            pre_gain_db += target_lufs - loudness_now

    final_loudness = integrated_loudness(quantized, sample_rate)
    final_true_peak, final_sample_peak = true_peak_dbtp(quantized, sample_rate)
    final_stems = {
        voice: stems[voice] * final_gain_envelope[:, None] for voice in VOICE_ORDER
    }
    energies = {voice: _voice_energy(final_stems[voice]) for voice in VOICE_ORDER}
    total_energy = sum(energies.values())
    balances: list[VoiceBalance] = []
    for voice in VOICE_ORDER:
        achieved = energies[voice] / total_energy if total_energy > 0 else 0.0
        balances.append(
            VoiceBalance(
                voice=voice,
                target_ratio=target_ratios[voice],
                achieved_ratio=achieved,
                correction_gain=float("nan"),
                correction_gain_db=float("nan"),
            )
        )
    return MasterResult(
        pcm=pcm,
        quantized=quantized,
        pcm_bytes=b"",
        stems=final_stems,
        balances=balances,
        integrated_loudness_lufs=final_loudness,
        true_peak_dbtp=final_true_peak,
        sample_peak_dbfs=final_sample_peak,
        limiter_triggered_samples=limiter_triggered,
        limiter_active=limiter_active,
        final_gain_envelope=final_gain_envelope,
    )


def balance_and_master(
    voice_buses: dict[str, np.ndarray],
    project: Project,
) -> MasterResult:
    energies = {voice: _voice_energy(voice_buses[voice]) for voice in VOICE_ORDER}
    for voice in VOICE_ORDER:
        if energies[voice] <= 1e-20:
            fail(
                "E_SILENT_VOICE",
                f"<voice:{voice}>",
                "voice has no measurable energy",
                voice=voice,
                exit_code=EXIT_PROCESSING,
            )
    correction = {
        voice: math.sqrt(project.voices[voice].target_energy_ratio / energies[voice])
        for voice in VOICE_ORDER
    }
    stems = _build_stems(voice_buses, project, correction)
    target_ratios = {
        voice: project.voices[voice].target_energy_ratio for voice in VOICE_ORDER
    }
    result = _finalize_once(
        stems,
        project.sample_rate,
        project.target_loudness_lufs,
        project.true_peak_limit_dbtp,
        project.bit_depth,
        target_ratios,
    )

    # A gain envelope reduces all voices equally, but a limiter can be
    # programme dependent. Rebalance once or twice if needed and include every
    # final correction in the report.
    for _ in range(6):
        errors = [
            abs(balance.achieved_ratio - balance.target_ratio)
            for balance in result.balances
        ]
        if max(errors) <= 0.012:
            break
        achieved = {balance.voice: balance.achieved_ratio for balance in result.balances}
        for voice in VOICE_ORDER:
            if achieved[voice] <= 1e-12:
                continue
            correction[voice] *= math.sqrt(
                project.voices[voice].target_energy_ratio / achieved[voice]
            )
        stems = _build_stems(voice_buses, project, correction)
        result = _finalize_once(
            stems,
            project.sample_rate,
            project.target_loudness_lufs,
            project.true_peak_limit_dbtp,
            project.bit_depth,
            target_ratios,
        )

    final_balances: list[VoiceBalance] = []
    for balance in result.balances:
        gain = correction[balance.voice]
        final_balances.append(
            VoiceBalance(
                voice=balance.voice,
                target_ratio=balance.target_ratio,
                achieved_ratio=balance.achieved_ratio,
                correction_gain=gain,
                correction_gain_db=20.0 * math.log10(max(gain, 1e-30)),
            )
        )
    result.balances = final_balances
    if abs(result.integrated_loudness_lufs - project.target_loudness_lufs) > 0.5:
        fail(
            "E_LOUDNESS_TARGET",
            "<master>",
            "could not meet the loudness target within 0.5 LU",
            exit_code=EXIT_PROCESSING,
        )
    if result.true_peak_dbtp > project.true_peak_limit_dbtp + 1e-9:
        fail(
            "E_TRUE_PEAK_LIMIT",
            "<master>",
            "could not meet the true-peak ceiling",
            exit_code=EXIT_PROCESSING,
        )
    if max(
        abs(balance.achieved_ratio - balance.target_ratio)
        for balance in result.balances
    ) > 0.02:
        fail(
            "E_VOICE_BALANCE",
            "<master>",
            "could not meet the voice energy balance tolerance",
            exit_code=EXIT_PROCESSING,
        )
    return result

