"""Masking-crosstalk decoupling using bed-assisted least-squares subtraction."""

from __future__ import annotations

import numpy as np

from . import PART_BANGU, PART_BED

# Narrow bands (Hz) that isolate the metallic masking partials.
SOURCE_BAND_HZ = {
    "大锣": (150.0, 900.0),
    "小锣": (700.0, 4000.0),
}


def bandpass_mask(n, sr, low_hz, high_hz):
    freqs = np.fft.rfftfreq(n, 1.0 / sr)
    mask = (freqs >= low_hz) & (freqs <= high_hz)
    return mask


def band_limited(x, sr, low_hz, high_hz):
    spec = np.fft.rfft(x)
    spec[~bandpass_mask(x.shape[0], sr, low_hz, high_hz)] = 0.0
    return np.fft.irfft(spec, x.shape[0])


def window_samples(sr, window_seconds, n):
    start = int(round(window_seconds[0] * sr))
    end = min(n, int(round(window_seconds[1] * sr)))
    return max(start, 0), max(end, start)


def rms(x):
    if x.size == 0:
        return 0.0
    return float(np.sqrt(np.mean(x**2)))


def energy_db(x):
    e = float(np.sum(x**2))
    if e <= 0.0:
        return -np.inf
    return 10.0 * np.log10(e)


def estimate_noise_floor(bed, channels_signals, sr):
    """RMS of the dedicated bed channel (same ambient floor for every mic)."""
    floor = rms(bed)
    return {name: floor for name in channels_signals}


def _correlation(a, b):
    sa, sb = np.std(a), np.std(b)
    if sa <= 0.0 or sb <= 0.0:
        return 0.0
    return float(np.mean((a - a.mean()) * (b - b.mean())) / (sa * sb))


def _onset_protection_mask(n, sr, ref_times, protect_seconds):
    mask = np.zeros(n, dtype=bool)
    radius = int(round(protect_seconds * sr))
    for t in ref_times:
        center = int(round(t * sr))
        lo = max(0, center - radius // 4)
        hi = min(n, center + radius)
        mask[lo:hi] = True
    return mask


def decouple_channel(victim_name, victim, source_name, source, cue, bed_rms,
                     ref_samples):
    """Run detection + subtraction for one declared crosstalk relationship.

    Returns ``(clean_signal, detail_dict)``.
    """
    sr = cue.sample_rate
    n = victim.shape[0]
    ch = cue.channel(victim_name)
    win_start, win_end = window_samples(sr, ch.crosstalk_window, n)
    band = SOURCE_BAND_HZ.get(source_name, (50.0, sr / 2.0 - 1.0))
    source_band = band_limited(source, sr, *band)
    victim_band = band_limited(victim, sr, *band)

    idx = np.arange(win_start, win_end)
    # Recreate the exact edge fades used at injection time so the least
    # squares fit matches the physically injected crosstalk waveform.
    fade = min(int(0.005 * sr), max((win_end - win_start) // 4, 1))
    window_env = np.ones(win_end - win_start)
    if fade > 1:
        ramp = np.linspace(0.0, 1.0, fade)
        window_env[:fade] = ramp
        window_env[-fade:] = ramp[::-1]
    s_win = source_band[idx] * window_env
    v_win = victim_band[idx]
    denom = float(np.dot(s_win, s_win))
    if denom <= 0.0:
        gain_hat = 0.0
    else:
        gain_hat = float(np.dot(s_win, v_win) / denom)
    declared_gain = 10.0 ** (ch.crosstalk_gain_db / 20.0)
    corr = _correlation(s_win, v_win)

    # Detection: declared-strength leakage AND statistically visible coupling.
    detected = (
        gain_hat >= 0.5 * declared_gain
        and corr >= 0.12
        and rms(v_win) > 2.0 * bed_rms
    )

    pre_energy = energy_db(victim_band[idx])
    detail = {
        "channel": victim_name,
        "crosstalk_source": source_name,
        "detected": bool(detected),
        "declared_gain_db": round(ch.crosstalk_gain_db, 6),
        "estimated_gain_linear": round(gain_hat, 6),
        "correlation": round(corr, 6),
        "window_start_sample": win_start,
        "window_end_sample": win_end,
        "band_hz": [band[0], band[1]],
        "modified_interval_count": 0,
        "max_modification": 0.0,
        "crosstalk_energy_before_db": _round_db(pre_energy),
        "crosstalk_energy_after_db": _round_db(pre_energy),
        "attenuation_db": 0.0,
        "crosstalk_projection_before": round(gain_hat, 6),
        "crosstalk_projection_after": round(gain_hat, 6),
        "crosstalk_attenuation_db": 0.0,
        "residual_detected": False,
        "residual_ratio_to_bed": 0.0,
        "modified": False,
    }
    clean = victim.copy()
    if not detected:
        detail["residual_ratio_to_bed"] = round(
            rms(victim_band[idx]) / max(bed_rms, 1e-12), 6
        )
        return clean, detail

    correction_full = gain_hat * source_band

    # Transient-peak protection for the 小锣->板鼓 leak: never cut more than
    # 0.8 dB off the host drum's own attack inside its onset windows.
    if victim_name == PART_BANGU:
        protect = _onset_protection_mask(n, sr, cue.reference_times, 0.03)
        max_cut = 1.0 - 10.0 ** (-0.8 / 20.0)
        scale = np.ones(n)
        host = np.abs(victim)
        peak_ref = float(np.max(host)) if np.max(host) > 0 else 1.0
        cap = max_cut * peak_ref
        hot = protect & (np.abs(correction_full) > cap) & (cap > 0.0)
        scale[hot] = cap / np.abs(correction_full[hot])
        correction = correction_full * scale
    else:
        correction = correction_full

    # Smooth the correction at the window edges so no clicks are introduced.
    env = np.zeros(n)
    env[win_start:win_end] = window_env
    correction *= env

    clean = victim - correction
    post_band = band_limited(clean, sr, *band)
    post_energy = energy_db(post_band[idx])
    mod = correction[win_start:win_end]
    nonzero = np.count_nonzero(np.abs(mod) > 1e-9)
    detail["modified"] = True
    detail["modified_interval_count"] = int(nonzero)
    detail["max_modification"] = round(float(np.max(np.abs(mod))), 6)
    detail["crosstalk_energy_after_db"] = _round_db(post_energy)
    detail["attenuation_db"] = _round_db(pre_energy - post_energy)
    projection = crosstalk_component_metrics(
        victim, clean, source, sr, ch.crosstalk_window, band
    )
    detail.update(projection)

    residual_rms = rms(post_band[idx])
    ratio = residual_rms / max(bed_rms, 1e-12)
    detail["residual_ratio_to_bed"] = round(ratio, 6)
    detail["residual_detected"] = bool(ratio > 1.4142)
    return clean, detail


def run_decoupling(signals, cue, ref_offsets=None):
    """Decouple every declared victim; returns (signals, reports list)."""
    sr = cue.sample_rate
    bed = signals[PART_BED]
    floors = estimate_noise_floor(bed, signals, sr)
    out = dict(signals)
    reports = []
    ref_samples = cue.ref_samples()
    for ch in cue.channels:
        if not ch.crosstalk_source:
            continue
        clean, detail = decouple_channel(
            ch.part,
            out[ch.part],
            ch.crosstalk_source,
            out[ch.crosstalk_source],
            cue,
            floors[ch.part],
            ref_samples,
        )
        out[ch.part] = clean
        reports.append(detail)
    return out, reports


def crosstalk_component_metrics(original_victim, cleaned_victim, source_signal,
                                sr, window_seconds, band):
    """Measure the injected crosstalk component itself (not total band energy).

    The victim's own voice also occupies part of the source band, so total
    band energy barely moves even when the leak is fully removed.  We project
    victim energy onto the band-limited source template instead, which
    isolates the crosstalk component and its residual.
    """
    n = original_victim.shape[0]
    win_start, win_end = window_samples(sr, window_seconds, n)
    idx = np.arange(win_start, win_end)
    source_band = band_limited(source_signal, sr, *band)
    before_band = band_limited(original_victim, sr, *band)
    after_band = band_limited(cleaned_victim, sr, *band)
    template = source_band[idx]
    denom = float(np.dot(template, template))
    if denom <= 0.0:
        return {
            "crosstalk_projection_before": 0.0,
            "crosstalk_projection_after": 0.0,
            "crosstalk_attenuation_db": 0.0,
        }
    before_coef = float(np.dot(template, before_band[idx]) / denom)
    after_coef = float(np.dot(template, after_band[idx]) / denom)
    before_energy = before_coef**2 * denom
    after_energy = after_coef**2 * denom
    attenuation = (
        10.0 * np.log10(before_energy / after_energy)
        if after_energy > 0.0 and before_energy > 0.0
        else None
    )
    return {
        "crosstalk_projection_before": round(before_coef, 6),
        "crosstalk_projection_after": round(after_coef, 6),
        "crosstalk_attenuation_db": _round_db(attenuation),
    }


def _round_db(value):
    if not np.isfinite(value):
        return None
    return round(float(value), 6)


def tail_energy_loss_db(original, cleaned, sr, tail_start_seconds):
    """Energy loss on the sustained vocal tail region."""
    start = int(round(tail_start_seconds * sr))
    before = energy_db(original[start:])
    after = energy_db(cleaned[start:])
    return _round_db(before - after)


def peak_loss_db(original, cleaned, sr, ref_times, window_seconds=0.03):
    """Peak loss around percussion transients."""
    radius = int(round(window_seconds * sr))
    n = original.shape[0]
    peak_before = 0.0
    peak_after = 0.0
    for t in ref_times:
        center = int(round(t * sr))
        lo = max(0, center - radius // 4)
        hi = min(n, center + radius)
        peak_before = max(peak_before, float(np.max(np.abs(original[lo:hi]))))
        peak_after = max(peak_after, float(np.max(np.abs(cleaned[lo:hi]))))
    if peak_before <= 0.0:
        return 0.0
    return round(20.0 * np.log10(peak_after / peak_before), 6)
