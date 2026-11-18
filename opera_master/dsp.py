"""Pure-NumPy DSP primitives used by loudness metering and limiting."""

from __future__ import annotations

from collections import deque

import numpy as np


def biquad_apply(x, b, a):
    """Apply cascaded biquad sections via exact frequency-domain filtering.

    K-weighting is LTI, so the cascaded transfer function
    ``prod(B_i)/prod(A_i)`` is evaluated once on the FFT grid.  This is
    numerically equivalent (to FFT precision) to a time-domain biquad chain
    while avoiding a per-sample Python loop on long masters.
    """
    b = np.asarray(b, dtype=np.float64)
    a = np.asarray(a, dtype=np.float64)
    if b.ndim == 1:
        b = b[None, :]
        a = a[None, :]
    n = np.asarray(x, dtype=np.float64).shape[0]
    n_fft = 1
    while n_fft < n * 2:
        n_fft <<= 1
    num = np.ones(n_fft // 2 + 1, dtype=np.complex128)
    den = np.ones_like(num)
    freqs = np.fft.rfftfreq(n_fft)
    z = np.exp(-2j * np.pi * freqs)
    for sec in range(b.shape[0]):
        bb, aa = b[sec] / a[sec, 0], a[sec] / a[sec, 0]
        num *= bb[0] + bb[1] * z + bb[2] * z**2
        den *= 1.0 + aa[1] * z + aa[2] * z**2
    spectrum = np.fft.rfft(x, n_fft)
    filtered = np.fft.irfft(spectrum * num / den, n_fft)
    return filtered[:n].astype(np.float64, copy=False)


def k_weighting_coeffs(sr):
    """ITU-R BS.1770-4 stage 1 shelf + stage 2 high-pass (48 kHz family)."""
    # Coefficients are documented for 48 kHz; the test material is fixed there.
    if sr != 48000:
        raise ValueError(f"K-weighting coefficients unavailable at {sr} Hz")
    shelf_b = [
        1.53512485958697,
        -2.69169618940638,
        1.19839281085285,
    ]
    shelf_a = [
        1.0,
        -1.69065929318241,
        0.73248077421585,
    ]
    hp_b = [
        1.0,
        -2.0,
        1.0,
    ]
    hp_a = [
        1.0,
        -1.99004745483398,
        0.99007225036621,
    ]
    return (np.array([shelf_b, hp_b]), np.array([shelf_a, hp_a]))


def fft_convolve(x, kernel):
    """Length-preserving causal FFT convolution (kernel assumed zero-phase
    centred when used for oversampling; callers pre-shift as needed)."""
    n_out = x.shape[0] + kernel.shape[0] - 1
    n_fft = 1
    while n_fft < n_out:
        n_fft <<= 1
    spec = np.fft.rfft(x, n_fft) * np.fft.rfft(kernel, n_fft)
    full = np.fft.irfft(spec, n_fft)
    return full[:n_out]


def _kaiser_beta(atten_db):
    if atten_db > 50.0:
        return 0.1102 * (atten_db - 8.7)
    if atten_db >= 21.0:
        return 0.5842 * (atten_db - 21.0) ** 0.4 + 0.07886 * (atten_db - 21.0)
    return 0.0


def _sinc(x):
    out = np.ones_like(x)
    nz = x != 0.0
    out[nz] = np.sin(np.pi * x[nz]) / (np.pi * x[nz])
    return out


def design_oversample_filter(factor=4, taps=129, atten_db=80.0):
    """Low-pass FIR for polyphase-free upsample: cutoff = fs/(2*factor)."""
    half = (taps - 1) // 2
    n = np.arange(-half, half + 1, dtype=np.float64)
    cutoff = 1.0 / (2.0 * factor)
    window = np.kaiser(taps, _kaiser_beta(atten_db))
    kernel = 2.0 * cutoff * _sinc(2.0 * cutoff * n) * window
    kernel /= kernel.sum()
    return kernel


def upsample_4x(x, kernel=None):
    """Zero-stuff + FIR low-pass, returned at 4x sample rate."""
    if kernel is None:
        kernel = design_oversample_filter()
    up = np.zeros(x.shape[0] * 4, dtype=np.float64)
    up[::4] = x
    # Shift the (odd-tap) kernel so convolution delay lands at half taps*4.
    shifted = np.zeros(kernel.shape[0] * 4)
    shifted[::4] = kernel
    convolved = fft_convolve(up, shifted)
    delay = (kernel.shape[0] // 2) * 4
    return convolved[delay : delay + up.shape[0]]


def true_peak_db(signal, kernel=None):
    """BS.1770 style true peak via 4x oversampled absolute maximum."""
    if signal.ndim == 1:
        signal = signal[:, None]
    peak = 0.0
    for ch in range(signal.shape[1]):
        over = upsample_4x(signal[:, ch], kernel)
        peak = max(peak, float(np.max(np.abs(over))))
    if peak <= 0.0:
        return -np.inf
    return 20.0 * np.log10(peak)


def _future_max(x, lookahead):
    """For each i, the max of x[i : i+lookahead+1] (causal look-ahead)."""
    n = x.shape[0]
    out = np.empty(n)
    dq = deque()
    # Pre-load the first window.
    for j in range(min(lookahead + 1, n)):
        while dq and x[dq[-1]] <= x[j]:
            dq.pop()
        dq.append(j)
    for i in range(n):
        while dq and dq[0] < i:
            dq.popleft()
        nxt = i + lookahead + 1
        if nxt < n:
            while dq and x[dq[-1]] <= x[nxt]:
                dq.pop()
            dq.append(nxt)
        out[i] = x[dq[0]]
    return out


def lookahead_limiter(signal, sr, ceiling_db, lookahead_ms=5.0,
                      release_ms=100.0):
    """Delay-compensated look-ahead brick-wall limiter.

    Returns ``(limited, metadata)`` with identical sample count: the gain
    envelope is advanced by the look-ahead so the limiter's own delay is
    compensated entirely inside the array.
    """
    if signal.ndim == 1:
        signal = signal[:, None]
    n, channels = signal.shape
    ceiling = 10.0 ** (ceiling_db / 20.0)
    lookahead = max(int(round(lookahead_ms * sr / 1000.0)), 1)
    release_coef = float(np.exp(-1.0 / max(release_ms * sr / 1000.0, 1.0)))

    magnitude = np.max(np.abs(signal), axis=1)
    future_peak = _future_max(magnitude, lookahead)
    target_gain = np.ones(n)
    hot = future_peak > ceiling
    target_gain[hot] = ceiling / future_peak[hot]

    env = np.ones(n)
    g = 1.0
    for i in range(n):
        t = target_gain[i]
        if t < g:
            g = t  # instant attack
        else:
            g = release_coef * g + (1.0 - release_coef) * t
        env[i] = g

    # Delay compensation: the gain computed for future sample i+lookahead is
    # applied now, so no net latency is introduced and the length is fixed.
    applied_env = np.ones(n)
    applied_env[: n - lookahead] = env[lookahead:]
    applied_env[n - lookahead :] = env[-1]
    limited = signal * applied_env[:, None]
    # "Triggered" means a musically meaningful gain reduction (>0.001 dB),
    # not the tiny numerical tail of a release smoothing back to unity.
    triggered = int(np.count_nonzero(applied_env < 10.0 ** (-0.001 / 20.0)))
    max_atten_db = float(-20.0 * np.log10(np.min(applied_env))) if triggered else 0.0
    metadata = {
        "lookahead_samples": lookahead,
        "ceiling_linear": ceiling,
        "triggered_samples": triggered,
        "max_attenuation_db": max_atten_db,
    }
    return limited, metadata
