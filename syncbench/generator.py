"""Deterministic multichannel sample generator.

Produces up to 8 per-channel 48 kHz 16-bit PCM WAV files with injected
periodic pulse markers. Channel clock behaviour (frequency skew, event
jumps, missing markers, low-SNR stretches) is fully derived from the
seed, so identical seeds and operation sequences reproduce identical
files byte-for-byte. Ground truth is written to truth.json and is the
recorded truth source for every marker.
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, field

import numpy as np
import soundfile as sf

from .wavio import NOMINAL_FS

TRUTH_SOURCE = "generator_truth"


def marker_template(fs=NOMINAL_FS):
    """Deterministic marker pulse: windowed broadband noise burst.

    A broadband burst has a thumbtack autocorrelation, which avoids the
    cycle ambiguity a tonal pulse would cause in cross-correlation delay
    estimation. Generated from a fixed seed so it is identical everywhere.
    """
    n = 128
    rng = np.random.default_rng(20240501)
    pulse = rng.standard_normal(n) * np.hanning(n)
    pulse -= pulse.mean()
    norm = np.linalg.norm(pulse)
    return pulse / norm


@dataclass
class ChannelSpec:
    skew_ppm: float = 0.0            # clock frequency offset
    offset_samples: float = 0.0      # constant initial offset
    jumps: list = field(default_factory=list)   # [{marker_index, step_samples}]
    missing_markers: list = field(default_factory=list)  # marker indices absent
    low_snr: list = field(default_factory=list)  # [{start, end, gain}]


@dataclass
class SampleSpec:
    name: str
    seed: int
    n_channels: int = 4
    duration_s: float = 20.0
    fs: int = NOMINAL_FS
    marker_period_s: float = 1.0
    marker_gain: float = 0.8
    noise_rms: float = 0.02
    channels: list = field(default_factory=list)  # list[ChannelSpec]

    def __post_init__(self):
        if not self.channels:
            self.channels = [ChannelSpec() for _ in range(self.n_channels)]
        if len(self.channels) != self.n_channels:
            raise ValueError("channels must match n_channels")
        if not 1 <= self.n_channels <= 8:
            raise ValueError("n_channels must be within 1..8")


def _channel_markers(spec, ch_spec):
    """True marker arrival samples for one channel."""
    period = spec.marker_period_s * spec.fs
    n_markers = int(spec.duration_s / spec.marker_period_s) - 1
    ratio = 1.0 + ch_spec.skew_ppm * 1e-6
    arrivals = []
    jumps = sorted(ch_spec.jumps, key=lambda j: j["marker_index"])
    for k in range(1, n_markers + 1):
        pos = k * period * ratio + ch_spec.offset_samples
        for jump in jumps:
            if k >= jump["marker_index"]:
                pos += jump["step_samples"]
        arrivals.append((k, pos))
    return arrivals


def generate_sample(out_dir, spec: SampleSpec):
    """Generate one dataset directory with per-channel WAVs and truth.json."""
    os.makedirs(out_dir, exist_ok=True)
    tmpl = marker_template(spec.fs)
    n_total = int(round(spec.duration_s * spec.fs))
    truth = {
        "name": spec.name,
        "seed": spec.seed,
        "fs": spec.fs,
        "n_channels": spec.n_channels,
        "duration_s": spec.duration_s,
        "marker_period_s": spec.marker_period_s,
        "truth_source": TRUTH_SOURCE,
        "channels": [],
    }
    paths = []
    for ch in range(spec.n_channels):
        ch_spec = spec.channels[ch]
        rng = np.random.default_rng(spec.seed * 1000 + ch)
        sig = rng.normal(0.0, spec.noise_rms, n_total)
        arrivals = _channel_markers(spec, ch_spec)
        ch_truth = {
            "channel": ch,
            "skew_ppm": ch_spec.skew_ppm,
            "offset_samples": ch_spec.offset_samples,
            "jumps": ch_spec.jumps,
            "missing_markers": list(ch_spec.missing_markers),
            "low_snr": ch_spec.low_snr,
            "markers": [],
        }
        for k, pos in arrivals:
            if k in ch_spec.missing_markers:
                continue
            gain = spec.marker_gain
            for seg in ch_spec.low_snr:
                if seg["start"] <= k <= seg["end"]:
                    gain *= seg["gain"]
            center = int(round(pos))
            frac = pos - center
            start = center - len(tmpl) // 2
            if start < 0 or start + len(tmpl) + 1 >= n_total:
                continue
            # fractional placement via linear interpolation of the template
            shifted = np.interp(
                np.arange(len(tmpl)) - frac, np.arange(len(tmpl)), tmpl
            )
            sig[start : start + len(tmpl)] += gain * shifted
            ch_truth["markers"].append({"index": k, "sample": pos, "gain": gain})
        peak = np.max(np.abs(sig)) or 1.0
        if peak > 0.999:
            sig = sig / peak * 0.999
        path = os.path.join(out_dir, f"{spec.name}_ch{ch}.wav")
        sf.write(path, sig, spec.fs, subtype="PCM_16")
        paths.append(path)
        truth["channels"].append(ch_truth)
    truth_path = os.path.join(out_dir, "truth.json")
    with open(truth_path, "w", encoding="utf-8") as fh:
        json.dump(truth, fh, indent=2, sort_keys=True)
    return paths, truth_path


def dataset_hash(paths):
    """Deterministic content hash over the dataset files (sorted by name)."""
    digest = hashlib.sha256()
    for path in sorted(paths):
        digest.update(os.path.basename(path).encode())
        with open(path, "rb") as fh:
            digest.update(fh.read())
    return digest.hexdigest()


def standard_suite(data_dir):
    """Generate the standard deterministic sample suite used by tests/baselines."""
    os.makedirs(data_dir, exist_ok=True)
    specs = [
        # pure clock skew on all non-reference channels
        SampleSpec(
            name="skew4",
            seed=11,
            n_channels=4,
            duration_s=24.0,
            channels=[
                ChannelSpec(),
                ChannelSpec(skew_ppm=40.0, offset_samples=3.0),
                ChannelSpec(skew_ppm=-25.0, offset_samples=-2.0),
                ChannelSpec(skew_ppm=60.0, offset_samples=1.5),
            ],
        ),
        # event jump on channel 2
        SampleSpec(
            name="jump4",
            seed=22,
            n_channels=4,
            duration_s=24.0,
            channels=[
                ChannelSpec(),
                ChannelSpec(skew_ppm=10.0),
                ChannelSpec(
                    skew_ppm=10.0,
                    jumps=[{"marker_index": 12, "step_samples": 40.0}],
                ),
                ChannelSpec(),
            ],
        ),
        # missing markers + low-SNR stretch
        SampleSpec(
            name="degraded4",
            seed=33,
            n_channels=4,
            duration_s=24.0,
            channels=[
                ChannelSpec(),
                ChannelSpec(skew_ppm=30.0, missing_markers=[5, 6, 7]),
                ChannelSpec(
                    skew_ppm=-20.0,
                    low_snr=[{"start": 10, "end": 14, "gain": 0.03}],
                ),
                ChannelSpec(),
            ],
        ),
        # unclassifiable drift: alternating random-ish steps (from seed)
        SampleSpec(
            name="unknown4",
            seed=44,
            n_channels=4,
            duration_s=24.0,
            channels=[
                ChannelSpec(),
                ChannelSpec(
                    jumps=[
                        {"marker_index": 4, "step_samples": 15.0},
                        {"marker_index": 8, "step_samples": -25.0},
                        {"marker_index": 12, "step_samples": 20.0},
                        {"marker_index": 16, "step_samples": -18.0},
                        {"marker_index": 20, "step_samples": 22.0},
                    ]
                ),
                ChannelSpec(),
                ChannelSpec(),
            ],
        ),
    ]
    outputs = []
    for spec in specs:
        out_dir = os.path.join(data_dir, spec.name)
        paths, truth_path = generate_sample(out_dir, spec)
        outputs.append((spec, paths, truth_path))
    return outputs
