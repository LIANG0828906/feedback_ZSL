"""Deterministic multi-microphone applause synthesis.

Everything is derived from the integer project seed and the declared
microphone positions. Re-running with identical parameters writes byte-for-byte
identical WAV files, and the ground-truth event schedule can always be
recomputed from the seed alone.
"""

from __future__ import annotations

import os

import numpy as np

from .wavio import write_wav

SPEED_OF_SOUND_MPS = 343.0

# Fixed synthesis levels (linear amplitude).
NOISE_RMS = 0.0030
CLAP_PEAK = 0.42
FOOTSTEP_PEAK = 0.20


def _rng(seed, *parts):
    """Independent, named random stream so draw order never cross-couples."""
    seq = np.random.SeedSequence([int(seed)] + [ord(ch) for ch in "|".join(parts)])
    return np.random.default_rng(seq)


def derive_schedule(seed, duration_seconds, sample_rate, microphones=None):
    """Recompute the ground-truth applause schedule from the seed.

    Returns plain JSON-serializable structures (times in seconds).
    """
    rng = _rng(seed, "events")
    event_count = int(rng.integers(4, 8))  # exclusive high -> 4..7 events
    # Sparse, well separated events.
    min_gap = 0.9
    events = []
    cursor = 1.0
    for index in range(event_count):
        remaining = duration_seconds - 0.6 - cursor
        if remaining < 0.8:
            break
        start = cursor + float(rng.uniform(0.0, max(0.05, min(0.6, remaining * 0.25))))
        length = float(rng.uniform(1.0, min(2.4, duration_seconds - start - 0.4)))
        end = min(start + length, duration_seconds - 0.35)
        # Shared pulse train: 40-90 ms between individual claps.
        pulse_times = []
        # First clap sits on the event start so energy-based detection has no
        # structural onset bias; remaining claps follow at 40-90 ms spacing.
        t = start + 0.002
        while t < end - 0.03:
            pulse_times.append(float(t))
            t += float(rng.uniform(0.040, 0.090))
        if not pulse_times or (end - 0.025) - pulse_times[-1] > 0.035:
            pulse_times.append(float(end - 0.02))
        events.append({
            "index": index,
            "start": round(start, 6),
            "end": round(end, 6),
            "duration": round(end - start, 6),
            "pulse_times": [round(v, 6) for v in pulse_times],
        })
        cursor = end + min_gap
    footstep_mics = _footstep_microphone_names(microphones) if microphones else []
    return {
        "seed": int(seed),
        "sample_rate": int(sample_rate),
        "duration_seconds": float(duration_seconds),
        "event_count": len(events),
        "events": events,
        "footstep_mics": footstep_mics,
        "provenance": (
            "Ground truth derived deterministically from project seed via "
            "numpy.random.SeedSequence stream 'events'; per-mic amplitude and "
            "phase derive from microphone 3D position and 343 m/s propagation."
        ),
    }


def _footstep_microphone_names(microphones):
    """Two closest-to-stage mics pick up actor exit footsteps."""
    if not microphones:
        return []
    ranked = sorted(
        microphones,
        key=lambda mic: float(np.linalg.norm(np.asarray(mic["position"], dtype=np.float64))),
    )
    return [mic["name"] for mic in ranked[:2]]


def mic_acoustics(mic):
    """Position-derived gain, propagation delay and phase parameters."""
    position = np.asarray(mic["position"], dtype=np.float64)
    distance = float(np.linalg.norm(position))
    delay_seconds = distance / SPEED_OF_SOUND_MPS
    gain = 1.0 / (1.0 + 0.12 * distance)
    # Position-dependent per-channel phase tilt, stable and reproducible.
    phase_tilt = float(np.tanh(position[0] * 0.08) * 0.55)
    return {
        "distance_m": round(distance, 4),
        "delay_ms": round(delay_seconds * 1000.0, 4),
        "gain": round(gain, 6),
        "phase_tilt": round(phase_tilt, 6),
    }


def _fractional_delay(signal, delay_samples):
    """Windowed-sinc fractional delay (linear phase, position dependent)."""
    whole = int(np.floor(delay_samples))
    frac = delay_samples - whole
    if frac < 1e-9:
        out = np.zeros_like(signal)
        if whole < len(signal):
            out[whole:] = signal[:len(signal) - whole]
        return out
    half = 8
    n = np.arange(-half, half + 1)
    kernel = np.sinc(n - frac) * np.hanning(2 * half + 1)
    kernel /= kernel.sum()
    delayed = np.convolve(signal, kernel, mode="same")
    out = np.zeros_like(signal)
    if whole < len(signal):
        out[whole:] = delayed[:len(signal) - whole]
    return out


def _clap_pulse(rng, sample_rate):
    """A single short broadband clap: decaying noise burst with a body ring."""
    tau = float(rng.uniform(0.003, 0.006))
    length = int(sample_rate * 0.02)
    t = np.arange(length) / sample_rate
    envelope = np.exp(-t / tau)
    noise = rng.standard_normal(length)
    ring_freq = float(rng.uniform(1800.0, 3400.0))
    ring = np.sin(2.0 * np.pi * ring_freq * t + rng.uniform(0.0, 2.0 * np.pi))
    pulse = (0.75 * noise + 0.45 * ring) * envelope
    pulse *= CLAP_PEAK / max(1e-9, float(np.max(np.abs(pulse))))
    return pulse


def _footstep(rng, sample_rate):
    """Low-frequency footstep thump, energy concentrated below ~300 Hz."""
    length = int(sample_rate * 0.14)
    t = np.arange(length) / sample_rate
    envelope = np.exp(-t / 0.035)
    freq = np.linspace(150.0, 70.0, length)
    phase = 2.0 * np.pi * np.cumsum(freq) / sample_rate
    thump = np.sin(phase + rng.uniform(0.0, 2.0 * np.pi)) * envelope
    thump *= FOOTSTEP_PEAK / max(1e-9, float(np.max(np.abs(thump))))
    return thump


def _footstep_times(seed, mic_index, truth, sample_rate):
    rng = _rng(seed, "feet", str(mic_index))
    times = []
    forbidden = [(e["start"] - 0.3, e["end"] + 0.3) for e in truth["events"]]
    candidate = 0.5
    duration = truth["duration_seconds"]
    while candidate < duration - 0.4:
        candidate += float(rng.uniform(0.7, 1.6))
        if any(lo <= candidate <= hi for lo, hi in forbidden):
            continue
        if candidate < duration - 0.3:
            times.append(round(candidate, 6))
    return times


def synthesize_mic(seed, truth, mic, mic_index, footstep_mics):
    """Render one microphone track as float64, shape (samples, channels)."""
    sr = truth["sample_rate"]
    total = int(round(truth["duration_seconds"] * sr))
    channels = int(mic["channels"])
    acoustics = mic_acoustics(mic)
    delay_samples = acoustics["delay_ms"] / 1000.0 * sr
    gain = acoustics["gain"]

    mono = np.zeros(total, dtype=np.float64)
    clap_rng = _rng(seed, "clap", str(mic_index))
    for event in truth["events"]:
        # Far mics receive a softer, jittered applause bed.
        distance_factor = 0.55 + 0.45 * gain
        for pulse_time in event["pulse_times"]:
            pulse = _clap_pulse(clap_rng, sr) * distance_factor
            center = int(round(pulse_time * sr))
            end = min(total, center + len(pulse))
            mono[center:end] += pulse[:end - center]

    if mic_index in footstep_mics:
        feet_rng = _rng(seed, "feet-render", str(mic_index))
        for foot_time in _footstep_times(seed, mic_index, truth, sr):
            step = _footstep(feet_rng, sr)
            center = int(round(foot_time * sr))
            end = min(total, center + len(step))
            mono[center:end] += step[:end - center]

    # Position-dependent propagation delay introduces the phase relationship.
    mono = _fractional_delay(mono, delay_samples) * gain

    tracks = [mono]
    for ch in range(1, channels):
        noise_rng = _rng(seed, "noise", str(mic_index), str(ch))
        extra = _fractional_delay(mono, 2.0 * ch)
        extra += NOISE_RMS * noise_rng.standard_normal(total)
        tracks.append(extra)
    noise_rng = _rng(seed, "noise", str(mic_index), "0")
    tracks[0] = mono + NOISE_RMS * noise_rng.standard_normal(total)
    signal = np.stack(tracks, axis=1)
    # Mild soft clip guard so 16-bit files never hard clip.
    signal = np.tanh(signal * 1.05) / np.tanh(1.05)
    return signal, acoustics


def generate_project_audio(project):
    """Synthesize every microphone WAV and return the ground-truth document."""
    sr = project["sample_rate"]
    truth = derive_schedule(project["seed"], project["duration_seconds"], sr,
                            project["microphones"])
    name_to_index = {mic["name"]: index for index, mic in enumerate(project["microphones"])}
    footstep_mics = {name_to_index[name] for name in truth["footstep_mics"]}
    truth["microphones"] = []
    for index, mic in enumerate(project["microphones"]):
        signal, acoustics = synthesize_mic(project["seed"], truth, mic, index, footstep_mics)
        os.makedirs(os.path.dirname(mic["file"]), exist_ok=True)
        write_wav(mic["file"], signal, sr, subtype="PCM_16")
        truth["microphones"].append({"name": mic["name"], **acoustics})
    return truth
