"""确定性本地样本生成器。

产出多种采样率、单/双声道 WAV 与 JSON 帧时刻描述，以及固定的参数修改
与回退序列。所有样本由确定性公式生成（无随机种子依赖、无网络），并输出
基线哈希供测试与轨迹复现比对。
"""

from __future__ import annotations

import json
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .parameters import TierParams
from .timebase import TICKS_PER_SECOND
from .util import bytes_hash, canonical_json


@dataclass(frozen=True)
class SampleSpec:
    name: str
    sample_rate: int
    channels: int
    duration_s: float
    frame_step_s: float


SAMPLE_SPECS = (
    SampleSpec("mono_16k", 16000, 1, 2.0, 0.25),
    SampleSpec("stereo_441k", 44100, 2, 1.5, 0.2),
    SampleSpec("stereo_48k", 48000, 2, 1.0, 0.1),
)

# 固定的档位参数修改与回退序列（version 1 -> 2 -> 3，再回退到 1 生成 v4）。
TIER_SEQUENCES: dict[str, list[TierParams]] = {
    "tier_low": [
        TierParams(16000, 256, 1000, 500_000),
        TierParams(16000, 300, 1000, 500_000),  # 仅增益变化
        TierParams(24000, 300, 1000, 500_000),  # 重采样变化
    ],
    "tier_high": [
        TierParams(44100, 256, 750, 375_000),
        TierParams(48000, 256, 750, 375_000),  # 仅重采样变化
        TierParams(48000, 256, 1250, 375_000),  # 量化网格变化（与低档不整除）
    ],
}


def _pcm_samples(spec: SampleSpec) -> np.ndarray:
    """确定性正弦 + 线性扫频混合波形，int16。"""
    count = int(spec.sample_rate * spec.duration_s)
    t = np.arange(count, dtype=np.float64) / spec.sample_rate
    wave_a = 0.55 * np.sin(2 * np.pi * 220.0 * t)
    sweep_freq = 180.0 + 320.0 * (t / spec.duration_s)
    phase = 2 * np.pi * np.cumsum(sweep_freq) / spec.sample_rate
    wave_b = 0.35 * np.sin(phase)
    mono = np.clip((wave_a + wave_b) * 20000.0, -32768, 32767)
    if spec.channels == 1:
        audio = mono.reshape(-1, 1)
    else:
        right = np.clip(mono * 0.7 + 1200.0, -32768, 32767)
        audio = np.stack([mono, right], axis=1)
    return np.rint(audio).astype("<i2")


def write_wav(path: Path, spec: SampleSpec, audio: np.ndarray) -> None:
    with wave.open(str(path), "wb") as wf:
        wf.setnchannels(spec.channels)
        wf.setsampwidth(2)
        wf.setframerate(spec.sample_rate)
        wf.writeframes(audio.astype("<i2").tobytes())


def write_frames(path: Path, spec: SampleSpec) -> list[float]:
    times: list[float] = []
    cursor = 0.0
    while cursor < spec.duration_s - 1e-9:
        times.append(round(cursor, 6))
        cursor += spec.frame_step_s
    payload = {
        "frames": [
            {"frame_id": idx, "time_s": value} for idx, value in enumerate(times)
        ]
    }
    path.write_text(json.dumps(payload, indent=2), encoding="utf-8")
    return times


def generate_samples(root: str | Path) -> dict:
    """在 ``root/sources`` 下生成全部样本并返回基线清单。"""
    base = Path(root)
    sources_dir = base / "sources"
    sources_dir.mkdir(parents=True, exist_ok=True)
    baseline: dict = {"samples": {}, "tier_sequences": {}}
    for spec in SAMPLE_SPECS:
        sample_dir = sources_dir / spec.name
        sample_dir.mkdir(parents=True, exist_ok=True)
        audio = _pcm_samples(spec)
        wav_path = sample_dir / "source.wav"
        frames_path = sample_dir / "frames.json"
        write_wav(wav_path, spec, audio)
        frame_times = write_frames(frames_path, spec)
        baseline["samples"][spec.name] = {
            "spec": {
                "sample_rate": spec.sample_rate,
                "channels": spec.channels,
                "duration_s": spec.duration_s,
                "frame_step_s": spec.frame_step_s,
            },
            "wav": str(wav_path),
            "frames": str(frames_path),
            "pcm_sha256": bytes_hash(audio.astype("<i2").tobytes()),
            "sample_count": int(audio.shape[0]),
            "frame_count": len(frame_times),
        }
    for tier, seq in TIER_SEQUENCES.items():
        baseline["tier_sequences"][tier] = [
            params.content_key() for params in seq
        ]
    baseline_path = base / "baseline.json"
    baseline_path.write_bytes(canonical_json(baseline))
    baseline["_baseline_path"] = str(baseline_path)
    return baseline
