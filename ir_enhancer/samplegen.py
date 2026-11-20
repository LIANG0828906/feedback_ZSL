"""确定性样本生成器：已知噪声底与直达声位置的多声道脉冲响应。"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from . import wavio
from .config import LAYOUT_51, SAMPLE_RATE


def generate_ir(
    seed: int = 1,
    channels: int = 6,
    seconds: float = 1.0,
    onset: int = 4800,
    direct_amp: float = 0.6,
    base_noise_rms: float = 0.004,
    rate: int = SAMPLE_RATE,
) -> tuple:
    """生成含噪脉冲响应；返回 (数据 [n, ch], 真值信息 dict)。"""
    rng = np.random.default_rng(seed)
    n = int(seconds * rate)
    data = np.zeros((n, channels), dtype=np.float64)
    noise_rms = np.zeros(channels)
    t = np.arange(n - onset) / rate
    for ch in range(channels):
        rms = base_noise_rms * (1.0 + 0.15 * ch)
        noise_rms[ch] = rms
        noise = rng.standard_normal(n) * rms
        tail = (
            rng.standard_normal(n - onset)
            * np.exp(-t / 0.08)
            * 0.08
            * (1.0 + 0.1 * ch)
        )
        signal = np.zeros(n)
        signal[onset] = direct_amp * (1.0 - 0.05 * ch)
        signal[onset:] += tail
        for delay, gain in ((1771, 0.25), (3542, 0.12)):
            pos = onset + delay
            if pos < n:
                signal[pos] += gain * direct_amp
        data[:, ch] = signal + noise
    truth = {
        "seed": seed,
        "channels": channels,
        "sample_rate": rate,
        "onset": onset,
        "direct_amp": direct_amp,
        "noise_rms": noise_rms.tolist(),
        "layout": list(LAYOUT_51) if channels == 6 else ["CH%d" % i for i in range(channels)],
    }
    return data, truth


def generate_sample(seed: int, out_dir, channels: int = 6, seconds: float = 1.0) -> dict:
    """落盘样本 WAV 与真值 JSON，返回路径信息。"""
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    data, truth = generate_ir(seed=seed, channels=channels, seconds=seconds)
    wav_path = out / f"sample_seed{seed}.wav"
    wavio.write_wav(wav_path, data, rate=truth["sample_rate"])
    truth_path = out / f"sample_seed{seed}.truth.json"
    with open(truth_path, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(truth, fh, sort_keys=True, ensure_ascii=False, indent=1)
    return {"wav": str(wav_path), "truth": str(truth_path), "meta": truth}
