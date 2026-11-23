"""本地确定性样本生成脚本。

按样本清单字段生成多段立体声/单声道节目素材：每段包含不同响度水平、
突发瞬态与静音间隙；逐段真值（积分响度、真峰值、目标增益）由参考
测量实现计算并写入 manifest，供验收比对。

用法::

    .venv/bin/python samples/generate_samples.py --out samples/data
"""
from __future__ import annotations

import argparse
import json
import os
import sys

import numpy as np

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from loudbench.audio.io import content_hash, read_wav, write_wav  # noqa: E402
from loudbench.core.config import FrameConfig  # noqa: E402
from loudbench.dsp.loudness import measure_loudness  # noqa: E402

#: 样本清单声明的固定节目结构（确定性，无随机种子漂移空间）。
SEGMENT_SPECS = [
    # name, fs, seconds, channels, rms_dbfs, transient_dbfs, silence_share
    ("seg01_loud_stereo", 48000, 4.0, 2, -18.0, -3.0, 0.25),
    ("seg02_quiet_stereo", 48000, 3.5, 2, -30.0, -12.0, 0.30),
    ("seg03_mono_speech", 48000, 3.0, 1, -23.0, -8.0, 0.30),
    ("seg04_transient_stereo", 44100, 3.0, 2, -26.0, -1.5, 0.35),
    ("seg05_mono_gap", 44100, 2.5, 1, -20.0, -6.0, 0.36),
    ("seg06_bed_stereo", 48000, 3.0, 2, -24.0, -10.0, 0.30),
]

TARGET_LUFS = -23.0
RNG_SEED = 20261123


def _tone_bed(n: int, fs: int, rng: np.random.Generator) -> np.ndarray:
    """多正弦 + 少量确定性噪声的节目底床，宽带且周期稳定。"""
    t = np.arange(n) / fs
    bed = (
        0.55 * np.sin(2 * np.pi * 220.0 * t)
        + 0.30 * np.sin(2 * np.pi * 466.16 * t + 0.7)
        + 0.15 * np.sin(2 * np.pi * 880.0 * t + 1.9)
        + 0.10 * rng.standard_normal(n)
    )
    # 幅度归一到 RMS=1 后再按目标 RMS 缩放
    bed /= max(np.sqrt(np.mean(bed ** 2)), 1e-12)
    return bed


def build_segment(fs: int, seconds: float, channels: int,
                  rms_dbfs: float, transient_dbfs: float,
                  silence_share: float, rng: np.random.Generator
                  ) -> np.ndarray:
    n = int(round(seconds * fs))
    bed = _tone_bed(n, fs, rng)
    rms_linear = 10.0 ** (rms_dbfs / 20.0)
    audio = bed * rms_linear

    # 突发瞬态：3 个窄脉冲窗，位置确定性
    peak_linear = 10.0 ** (transient_dbfs / 20.0)
    for pos in (0.25, 0.55, 0.82):
        center = int(n * pos)
        width = max(8, fs // 2000)  # 0.5 ms 量级
        idx = np.arange(max(center - width, 0), min(center + width, n))
        env = np.sin(np.pi * (idx - idx[0]) / max(len(idx) - 1, 1)) ** 2
        audio[idx] += peak_linear * env

    # 静音间隙：确定性位置清零，供门限扣除
    gap_total = int(n * silence_share)
    gap1 = gap_total // 2
    audio[int(n * 0.40):int(n * 0.40) + gap1] = 0.0
    audio[int(n * 0.70):int(n * 0.70) + (gap_total - gap1)] = 0.0

    if channels == 2:
        # 立体声：右通道加确定性相位差与轻微电平差
        shift = fs // 220
        right = np.roll(audio, shift) * 0.92
        x = np.stack([audio, right], axis=1)
    else:
        x = audio
    return x.astype(np.float64)


def generate(out_dir: str) -> dict:
    os.makedirs(out_dir, exist_ok=True)
    rng = np.random.default_rng(RNG_SEED)
    frame_cfg = FrameConfig()
    manifest = {
        "schema": "loudbench-samples/v1",
        "target_lufs": TARGET_LUFS,
        "frame": frame_cfg.__dict__.copy(),
        "segments": [],
    }

    for index, (name, fs, seconds, channels, rms_db, tp_db, gap) in enumerate(
            SEGMENT_SPECS):
        x = build_segment(fs, seconds, channels, rms_db, tp_db, gap, rng)
        path = os.path.join(out_dir, f"{name}.wav")
        write_wav(path, x, fs)
        # 哈希以实际落盘并可被工作台读回的样本为准，端到端一致
        _, x_on_disk = read_wav(path)
        result = measure_loudness(x, fs, frame_cfg)
        target_gain_db = (TARGET_LUFS - result.integrated_lufs
                          if np.isfinite(result.integrated_lufs) else 0.0)
        entry = {
            "index": index,
            "name": name,
            "path": os.path.relpath(path, os.path.dirname(out_dir)),
            "sample_rate": fs,
            "duration_seconds": seconds,
            "channels": channels,
            "declared_rms_dbfs": rms_db,
            "declared_transient_dbfs": tp_db,
            "silence_share": gap,
            "content_hash": content_hash(x_on_disk),
            "truth": {
                "integrated_lufs": result.integrated_lufs,
                "true_peak_db": result.true_peak_db,
                "target_gain_db": target_gain_db,
                "frame_count": result.frame_count,
                "gated_frame_count": result.gated_frame_count,
            },
        }
        manifest["segments"].append(entry)

    manifest_path = os.path.join(os.path.dirname(out_dir), "manifest.json")
    with open(manifest_path, "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2, ensure_ascii=False)
    return manifest


def main() -> None:
    parser = argparse.ArgumentParser(description="生成确定性音频样本")
    parser.add_argument("--out", default="samples/data")
    args = parser.parse_args()
    manifest = generate(args.out)
    print(f"已生成 {len(manifest['segments'])} 段样本，"
          f"清单: {os.path.join(os.path.dirname(args.out), 'manifest.json')}")


if __name__ == "__main__":
    main()
